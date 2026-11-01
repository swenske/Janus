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

// newAdminKey is a key a rotation's admin certificate is issued for.
func newAdminKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestBootstrapMakesNoAdmin: a first boot makes the node's CA and its
// server certificate - no admin credential, none on disk.
func TestBootstrapMakesNoAdmin(t *testing.T) {
	dir := t.TempDir()
	b, err := LoadOrBootstrap(dir, "node", nil)
	if err != nil || !b.Generated {
		t.Fatalf("first boot: %v (generated %v)", err, err == nil && b.Generated)
	}
	for _, f := range []string{adminCertFile, adminKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s made at the first boot: %v", f, err)
		}
	}
	if again, err := LoadOrBootstrap(dir, "node", nil); err != nil || again.Generated || !again.CA.Cert.Equal(b.CA.Cert) {
		t.Fatalf("second boot: %v", err)
	}
}

// TestRotate: a node's new own CA replaces the old one on disk and in
// memory, its admin certificate issued for a key the node never sees -
// and only the new CA's certificates let in afterwards. Without a key,
// nothing changes: the node makes none.
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
	cfg := NodeTLSConfig(local.CA, sc, fleet, nil)
	oldCert, oldKey, err := b.CA.Issue(IssueOptions{CommonName: "rescue:alice", Roles: []string{RoleAdmin}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	oldAdmin, err := tls.X509KeyPair(oldCert, oldKey)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := sc.Rotate(local, "localhost", nil, nil); err == nil || local.CA() != b.CA {
		t.Fatalf("a rotation without a key: %v, CA swapped %v", err, local.CA() != b.CA)
	}

	key := newAdminKey(t)
	r, err := sc.Rotate(local, "localhost", nil, &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if local.CA() != r.CA || r.CA.Cert.Equal(b.CA.Cert) {
		t.Fatalf("rotation: local CA swapped %v", local.CA() == r.CA)
	}
	admin := parseCert(t, r.AdminCertPEM)
	if admin.CheckSignatureFrom(r.CA.Cert) != nil || !admin.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey) {
		t.Error("the admin certificate isn't the new CA's, for the given key")
	}
	for _, f := range []string{adminCertFile, adminKeyFile, rotationDir} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s on disk after the rotation: %v", f, err)
		}
	}

	again, err := LoadOrBootstrap(dir, "node", nil)
	if err != nil || !again.CA.Cert.Equal(r.CA.Cert) || again.Generated {
		t.Fatalf("reloaded: %v (a new CA %v)", err, err == nil && again.Generated)
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
}

// TestRotateOlderNode: a node from before the pairing code keeps an
// admin.crt - the new one, what it is known by (cmd/janusd's pairing
// window) -, never the admin.key its first boot made.
func TestRotateOlderNode(t *testing.T) {
	dir := t.TempDir()
	b, err := LoadOrBootstrap(dir, "node", nil)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := b.CA.Issue(IssueOptions{CommonName: "admin", Roles: []string{RoleAdmin}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFiles(dir, file{adminCertFile, certPEM, 0o600}, file{adminKeyFile, keyPEM, 0o600}); err != nil {
		t.Fatal(err)
	}
	key := newAdminKey(t)
	r, err := NewServerCert(b.CA, dir, b.ServerCert).Rotate(NewLocal(b.CA), "localhost", nil, &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk, _ := os.ReadFile(filepath.Join(dir, adminCertFile)); string(onDisk) != string(r.AdminCertPEM) {
		t.Error("an older node's admin.crt isn't the new admin certificate")
	}
	if _, err := os.Stat(filepath.Join(dir, adminKeyFile)); !os.IsNotExist(err) {
		t.Errorf("an older node's admin.key kept: %v", err)
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
	r, err := sc.Rotate(NewLocal(b.CA), "localhost", nil, &newAdminKey(t).PublicKey)
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
