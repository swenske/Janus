package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func leafOf(t *testing.T, s *ServerCert) *x509.Certificate {
	t.Helper()
	c, _ := s.GetCertificate(nil)
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func TestServerCertRefresh(t *testing.T) {
	dir := t.TempDir()
	b, err := LoadOrBootstrap(dir, "node1", []net.IP{net.ParseIP("10.0.2.15")})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCert(b.CA, dir, b.ServerCert)

	// Same hostname and addresses (link-local ignored): nothing to do.
	if again, err := s.Refresh("node1", []net.IP{net.ParseIP("10.0.2.15"), net.ParseIP("fe80::1")}); err != nil || again {
		t.Fatalf("refresh with unchanged SANs: reissued=%v err=%v", again, err)
	}

	// A new address: reissued, served at once, persisted.
	if again, err := s.Refresh("node1", []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::10")}); err != nil || !again {
		t.Fatalf("refresh with a new address: reissued=%v err=%v", again, err)
	}
	leaf := leafOf(t, s)
	if err := leaf.VerifyHostname("192.0.2.10"); err != nil {
		t.Errorf("new address not covered: %v", err)
	}
	if err := leaf.VerifyHostname("10.0.2.15"); err == nil {
		t.Error("the old address is still covered")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: b.CA.CertPool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Errorf("reissued certificate doesn't chain to the node CA: %v", err)
	}
	reloaded, err := LoadOrBootstrap(dir, "node1", nil)
	if err != nil || reloaded.AdminIssued {
		t.Fatalf("reload: %v (admin reissued: %v)", err, reloaded.AdminIssued)
	}
	if rl, _ := x509.ParseCertificate(reloaded.ServerCert.Certificate[0]); rl.VerifyHostname("192.0.2.10") != nil {
		t.Error("the reissued certificate wasn't persisted")
	}

	// A new hostname: reissued.
	if again, _ := s.Refresh("lb1", []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::10")}); !again {
		t.Error("hostname change didn't reissue")
	}
	if err := leafOf(t, s).VerifyHostname("lb1"); err != nil {
		t.Error(err)
	}
}

func TestServerCertRenewsNearExpiry(t *testing.T) {
	dir := t.TempDir()
	b, err := LoadOrBootstrap(dir, "node1", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A certificate with the right SANs but ten days left.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "node1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 24 * time.Hour),
		DNSNames: []string{"node1", "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1").To4()},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, b.CA.Cert, &key.PublicKey, b.CA.Key)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCert(b.CA, dir, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})
	if again, err := s.Refresh("node1", nil); err != nil || !again {
		t.Fatalf("near expiry: reissued=%v err=%v", again, err)
	}
	if left := time.Until(leafOf(t, s).NotAfter); left < 300*24*time.Hour {
		t.Errorf("renewed certificate still expires in %s", left)
	}
}

func TestLoadRecoversMismatchedServerPair(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrBootstrap(dir, "node1", nil); err != nil {
		t.Fatal(err)
	}
	// A power cut between the two renames: a key that isn't the
	// certificate's.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(other)
	if err := os.WriteFile(filepath.Join(dir, serverKeyFile), encodePEM(caKeyPEMType, keyDER), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrBootstrap(dir, "node1", []net.IP{net.ParseIP("10.0.2.15")})
	if err != nil {
		t.Fatalf("a mismatched server pair must be reissued, not fatal: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(dir, serverCertFile), filepath.Join(dir, serverKeyFile)); err != nil {
		t.Errorf("reissued pair on disk: %v", err)
	}
	if leaf, _ := x509.ParseCertificate(b.ServerCert.Certificate[0]); leaf.VerifyHostname("10.0.2.15") != nil {
		t.Error("reissued certificate misses the node's address")
	}
}
