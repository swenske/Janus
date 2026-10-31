package identity

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/internal/pki"
)

func masterKey(t *testing.T, dir string) *secrets.Key {
	t.Helper()
	key, err := secrets.LoadOrCreate("", dir)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func leafOf(t *testing.T, c *tls.Certificate) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func hasIP(leaf *x509.Certificate, s string) bool {
	return slices.ContainsFunc(leaf.IPAddresses, net.ParseIP(s).Equal)
}

// verifies reports whether a client given trustPEM - as a node is, with
// AppendCertsFromPEM - takes cert for name.
func verifies(trustPEM []byte, cert *tls.Certificate, name string) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trustPEM) {
		return errors.New("no certificate in the trust")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	inter := x509.NewCertPool()
	for _, der := range cert.Certificate[1:] {
		c, _ := x509.ParseCertificate(der)
		inter.AddCert(c)
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, DNSName: name})
	return err
}

// TestNewIdentity: a new Controller has its CA and a certificate it
// signs, for its addresses and -advertise-address's, served with the CA;
// the same comes back at the next start.
func TestNewIdentity(t *testing.T) {
	dir := t.TempDir()
	key := masterKey(t, dir)
	id, err := Open(dir, key, "controller.example.com, 203.0.113.5")
	if err != nil {
		t.Fatal(err)
	}
	cert := id.Certificate()
	leaf := leafOf(t, cert)
	if !slices.Contains(leaf.DNSNames, "controller.example.com") || !slices.Contains(leaf.DNSNames, "localhost") || !hasIP(leaf, "203.0.113.5") || !hasIP(leaf, "127.0.0.1") {
		t.Errorf("names: %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if len(cert.Certificate) != 2 || leaf.CheckSignatureFrom(id.CA()) != nil {
		t.Errorf("served: %d certificates, signed by the CA: %v", len(cert.Certificate), leaf.CheckSignatureFrom(id.CA()))
	}
	if id.Legacy() || !id.SwitchAt().IsZero() || !bytes.Equal(id.TrustPEM(), id.CAPEM()) {
		t.Errorf("a new identity: legacy %v, switch %v, trust %q", id.Legacy(), id.SwitchAt(), id.TrustPEM())
	}
	if left := time.Until(leaf.NotAfter); left < 360*24*time.Hour || left > 366*24*time.Hour {
		t.Errorf("valid %v", left)
	}
	if err := verifies(id.TrustPEM(), cert, "controller.example.com"); err != nil {
		t.Errorf("a node given the trust: %v", err)
	}
	for _, f := range []string{CAKeyFile, KeyFile} {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v", f, err)
		}
	}
	sealed, _ := os.ReadFile(filepath.Join(dir, CAKeyFile))
	if bytes.Contains(sealed, []byte("PRIVATE KEY")) {
		t.Error("the CA's key isn't sealed")
	}
	// The file every release loads.
	if _, err := tls.LoadX509KeyPair(filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile)); err != nil {
		t.Errorf("the certificate's files: %v", err)
	}

	again, err := Open(dir, key, "controller.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Certificate().Certificate[0], cert.Certificate[0]) || !again.CA().Equal(id.CA()) {
		t.Error("not the same identity at the next start")
	}
	other, _ := secrets.LoadOrCreate(filepath.Join(t.TempDir(), "other.key"), dir)
	if _, err := Open(dir, other, ""); err == nil {
		t.Error("opened with another master key")
	}
}

// TestAdvertiseAddress: an address added to -advertise-address gets a
// new certificate at the next start - still naming what the one before
// did, still signed by the same CA.
func TestAdvertiseAddress(t *testing.T) {
	dir := t.TempDir()
	key := masterKey(t, dir)
	id, err := Open(dir, key, "old.example.com")
	if err != nil {
		t.Fatal(err)
	}
	first := id.Certificate()
	id2, err := Open(dir, key, "new.example.com,198.51.100.7")
	if err != nil {
		t.Fatal(err)
	}
	leaf := leafOf(t, id2.Certificate())
	if bytes.Equal(id2.Certificate().Certificate[0], first.Certificate[0]) {
		t.Fatal("not renewed for a new -advertise-address")
	}
	if !slices.Contains(leaf.DNSNames, "old.example.com") || !slices.Contains(leaf.DNSNames, "new.example.com") || !hasIP(leaf, "198.51.100.7") {
		t.Errorf("names: %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if !id2.CA().Equal(id.CA()) || verifies(id.TrustPEM(), id2.Certificate(), "old.example.com") != nil {
		t.Error("what trusted the identity before doesn't take the new certificate")
	}
	if id2.PreviousFingerprint(time.Minute) != pki.Fingerprint(first.Certificate[0]) {
		t.Error("the certificate replaced isn't remembered")
	}
	id3, _ := Open(dir, key, "new.example.com")
	if !bytes.Equal(id3.Certificate().Certificate[0], id2.Certificate().Certificate[0]) {
		t.Error("renewed again though nothing changed")
	}
}

// TestRenewal: 35 days before its end the certificate is renewed - by
// the same CA, the same names.
func TestRenewal(t *testing.T) {
	dir := t.TempDir()
	id, err := Open(dir, masterKey(t, dir), "controller.example.com")
	if err != nil {
		t.Fatal(err)
	}
	first := id.Certificate()
	if renewed, err := id.Refresh(); err != nil || renewed {
		t.Fatalf("a fresh certificate renewed: %v %v", renewed, err)
	}
	id.now = func() time.Time { return leafOf(t, first).NotAfter.Add(-RenewBefore - time.Hour) }
	if renewed, _ := id.Refresh(); renewed {
		t.Error("renewed more than 35 days before the end")
	}
	at := leafOf(t, first).NotAfter.Add(-RenewBefore + time.Hour)
	id.now = func() time.Time { return at }
	renewed, err := id.Refresh()
	if err != nil || !renewed {
		t.Fatalf("not renewed 35 days before the end: %v %v", renewed, err)
	}
	if bytes.Equal(id.Certificate().Certificate[0], first.Certificate[0]) || verifies(id.TrustPEM(), id.Certificate(), "controller.example.com") != nil {
		t.Error("the renewed certificate isn't one the CA signed for the same names")
	}
	if got := id.PreviousFingerprint(time.Minute); got != pki.Fingerprint(first.Certificate[0]) {
		t.Errorf("previous %q", got)
	}
	id.now = func() time.Time { return at.Add(time.Hour) }
	if id.PreviousFingerprint(10*time.Minute) != "" {
		t.Error("the previous certificate still good long after")
	}
}

// legacyIdentity writes a certificate as Controllers made before the CA
// was kept: signed by a CA thrown away, valid for validity.
func legacyIdentity(t *testing.T, dir string, validity time.Duration) *x509.Certificate {
	t.Helper()
	ca, err := pki.NewCA("dashboard")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{
		CommonName:  "dashboard",
		DNSNames:    []string{"localhost", "legacy.example.com"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.0.2.80")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		Validity:    validity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CertFile), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, KeyFile), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(certPEM)
	leaf, _ := x509.ParseCertificate(b.Bytes)
	return leaf
}

// TestLegacyKept: a Controller made before keeps serving its certificate
// - everything it provisioned pins it - while what it provisions now
// gets the CA as well.
func TestLegacyKept(t *testing.T) {
	dir := t.TempDir()
	legacy := legacyIdentity(t, dir, 200*24*time.Hour)
	id, err := Open(dir, masterKey(t, dir), "")
	if err != nil {
		t.Fatal(err)
	}
	cert := id.Certificate()
	if !bytes.Equal(cert.Certificate[0], legacy.Raw) || len(cert.Certificate) != 1 || !id.Legacy() {
		t.Fatal("the legacy certificate isn't served as it is")
	}
	if want := legacy.NotAfter.Add(-RenewBefore); !id.SwitchAt().Equal(want) {
		t.Errorf("switch at %v, want %v", id.SwitchAt(), want)
	}
	trust := id.TrustPEM()
	if err := verifies(trust, cert, "legacy.example.com"); err != nil {
		t.Errorf("the trust given now doesn't take the legacy certificate: %v", err)
	}
	if !id.HoldsCA(trust) || id.HoldsCA(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: legacy.Raw})) {
		t.Error("HoldsCA")
	}

	// At the switch, what was given the trust takes the CA's certificate;
	// what was given only the legacy one doesn't.
	id.now = func() time.Time { return id.SwitchAt().Add(time.Minute) }
	if renewed, err := id.Refresh(); err != nil || !renewed {
		t.Fatalf("not switched: %v %v", renewed, err)
	}
	next := id.Certificate()
	leaf := leafOf(t, next)
	if id.Legacy() || leaf.CheckSignatureFrom(id.CA()) != nil {
		t.Fatal("after the switch, still not a certificate the CA signs")
	}
	if !slices.Contains(leaf.DNSNames, "legacy.example.com") || !hasIP(leaf, "192.0.2.80") {
		t.Errorf("the legacy certificate's names are lost: %v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	if err := verifies(trust, next, "legacy.example.com"); err != nil {
		t.Errorf("the trust given before the switch: %v", err)
	}
	if verifies(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: legacy.Raw}), next, "legacy.example.com") == nil {
		t.Error("the legacy certificate alone takes the new one?")
	}
	if !bytes.Equal(id.TrustPEM(), id.CAPEM()) {
		t.Error("after the switch, the trust is more than the CA")
	}
}

// TestLegacyNearItsEnd: a Controller made before whose certificate ends
// within 35 days switches at its start.
func TestLegacyNearItsEnd(t *testing.T) {
	dir := t.TempDir()
	legacyIdentity(t, dir, 10*24*time.Hour)
	id, err := Open(dir, masterKey(t, dir), "")
	if err != nil {
		t.Fatal(err)
	}
	if id.Legacy() || leafOf(t, id.Certificate()).CheckSignatureFrom(id.CA()) != nil {
		t.Error("a legacy certificate ending within 35 days isn't replaced at the start")
	}
}

// TestMismatchedFiles: a key that isn't the certificate's (a renewal cut
// between its two files) gets a new certificate the CA signs.
func TestMismatchedFiles(t *testing.T) {
	dir := t.TempDir()
	key := masterKey(t, dir)
	if _, err := Open(dir, key, ""); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if _, err := Open(other, masterKey(t, other), ""); err != nil {
		t.Fatal(err)
	}
	foreign, _ := os.ReadFile(filepath.Join(other, KeyFile))
	if err := os.WriteFile(filepath.Join(dir, KeyFile), foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := Open(dir, key, "")
	if err != nil {
		t.Fatal(err)
	}
	if leafOf(t, id.Certificate()).CheckSignatureFrom(id.CA()) != nil {
		t.Error("not a certificate of the CA")
	}
}
