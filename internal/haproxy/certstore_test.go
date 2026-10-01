package haproxy

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// servedCN is the common name of the certificate HAProxy presents on
// port for sni ("" for none).
func servedCN(t *testing.T, port int, sni string) string {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", fmt.Sprintf("127.0.0.1:%d", port),
		&tls.Config{ServerName: sni, InsecureSkipVerify: true}) //nolint:gosec // the test reads which certificate is served
	if err != nil {
		t.Fatalf("TLS to :%d (sni %q): %v", port, sni, err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Subject.CommonName
}

func certNames(t *testing.T, m *Manager) []string {
	t.Helper()
	certs, err := m.CertificateList()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range certs {
		names = append(names, c.Name)
	}
	return names
}

// TestCertificatesSurviveReloadAndRestart runs the real build/haproxy:
// certificates uploaded at runtime - one stored only, one bound into a
// crt-list for an SNI - are back after a reload and after a restart by a
// new Manager (a janusd restart, a reboot), until deleted.
func TestCertificatesSurviveReloadAndRestart(t *testing.T) {
	bin, _ := filepath.Abs("../../build/haproxy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("build/haproxy not built (make haproxy-build)")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "admin.sock")
	crtList := filepath.Join(dir, "crt-list.txt")
	if err := os.WriteFile(filepath.Join(dir, "default.pem"), selfSignedBundle(t, "default.test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crtList, []byte(filepath.Join(dir, "default.pem")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	httpPort, tlsPort := freePort(t), freePort(t)
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
frontend tls
    bind 127.0.0.1:%d ssl crt-list %s
    http-request return status 200
`, sock, httpPort, tlsPort, crtList)
	newManager := func() *Manager {
		m := NewManager(bin, filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), sock)
		m.Output = &testWriter{t}
		m.CertStoreDir = filepath.Join(dir, "runtime-certs")
		return m
	}
	m := newManager()
	if errs, err := m.Apply([]byte(cfg)); err != nil {
		t.Fatalf("Apply: %v %v", err, errs)
	}
	t.Cleanup(func() { _ = m.Stop(5 * time.Second) })

	if err := m.CertificateUpload("stored.pem", selfSignedBundle(t, "stored.test"), "", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.CertificateUpload("served.pem", selfSignedBundle(t, "served.test"), crtList, []string{"served.test"}); err != nil {
		t.Fatal(err)
	}
	if cn := servedCN(t, tlsPort, "served.test"); cn != "served.test" {
		t.Fatalf("before any reload, SNI served.test got %q", cn)
	}
	check := func(when string) {
		t.Helper()
		names := certNames(t, m)
		for _, want := range []string{"stored.pem", "served.pem"} {
			if !slices.Contains(names, want) {
				t.Errorf("%s: %s isn't loaded: %v", when, want, names)
			}
		}
		if cn := servedCN(t, tlsPort, "served.test"); cn != "served.test" {
			t.Errorf("%s: SNI served.test got %q", when, cn)
		}
		if cn := servedCN(t, tlsPort, ""); cn != "default.test" {
			t.Errorf("%s: no SNI got %q", when, cn)
		}
	}

	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	check("after a reload")

	// A restart: another Manager, a fresh HAProxy, the same store.
	if err := m.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	m = newManager()
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	check("after a restart")

	if err := m.CertificateDelete("stored.pem", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.CertificateDelete("served.pem", crtList); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if names := certNames(t, m); slices.Contains(names, "stored.pem") || slices.Contains(names, "served.pem") {
		t.Errorf("deleted certificates are back after a reload: %v", names)
	}
	if cn := servedCN(t, tlsPort, "served.test"); cn != "default.test" {
		t.Errorf("after the delete, SNI served.test got %q", cn)
	}
	if stored, err := m.StoredCertificates(); err != nil || !reflect.DeepEqual(stored, []string{}) {
		t.Errorf("store after the deletes: %v %v", stored, err)
	}
	if err := m.CertificateDelete("stored.pem", ""); err == nil {
		t.Error("deleting a certificate that's gone everywhere succeeded")
	}
	fi, err := os.Stat(m.CertStoreDir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("store directory mode: %v %v", fi.Mode(), err)
	}
}
