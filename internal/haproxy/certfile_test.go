package haproxy

import (
	"bytes"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// servedDER is the certificate HAProxy presents on port for sni.
func servedDER(t *testing.T, port int, sni string) []byte {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", fmt.Sprintf("127.0.0.1:%d", port),
		&tls.Config{ServerName: sni, InsecureSkipVerify: true}) //nolint:gosec // the test reads which certificate is served
	if err != nil {
		t.Fatalf("TLS to :%d (sni %q): %v", port, sni, err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Raw
}

// TestReplaceCertFileAndEnv runs the real build/haproxy: a certificate
// loaded from a crt directory is swapped without a reload, a file HAProxy
// didn't load is reported as such, and Env reaches the configuration.
func TestReplaceCertFileAndEnv(t *testing.T) {
	bin, _ := filepath.Abs("../../build/haproxy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("build/haproxy not built (make haproxy-build)")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "admin.sock")
	acmeDir := filepath.Join(dir, "acme")
	if err := os.Mkdir(acmeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "default.pem"), selfSignedBundle(t, "default.test"), 0o600); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(acmeDir, "site.pem")
	if err := os.WriteFile(certPath, selfSignedBundle(t, "site.test"), 0o600); err != nil {
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
    http-request return status 200 content-type text/plain lf-string "%%[path,field(-1,/)].${JANUS_TEST_THUMBPRINT}" if { path_beg /.well-known/acme-challenge/ }
frontend tls
    bind 127.0.0.1:%d ssl crt %s crt %s/
    http-request return status 200
`, sock, httpPort, tlsPort, filepath.Join(dir, "default.pem"), acmeDir)
	m := NewManager(bin, filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), sock)
	m.Output = &testWriter{t}
	m.Env = func() []string { return []string{"JANUS_TEST_THUMBPRINT=thumb"} }
	if errs, err := m.Apply([]byte(cfg)); err != nil {
		t.Fatalf("Apply: %v %v", err, errs)
	}
	t.Cleanup(func() { _ = m.Stop(5 * time.Second) })
	if m.StartedAt().IsZero() {
		t.Error("StartedAt is zero while HAProxy runs")
	}

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/.well-known/acme-challenge/tok", httpPort))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "tok.thumb" {
		t.Errorf("challenge answer %q, want tok.thumb", body)
	}

	next := selfSignedBundle(t, "site.test")
	block, _ := pem.Decode(next)
	if bytes.Equal(servedDER(t, tlsPort, "site.test"), block.Bytes) {
		t.Fatal("the new certificate is served before the swap")
	}
	if err := m.ReplaceCertFile(certPath, next); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(servedDER(t, tlsPort, "site.test"), block.Bytes) {
		t.Error("the swapped certificate isn't served")
	}
	if got := m.Counters().Reloads.Load(); got != 0 {
		t.Errorf("%d reloads, want none", got)
	}

	if err := m.ReplaceCertFile(filepath.Join(acmeDir, "other.pem"), next); !errors.Is(err, ErrCertNotLoaded) {
		t.Errorf("a file HAProxy didn't load: %v, want ErrCertNotLoaded", err)
	}
}
