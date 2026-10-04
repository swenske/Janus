package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func parseCert(t *testing.T, p []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(p)
	if block == nil {
		t.Fatal("no PEM certificate")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestRotate: a node's new own CA replaces the old one on disk and in
// memory, its admin certificate issued for a key the node never sees -
// and only the new CA's certificates let in afterwards.
func TestRotate(t *testing.T) {
	dir := t.TempDir()
	b, err := LoadOrBootstrap(dir, "node", nil)
	if err != nil {
		t.Fatal(err)
	}
	local := NewLocal(b.CA)
	sc := NewServerCert(b.CA, dir, b.ServerCert)
	fleet, err := OpenFleet(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := NodeTLSConfig(local.CA, sc, fleet)
	oldAdmin, err := tls.X509KeyPair(b.AdminCertPEM, b.AdminKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r, err := sc.Rotate(local, "localhost", nil, &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if r.AdminKeyPEM != nil || local.CA() != r.CA || r.CA.Cert.Equal(b.CA.Cert) {
		t.Fatalf("rotation: key %v, local CA swapped %v", r.AdminKeyPEM != nil, local.CA() == r.CA)
	}
	admin := parseCert(t, r.AdminCertPEM)
	if admin.CheckSignatureFrom(r.CA.Cert) != nil || !admin.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey) {
		t.Error("the admin certificate isn't the new CA's, for the given key")
	}
	if _, err := os.Stat(filepath.Join(dir, adminKeyFile)); !os.IsNotExist(err) {
		t.Errorf("admin.key kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, rotationDir)); !os.IsNotExist(err) {
		t.Errorf("rotation directory left: %v", err)
	}

	again, err := LoadOrBootstrap(dir, "node", nil)
	if err != nil || !again.CA.Cert.Equal(r.CA.Cert) || again.AdminIssued {
		t.Fatalf("reloaded: %v (admin issued %v)", err, again.AdminIssued)
	}

	newAdmin, err := tls.X509KeyPair(r.AdminCertPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: must(x509.MarshalPKCS8PrivateKey(key))}))
	if err != nil {
		t.Fatal(err)
	}
	roots := r.CA.CertPool()
	if err := handshake(t, cfg, newAdmin, roots); err != nil {
		t.Errorf("the new admin certificate refused: %v", err)
	}
	if err := handshake(t, cfg, oldAdmin, roots); err == nil {
		t.Error("the old CA's admin certificate still lets in")
	}

	// Without a public key, the node makes the key: kept like at first
	// boot, to be shown once.
	r2, err := sc.Rotate(local, "localhost", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk, _ := os.ReadFile(filepath.Join(dir, adminKeyFile)); string(onDisk) != string(r2.AdminKeyPEM) || r2.AdminKeyPEM == nil {
		t.Error("the node-made admin key isn't kept")
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// TestRotationInterrupted: a power cut leaves the old CA or the new one,
// whole - a rotation is finished at boot only if it was ready.
func TestRotationInterrupted(t *testing.T) {
	setup := func(t *testing.T, ready bool, movedOne bool) (dir string, old, next *CA) {
		dir = t.TempDir()
		b, err := LoadOrBootstrap(dir, "node", nil)
		if err != nil {
			t.Fatal(err)
		}
		next, err = NewCA("next")
		if err != nil {
			t.Fatal(err)
		}
		keyPEM, _ := next.KeyPEM()
		rd := filepath.Join(dir, rotationDir)
		if err := os.MkdirAll(rd, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{caCertFile: next.CertPEM, caKeyFile: keyPEM} {
			if err := os.WriteFile(filepath.Join(rd, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if ready {
			if err := os.WriteFile(filepath.Join(rd, rotationReady), []byte(adminKeyFile), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if movedOne {
			if err := os.Rename(filepath.Join(rd, caCertFile), filepath.Join(dir, caCertFile)); err != nil {
				t.Fatal(err)
			}
		}
		return dir, b.CA, next
	}

	dir, old, _ := setup(t, false, false)
	b, err := LoadOrBootstrap(dir, "node", nil)
	if err != nil || !b.CA.Cert.Equal(old.Cert) {
		t.Errorf("not ready: %v, kept the old CA %v", err, err == nil && b.CA.Cert.Equal(old.Cert))
	}

	for _, moved := range []bool{false, true} {
		dir, _, next := setup(t, true, moved)
		b, err := LoadOrBootstrap(dir, "node", nil)
		if err != nil || !b.CA.Cert.Equal(next.Cert) {
			t.Errorf("ready (one file already moved: %v): %v", moved, err)
		}
		if _, err := os.Stat(filepath.Join(dir, adminKeyFile)); !os.IsNotExist(err) {
			t.Errorf("ready (moved %v): admin.key not removed", moved)
		}
	}
}

// verifiesNode reports whether a client pinning roots verifies the
// server certificate sc serves for localhost.
func verifiesNode(t *testing.T, sc *ServerCert, roots *x509.CertPool) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: sc.GetCertificate, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13})
	if err != nil {
		return err
	}
	return conn.Close()
}

// TestRotationCrossSigned: after the node replaced its CA, a client that
// pinned the old one still verifies it - through the new CA cross-signed
// by the old - as does one with the new CA; after the server certificate
// is renewed too, and once read back from disk.
func TestRotationCrossSigned(t *testing.T) {
	dir := t.TempDir()
	b, err := LoadOrBootstrap(dir, "localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	sc := NewServerCert(b.CA, dir, b.ServerCert)
	r, err := sc.Rotate(NewLocal(b.CA), "localhost", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	check := func(stage string, sc *ServerCert) {
		t.Helper()
		if err := verifiesNode(t, sc, b.CA.CertPool()); err != nil {
			t.Errorf("%s: a client pinning the old CA: %v", stage, err)
		}
		if err := verifiesNode(t, sc, r.CA.CertPool()); err != nil {
			t.Errorf("%s: a client pinning the new CA: %v", stage, err)
		}
	}
	check("rotated", sc)
	if _, err := sc.Refresh("localhost", []net.IP{net.ParseIP("192.0.2.7")}); err != nil {
		t.Fatal(err)
	}
	check("server certificate renewed", sc)
	again, err := LoadOrBootstrap(dir, "localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	check("read back", NewServerCert(again.CA, dir, again.ServerCert))

	stranger, _ := NewCA("stranger")
	if err := verifiesNode(t, sc, stranger.CertPool()); err == nil {
		t.Error("a client pinning another CA verifies the node")
	}
}
