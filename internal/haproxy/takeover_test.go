package haproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCertificateUploadRightAfterReload runs the real build/haproxy:
// right after a reload the old process can still answer on the stats
// socket, and a certificate staged there then committed in the new one
// failed with "No ongoing transaction" (seen on the CI runner). Every
// upload straight after a reload must now succeed.
func TestCertificateUploadRightAfterReload(t *testing.T) {
	bin, _ := filepath.Abs("../../build/haproxy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("build/haproxy not built (make haproxy-build)")
	}
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	sock := filepath.Join(dir, "admin.sock")
	cfg := fmt.Sprintf(`global
    stats socket %s mode 600 level admin
defaults
    mode http
    timeout connect 1s
    timeout client 1s
    timeout server 1s
frontend f
    bind 127.0.0.1:%d
    http-request return status 200
`, sock, port)
	m := NewManager(bin, filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), sock)
	m.Output = &testWriter{t}
	if errs, err := m.Apply([]byte(cfg)); err != nil {
		t.Fatalf("Apply: %v %v", err, errs)
	}
	t.Cleanup(func() { _ = m.Stop(5 * time.Second) })

	bundle := selfSignedBundle(t, "takeover.test")
	for i := range 10 {
		if err := m.Reload(); err != nil {
			t.Fatalf("Reload %d: %v", i, err)
		}
		if err := m.CertificateUpload(fmt.Sprintf("/tmp/takeover-%d.pem", i), bundle, "", nil); err != nil {
			t.Fatalf("CertificateUpload right after reload %d: %v", i, err)
		}
	}

	// Serving: true while it runs, false as soon as a stop begins.
	if !m.Serving() {
		t.Error("Serving() is false with HAProxy running")
	}
	if err := m.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if m.Serving() {
		t.Error("Serving() is still true after Stop")
	}
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

func selfSignedBundle(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{cn}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...)
}
