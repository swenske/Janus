package fleet

import (
	"bytes"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age/armor"

	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/internal/pki"
)

func newStore(t *testing.T, dataDir string) *Store {
	t.Helper()
	key, err := secrets.LoadOrCreate("", dataDir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dataDir, "fleet"), key, "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSetupConfirm: nothing reaches the nodes until the operator gives
// back the kit and its passphrase - then the root's key is gone from the
// Controller, and what it keeps is enough for the nodes to let it in.
func TestSetupConfirm(t *testing.T) {
	dataDir := t.TempDir()
	s := newStore(t, dataDir)
	if st := s.Status(); st.State != StateNone || !st.MasterKeyBeside {
		t.Fatalf("fresh: %+v", st)
	}
	if _, err := s.RecoveryKit(); !errors.Is(err, ErrState) {
		t.Errorf("a kit before setup: %v", err)
	}

	pass, err := s.Setup()
	if err != nil {
		t.Fatal(err)
	}
	if len(pass) != 29 || strings.Count(pass, "-") != 5 {
		t.Errorf("passphrase %q", pass)
	}
	if _, _, _, ok := s.Trust(); ok {
		t.Error("the nodes get the fleet before the kit is confirmed")
	}
	if _, err := s.ClientCertificate(); err == nil {
		t.Error("a Controller certificate before the kit is confirmed")
	}
	kit, err := s.RecoveryKit()
	if err != nil || !bytes.HasPrefix(kit, []byte(armor.Header)) {
		t.Fatalf("kit: %v, %.40q", err, kit)
	}
	content, err := OpenKit(kit, strings.ToLower(pass))
	if err != nil || !strings.Contains(content.RootKey, "PRIVATE KEY") {
		t.Fatalf("the kit with its passphrase, lowercased: %v", err)
	}

	other := newStore(t, t.TempDir())
	otherPass, err := other.Setup()
	if err != nil {
		t.Fatal(err)
	}
	otherKit, _ := other.RecoveryKit()
	for name, c := range map[string]struct {
		kit  []byte
		pass string
	}{
		"a wrong passphrase":  {kit, otherPass},
		"another fleet's kit": {otherKit, otherPass},
		"not a kit":           {[]byte("hello"), pass},
		"an empty passphrase": {kit, ""},
	} {
		if err := s.Confirm(c.kit, c.pass); !errors.Is(err, ErrKit) {
			t.Errorf("%s: %v", name, err)
		}
	}

	if err := s.Confirm(kit, pass); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{rootKeyFile, kitFile} {
		if _, err := os.Stat(filepath.Join(dataDir, "fleet", name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still there after confirmation: %v", name, err)
		}
	}
	if _, err := s.RecoveryKit(); !errors.Is(err, ErrState) {
		t.Errorf("a kit after confirmation: %v", err)
	}
	if _, err := s.Setup(); !errors.Is(err, ErrState) {
		t.Errorf("setup again once ready: %v", err)
	}

	// A node that trusts this fleet lets the Controller in.
	rootPEM, bundle, version, ok := s.Trust()
	if !ok || version != 1 {
		t.Fatalf("trust: %v %d", ok, version)
	}
	node, err := pki.OpenFleet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Set(rootPEM, bundle); err != nil {
		t.Fatal(err)
	}
	accepts := func(s *Store) {
		t.Helper()
		id, err := s.ClientCertificate()
		if err != nil {
			t.Fatal(err)
		}
		if id.Leaf.Subject.CommonName != IdentityName || len(id.Leaf.Subject.Organization) != 1 || id.Leaf.Subject.Organization[0] != pki.RoleController {
			t.Errorf("identity: %v", id.Leaf.Subject)
		}
		roots, inter := x509.NewCertPool(), x509.NewCertPool()
		roots.AddCert(node.Root())
		issuing, _ := x509.ParseCertificate(id.Certificate[1])
		inter.AddCert(issuing)
		chains, err := id.Leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		if err != nil {
			t.Fatal(err)
		}
		local, _ := pki.NewCA("a node CA")
		if err := node.AcceptChains(chains, local.Cert); err != nil {
			t.Errorf("a node of the fleet refuses the Controller: %v", err)
		}
	}
	accepts(s)
	first, _ := s.ClientCertificate()
	if again, _ := s.ClientCertificate(); again != first {
		t.Error("the Controller's certificate isn't reused while fresh")
	}

	// Reopened, the Controller still is - with its master key only.
	reopened := newStore(t, dataDir)
	if st := reopened.Status(); st.State != StateReady || st.RootFingerprint != s.Status().RootFingerprint {
		t.Errorf("reopened: %+v", st)
	}
	accepts(reopened)
	wrongKey, _ := secrets.LoadOrCreate(filepath.Join(t.TempDir(), "other.key"), dataDir)
	if _, err := Open(filepath.Join(dataDir, "fleet"), wrongKey, "x"); err == nil {
		t.Error("opened with another master key")
	}
}

// TestSetupAgain: while the kit waits, setting up again starts over - a
// kit lost before it was confirmed.
func TestSetupAgain(t *testing.T) {
	s := newStore(t, t.TempDir())
	first, err := s.Setup()
	if err != nil {
		t.Fatal(err)
	}
	firstKit, _ := s.RecoveryKit()
	root := s.Status().RootFingerprint
	second, err := s.Setup()
	if err != nil || second == first || s.Status().RootFingerprint == root {
		t.Fatalf("again: %v, same passphrase %v, same root %v", err, second == first, s.Status().RootFingerprint == root)
	}
	if err := s.Confirm(firstKit, first); !errors.Is(err, ErrKit) {
		t.Errorf("the first kit after starting over: %v", err)
	}
}
