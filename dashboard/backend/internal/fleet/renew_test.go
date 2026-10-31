package fleet

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/internal/pki"
)

// readyStore is a fleet set up and confirmed in dataDir, with its kit and
// passphrase.
func readyStore(t *testing.T, dataDir string) (*Store, []byte, string) {
	t.Helper()
	s := newStore(t, dataDir)
	pass, err := s.Setup()
	if err != nil {
		t.Fatal(err)
	}
	kit, _ := s.RecoveryKit()
	if err := s.Confirm(kit, pass); err != nil {
		t.Fatal(err)
	}
	return s, kit, pass
}

// fleetNode is a node trusting s's fleet at its current bundle.
func fleetNode(t *testing.T, s *Store) *pki.Fleet {
	t.Helper()
	node, err := pki.OpenFleet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rootPEM, bundle, _, _ := s.Trust()
	if err := node.Set(rootPEM, bundle); err != nil {
		t.Fatal(err)
	}
	return node
}

// lets reports whether node lets cert in - as its TLS handshake and
// AcceptChains decide.
func lets(node *pki.Fleet, cert *tls.Certificate) error {
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(node.Root())
	for _, der := range cert.Certificate[1:] {
		c, _ := x509.ParseCertificate(der)
		inter.AddCert(c)
	}
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		return err
	}
	local, _ := pki.NewCA("a node CA")
	return node.AcceptChains(chains, local.Cert)
}

func issuerOf(t *testing.T, cert *tls.Certificate) string {
	t.Helper()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Issuer.CommonName
}

func userCert(t *testing.T, s *Store) (*tls.Certificate, error) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	chain, _, err := s.IssueUser(&key.PublicKey, "sam", pki.RoleOperator, nil, time.Hour)
	if err != nil {
		return nil, err
	}
	var c tls.Certificate
	for rest := chain; ; {
		b, r := pemDecode(rest)
		if b == nil {
			break
		}
		c.Certificate = append(c.Certificate, b)
		rest = r
	}
	return &c, nil
}

// TestRenew: the kit makes a new issuing CA and a bundle listing it and
// the one before; a node still on the old bundle is reached with the old
// CA's certificate, one on the new with the new's; janusctl's
// certificates switch once every node has the bundle; an older release
// reading issuing.* signs with the CA janusctl's come from.
func TestRenew(t *testing.T) {
	dataDir := t.TempDir()
	s, kit, pass := readyStore(t, dataDir)
	oldName := s.Status().IssuingName
	behind := fleetNode(t, s) // stays on bundle 1
	ahead := fleetNode(t, s)

	other, otherKit, otherPass := readyStore(t, t.TempDir())
	_ = other
	for name, c := range map[string]struct {
		kit  []byte
		pass string
	}{"a wrong passphrase": {kit, otherPass}, "another fleet's kit": {otherKit, otherPass}} {
		if err := s.Renew(c.kit, c.pass, 0); !errors.Is(err, ErrKit) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if st := s.Status(); st.Renewal != nil || st.BundleVersion != 1 {
		t.Fatalf("a refused renewal changed the fleet: %+v", st)
	}

	if err := s.Renew(kit, pass, 42); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.Renewal == nil || st.Renewal.Version < uint64(time.Now().Add(-time.Minute).UnixMilli()) || st.BundleVersion != st.Renewal.Version || !st.Renewal.Switched.IsZero() || st.Renewal.PreviousName != oldName || st.Renewal.RescueUntil.IsZero() {
		t.Fatalf("after the renewal: %+v %+v", st, st.Renewal)
	}
	if !strings.HasPrefix(st.IssuingName, "Janus fleet issuing CA 20") || st.IssuingName == oldName {
		t.Errorf("the new CA's name: %q", st.IssuingName)
	}
	if err := s.Renew(kit, pass, 0); !errors.Is(err, ErrRenewing) {
		t.Errorf("again before the switch: %v", err)
	}
	rootPEM, bundle, version, _ := s.Trust()
	b, cas, err := pki.VerifyBundle(behind.Root(), bundle)
	if err != nil || b.Version != version || len(cas) != 2 {
		t.Fatalf("the bundle: %v, %d CAs", err, len(cas))
	}
	if err := ahead.Set(rootPEM, bundle); err != nil {
		t.Fatalf("a node takes the renewal's bundle: %v", err)
	}

	// Each node is reached with a certificate it lets in.
	old, _ := s.ClientCertificateFor(1)
	next, _ := s.ClientCertificateFor(version)
	if issuerOf(t, old) != oldName || issuerOf(t, next) != st.IssuingName {
		t.Fatalf("issuers: %s, %s", issuerOf(t, old), issuerOf(t, next))
	}
	if err := lets(behind, old); err != nil {
		t.Errorf("the node on bundle 1, the old CA's certificate: %v", err)
	}
	if lets(behind, next) == nil {
		t.Error("the node on bundle 1 lets the new CA's certificate in")
	}
	for _, c := range []*tls.Certificate{old, next} {
		if err := lets(ahead, c); err != nil {
			t.Errorf("the node on the new bundle: %v", err)
		}
	}
	// The registration certificate is the new CA's at once: nodes check
	// it against the root.
	if srv, _ := s.ServerCertificate(); issuerOf(t, srv) != st.IssuingName {
		t.Error("the registration certificate isn't the new CA's")
	}

	// janusctl: the old CA's until the switch.
	if u, _ := userCert(t, s); issuerOf(t, u) != oldName || lets(behind, u) != nil {
		t.Error("before the switch, an account's certificate isn't the old CA's")
	}
	issuingPEM := func() []byte { b, _ := os.ReadFile(filepath.Join(dataDir, "fleet", "issuing.crt")); return b }
	if c, _ := parseCert(issuingPEM()); c.Subject.CommonName != oldName {
		t.Error("before the switch, issuing.crt isn't the CA janusctl's certificates come from")
	}
	if switched, err := s.Switch(false); err != nil || switched {
		t.Fatalf("switched with a node behind: %v %v", switched, err)
	}
	if switched, err := s.Switch(true); err != nil || !switched {
		t.Fatalf("not switched with every node on the bundle: %v %v", switched, err)
	}
	if u, _ := userCert(t, s); issuerOf(t, u) != st.IssuingName || lets(ahead, u) != nil {
		t.Error("after the switch, an account's certificate isn't the new CA's")
	}
	if c, _ := parseCert(issuingPEM()); c.Subject.CommonName != st.IssuingName {
		t.Error("after the switch, issuing.crt isn't the new CA")
	}
	// What an older release reads: issuing.* opens with its key.
	key, _ := secrets.LoadOrCreate("", dataDir)
	sealed, _ := os.ReadFile(filepath.Join(dataDir, "fleet", "issuing.key.sealed"))
	keyPEM, err := key.Open(sealed, "fleet/issuing.key.sealed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pki.LoadCA(issuingPEM(), keyPEM); err != nil {
		t.Errorf("an older release can't load issuing.*: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dataDir, "fleet", "bundle.json")); !bytes.Equal(got, bundle) {
		t.Error("bundle.json isn't the renewal's bundle")
	}
	matches, _ := filepath.Glob(filepath.Join(dataDir, "fleet", "issuing-*"))
	if len(matches) != 0 {
		t.Errorf("left after the switch: %v", matches)
	}

	// Reopened: the same, the node behind still reached.
	again := newStore(t, dataDir)
	if st2 := again.Status(); st2.IssuingName != st.IssuingName || st2.Renewal == nil || st2.Renewal.Switched.IsZero() {
		t.Fatalf("reopened: %+v", st2)
	}
	if c, _ := again.ClientCertificateFor(1); lets(behind, c) != nil {
		t.Error("reopened, the node behind isn't reached")
	}

	// The next renewal drops the CA before.
	if err := again.Renew(kit, pass, 0); err != nil {
		t.Fatal(err)
	}
	_, bundle3, _, _ := again.Trust()
	if _, cas, _ := pki.VerifyBundle(behind.Root(), bundle3); len(cas) != 2 || cas[0].Subject.CommonName != st.IssuingName {
		t.Errorf("the next renewal's bundle: %d CAs", len(cas))
	}
}

// TestRenewRollForward: a stop between a renewal's or a switch's writes
// is finished at the next start.
func TestRenewRollForward(t *testing.T) {
	dataDir := t.TempDir()
	s, kit, pass := readyStore(t, dataDir)
	if err := s.Renew(kit, pass, 0); err != nil {
		t.Fatal(err)
	}
	_, bundle, _, _ := s.Trust()
	// Cut before bundle.json was copied.
	if err := os.WriteFile(filepath.Join(dataDir, "fleet", "bundle.json"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := newStore(t, dataDir)
	if got, _ := os.ReadFile(filepath.Join(dataDir, "fleet", "bundle.json")); !bytes.Equal(got, bundle) {
		t.Error("bundle.json not written again")
	}
	// Cut after the switch's state, before issuing.* was rewritten.
	s2.mu.Lock()
	st := s2.st
	st.Switched = time.Now()
	st.Previous = "issuing"
	_ = s2.writeState(st)
	s2.mu.Unlock()
	s3 := newStore(t, dataDir)
	if s3.st.Newest != "" {
		t.Fatalf("the switch isn't finished: %+v", s3.st)
	}
	c, _ := parseCert(mustRead(t, filepath.Join(dataDir, "fleet", "issuing.crt")))
	if c.Subject.CommonName != s3.Status().IssuingName {
		t.Error("issuing.crt isn't the new CA after the switch was finished")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSwitchBefore: 30 days before the CA before ends, janusctl's
// certificates switch whatever the nodes.
func TestSwitchBefore(t *testing.T) {
	prev := switchBefore
	switchBefore = 3 * 365 * 24 * time.Hour // more than the CA lasts
	t.Cleanup(func() { switchBefore = prev })
	s, kit, pass := readyStore(t, t.TempDir())
	if err := s.Renew(kit, pass, 0); err != nil {
		t.Fatal(err)
	}
	if s.Status().Renewal.SwitchBy.IsZero() {
		t.Error("no date for the switch")
	}
	if switched, err := s.Switch(false); err != nil || !switched {
		t.Errorf("not switched near the previous CA's end: %v %v", switched, err)
	}
}

// TestExpiredAndRescue: an issuing CA past its end signs nothing; the kit
// gives a root-signed certificate an hour, which nodes let in, and a
// renewal makes a bundle without the expired CA.
func TestExpiredAndRescue(t *testing.T) {
	prev := issuingValidity
	issuingValidity = 3 * time.Second
	s, kit, pass := readyStore(t, t.TempDir())
	issuingValidity = prev
	node := fleetNode(t, s)
	time.Sleep(time.Until(s.Status().IssuingNotAfter) + 100*time.Millisecond)

	if _, err := s.ClientCertificateFor(1); !errors.Is(err, ErrExpired) {
		t.Errorf("a certificate from an expired CA: %v", err)
	}
	if _, err := userCert(t, s); !errors.Is(err, ErrExpired) {
		t.Errorf("an account's certificate from an expired CA: %v", err)
	}
	if err := s.Rescue(kit, "0000-0000-0000-0000-0000-0000"); !errors.Is(err, ErrKit) {
		t.Errorf("a rescue with a wrong passphrase: %v", err)
	}
	if err := s.Rescue(kit, pass); err != nil {
		t.Fatal(err)
	}
	rescue, err := s.ClientCertificateFor(1)
	if err != nil {
		t.Fatal(err)
	}
	if issuerOf(t, rescue) != "Janus fleet root" || lets(node, rescue) != nil {
		t.Errorf("the rescue certificate: issuer %s, %v", issuerOf(t, rescue), lets(node, rescue))
	}
	if left := s.Status().Renewal.RescueUntil; time.Until(left) > time.Hour || time.Until(left) < 59*time.Minute {
		t.Errorf("rescue until %v", left)
	}

	if err := s.Renew(kit, pass, 0); err != nil {
		t.Fatal(err)
	}
	rootPEM, bundle, version, _ := s.Trust()
	if _, cas, err := pki.VerifyBundle(node.Root(), bundle); err != nil || len(cas) != 1 {
		t.Errorf("a renewal after the CA expired lists %d CAs: %v", len(cas), err)
	}
	// The node behind is reached with the rescue certificate - the old
	// CA is no use -, takes the bundle, then lets the new CA's in.
	if c, _ := s.ClientCertificateFor(1); issuerOf(t, c) != "Janus fleet root" {
		t.Errorf("the node behind: %s", issuerOf(t, c))
	}
	if err := node.Set(rootPEM, bundle); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.ClientCertificateFor(version); lets(node, c) != nil {
		t.Error("the new CA's certificate after the bundle")
	}
	if switched, _ := s.Switch(false); !switched {
		t.Error("not switched with the CA before expired")
	}
}

// pemDecode is the next PEM block's bytes and the rest - nil at the end.
func pemDecode(in []byte) ([]byte, []byte) {
	b, rest := pem.Decode(in)
	if b == nil {
		return nil, nil
	}
	return b.Bytes, rest
}
