package uitls

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// prefixSealer stands in for the master key: reversible, bound to purpose.
type prefixSealer struct{}

func (prefixSealer) Seal(p []byte, purpose string) ([]byte, error) {
	return append([]byte(purpose+"|"), bytes.ReplaceAll(p, []byte("PRIVATE"), []byte("SEALED"))...), nil
}

func (prefixSealer) Open(s []byte, purpose string) ([]byte, error) {
	out, ok := bytes.CutPrefix(s, []byte(purpose+"|"))
	if !ok {
		return nil, errors.New("sealed for another purpose")
	}
	return bytes.ReplaceAll(out, []byte("SEALED"), []byte("PRIVATE")), nil
}

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

type issued struct {
	cert *x509.Certificate
	key  crypto.Signer
	pem  []byte
}

func keyPEM(t *testing.T, k crypto.Signer) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// issue makes a certificate - a CA's when parent is nil and ca is set.
func issue(t *testing.T, tmpl *x509.Certificate, key crypto.Signer, parent *issued) issued {
	t.Helper()
	if key == nil {
		key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	if tmpl.NotBefore.IsZero() {
		tmpl.NotBefore, tmpl.NotAfter = t0.Add(-time.Hour), t0.Add(90*24*time.Hour)
	}
	signer, parentCert := key, tmpl
	if parent != nil {
		signer, parentCert = parent.key, parent.cert
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parentCert, key.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return issued{c, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func ca(t *testing.T, name string, parent *issued) issued {
	return issue(t, &x509.Certificate{Subject: pkix.Name{CommonName: name}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}, nil, parent)
}

func server(t *testing.T, parent issued, mod func(*x509.Certificate), key crypto.Signer) issued {
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "controller.example.com"},
		DNSNames:    []string{"controller.example.com"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if mod != nil {
		mod(tmpl)
	}
	return issue(t, tmpl, key, &parent)
}

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestParse(t *testing.T) {
	root := ca(t, "Example Root", nil)
	inter := ca(t, "Example Issuing CA", &root)
	leaf := server(t, inter, nil, nil)
	good := cat(leaf.pem, inter.pem, keyPEM(t, leaf.key))

	cert, err := Check(good, t0)
	if err != nil {
		t.Fatalf("a good bundle: %v", err)
	}
	if len(cert.Certificate) != 2 || cert.Leaf.Subject.CommonName != "controller.example.com" {
		t.Errorf("served chain: %d certificates, leaf %v", len(cert.Certificate), cert.Leaf.Subject)
	}
	// The key first is fine.
	if _, err := Check(cat(keyPEM(t, leaf.key), leaf.pem, inter.pem), t0); err != nil {
		t.Errorf("the key first: %v", err)
	}

	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	edPub, edKey, _ := ed25519.GenerateKey(rand.Reader)
	_ = edPub
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	for _, tc := range []struct {
		name   string
		bundle []byte
		want   string
	}{
		{"nothing", []byte("hello"), "no certificate"},
		{"no key", cat(leaf.pem, inter.pem), "no private key"},
		{"two keys", cat(leaf.pem, keyPEM(t, leaf.key), keyPEM(t, other)), "2 private keys"},
		{"another key", cat(leaf.pem, keyPEM(t, other)), "the key and the certificate"},
		{"encrypted key", cat(leaf.pem, pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte{1}})), "encrypted"},
		{"chain out of order", cat(leaf.pem, root.pem, inter.pem, keyPEM(t, leaf.key)), "isn't issued by the next one"},
		{"a CA first", cat(inter.pem, keyPEM(t, inter.key)), "a CA's"},
		{"expired", func() []byte {
			l := server(t, inter, func(c *x509.Certificate) { c.NotBefore, c.NotAfter = t0.Add(-48*time.Hour), t0.Add(-time.Hour) }, nil)
			return cat(l.pem, keyPEM(t, l.key))
		}(), "expired"},
		{"not yet valid", func() []byte {
			l := server(t, inter, func(c *x509.Certificate) { c.NotBefore, c.NotAfter = t0.Add(time.Hour), t0.Add(48*time.Hour) }, nil)
			return cat(l.pem, keyPEM(t, l.key))
		}(), "valid from"},
		{"for clients", func() []byte {
			l := server(t, inter, func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} }, nil)
			return cat(l.pem, keyPEM(t, l.key))
		}(), "isn't for a server"},
		{"Ed25519", func() []byte {
			l := server(t, inter, nil, edKey)
			return cat(l.pem, keyPEM(t, edKey))
		}(), "Ed25519"},
		{"small RSA", func() []byte {
			l := server(t, inter, nil, small)
			return cat(l.pem, keyPEM(t, small))
		}(), "1024 bits"},
		{"too big", bytes.Repeat([]byte("x"), MaxBundle+1), "KiB"},
	} {
		if _, err := Check(tc.bundle, t0); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error with %q", tc.name, err, tc.want)
		}
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	root := ca(t, "dashboard", nil)
	l := server(t, root, func(c *x509.Certificate) { c.DNSNames = []string{"localhost"} }, nil)
	cert, err := tls.X509KeyPair(l.pem, keyPEM(t, l.key))
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestUploaded(t *testing.T) {
	dir := t.TempDir()
	self := selfSigned(t)
	s, err := Open(dir, prefixSealer{}, self, "", "")
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return t0 }
	if c, src := s.Current(); src != SourceSelfSigned || !bytes.Equal(c.Certificate[0], self.Certificate[0]) {
		t.Fatalf("a fresh Controller serves %s", src)
	}

	root := ca(t, "Example Root", nil)
	leaf := server(t, root, nil, nil)
	if _, err := s.Upload(cat(leaf.pem, keyPEM(t, leaf.key))); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetCertificate(nil)
	if !bytes.Equal(got.Certificate[0], leaf.cert.Raw) {
		t.Error("the uploaded certificate isn't served")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, fileName))
	if bytes.Contains(raw, []byte("PRIVATE KEY")) {
		t.Error("the key is stored in the clear")
	}

	// A restart serves it still.
	s2, err := Open(dir, prefixSealer{}, self, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if c, src := s2.Current(); src != SourceUploaded || !bytes.Equal(c.Certificate[0], leaf.cert.Raw) {
		t.Errorf("after a restart: %s", src)
	}
	// One that no longer opens (another master key) leaves the page
	// reachable on the self-signed identity.
	s3, err := Open(dir, otherSealer{}, self, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, src := s3.Current(); src != SourceSelfSigned {
		t.Errorf("an uploaded certificate whose key won't open: %s served", src)
	}

	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, src := s.Current(); src != SourceSelfSigned {
		t.Errorf("after removing it: %s", src)
	}
	if _, err := os.Stat(filepath.Join(dir, fileName)); !os.IsNotExist(err) {
		t.Errorf("the uploaded certificate's file is left: %v", err)
	}
}

type otherSealer struct{}

func (otherSealer) Seal(p []byte, _ string) ([]byte, error) { return p, nil }
func (otherSealer) Open([]byte, string) ([]byte, error)     { return nil, errors.New("not my key") }

func writePair(t *testing.T, dir string, l issued) (string, string) {
	t.Helper()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, l.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM(t, l.key), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	root := ca(t, "Example Root", nil)
	first := server(t, root, nil, nil)
	certFile, keyFile := writePair(t, dir, first)

	if _, err := Open(dir, prefixSealer{}, selfSigned(t), certFile, ""); err == nil {
		t.Error("-tls-cert without -tls-key accepted")
	}
	if _, err := Open(dir, prefixSealer{}, selfSigned(t), "", keyFile); err == nil {
		t.Error("-tls-key without -tls-cert accepted")
	}
	s, err := Open(dir, prefixSealer{}, selfSigned(t), certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	s.now = func() time.Time { return now }
	s.lastCheck = now
	if c, src := s.Current(); src != SourceFiles || !bytes.Equal(c.Certificate[0], first.cert.Raw) {
		t.Fatalf("serves %s", src)
	}
	if _, err := s.Upload(cat(first.pem, keyPEM(t, first.key))); !errors.Is(err, ErrFromFiles) {
		t.Errorf("an upload over the flags: %v", err)
	}
	if err := s.Remove(); !errors.Is(err, ErrFromFiles) {
		t.Errorf("removing the flags' certificate: %v", err)
	}

	// Renewed on disk: served once looked at again.
	renewed := server(t, root, nil, nil)
	writePair(t, dir, renewed)
	future := time.Now().Add(time.Minute) // a modification time that differs
	_ = os.Chtimes(certFile, future, future)
	if c, _ := s.GetCertificate(nil); !bytes.Equal(c.Certificate[0], first.cert.Raw) {
		t.Error("the files are read again before reloadEvery")
	}
	now = now.Add(reloadEvery)
	if c, _ := s.GetCertificate(nil); !bytes.Equal(c.Certificate[0], renewed.cert.Raw) {
		t.Error("the renewed files aren't served")
	}

	// Half written - a key that isn't the certificate's yet: the
	// previous one stays.
	half := server(t, root, nil, nil)
	if err := os.WriteFile(certFile, half.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	later := future.Add(time.Minute)
	_ = os.Chtimes(certFile, later, later)
	now = now.Add(reloadEvery)
	if c, _ := s.GetCertificate(nil); !bytes.Equal(c.Certificate[0], renewed.cert.Raw) {
		t.Error("a pair that doesn't load replaced the served certificate")
	}
}

func TestDescribe(t *testing.T) {
	root := ca(t, "Example Root", nil)
	l := server(t, root, func(c *x509.Certificate) {
		c.NotBefore, c.NotAfter = t0.Add(-time.Hour), t0.Add(10*24*time.Hour)
		c.IPAddresses = nil
	}, nil)
	cert, err := tls.X509KeyPair(l.pem, keyPEM(t, l.key))
	if err != nil {
		t.Fatal(err)
	}
	info := Describe(&cert, SourceUploaded, "controller.example.com:8080", t0)
	if len(info.Warnings) != 1 || !strings.Contains(info.Warnings[0], "expires in 10 days") {
		t.Errorf("warnings for the right name: %v", info.Warnings)
	}
	if info.Issuer != "CN=Example Root" || len(info.Names) != 1 || info.Names[0] != "controller.example.com" || len(info.Fingerprint) != 64 {
		t.Errorf("info = %+v", info)
	}
	info = Describe(&cert, SourceUploaded, "192.0.2.10:8080", t0)
	if len(info.Warnings) != 2 || !strings.Contains(info.Warnings[0], "doesn't name 192.0.2.10") {
		t.Errorf("warnings for an address it doesn't name: %v", info.Warnings)
	}
}
