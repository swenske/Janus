package haproxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestFiles runs the real build/haproxy: files a configuration references
// are put, read back (never a private key), refused when haproxy.cfg
// wouldn't load with them - the previous one kept - and removed only when
// nothing needs them.
func TestFiles(t *testing.T) {
	bin, _ := filepath.Abs("../../build/haproxy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("build/haproxy not built (make haproxy-build)")
	}
	dir := t.TempDir()
	m := NewManager(bin, filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), filepath.Join(dir, "admin.sock"))
	m.Output = &testWriter{t}
	m.FilesDir = filepath.Join(dir, "files")

	page := []byte("HTTP/1.0 503 Service Unavailable\r\nContent-Type: text/html\r\n\r\n<html>down</html>\n")
	cert := selfSignedBundle(t, "admin.test")
	if errs, err := m.FilePut("errors/503.http", page); err != nil {
		t.Fatal(errs, err)
	}
	if errs, err := m.FilePut("certs/admin.pem", cert); err != nil {
		t.Fatal(errs, err)
	}
	if errs, err := m.FilePut("unused.map", []byte("a.test backend_a\n")); err != nil {
		t.Fatal(errs, err)
	}

	tlsPort, httpPort := freePort(t), freePort(t)
	cfg := fmt.Sprintf(`global
    stats socket %s mode 600 level admin
defaults
    mode http
    timeout connect 1s
    timeout client 1s
    timeout server 1s
    errorfile 503 %s
frontend admin
    bind 127.0.0.1:%d ssl crt %s
    http-request return status 200
frontend web
    bind 127.0.0.1:%d
    default_backend none
backend none
`, m.StatsSocketPath, filepath.Join(m.FilesDir, "errors/503.http"), tlsPort, filepath.Join(m.FilesDir, "certs/admin.pem"), httpPort)
	if errs, err := m.Apply([]byte(cfg)); err != nil {
		t.Fatalf("Apply: %v %v", err, errs)
	}
	t.Cleanup(func() { _ = m.Stop(5 * time.Second) })
	if cn := servedCN(t, tlsPort, "admin.test"); cn != "admin.test" {
		t.Errorf("served %q", cn)
	}

	files, err := m.Files()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
		if f.Name == "certs/admin.pem" && !f.Secret {
			t.Error("the certificate's key isn't marked secret")
		}
		if f.Name == "errors/503.http" && (f.Secret || f.Size != int64(len(page)) || len(f.SHA256) != 64) {
			t.Errorf("error page info %+v", f)
		}
	}
	if !slices.Equal(names, []string{"certs/admin.pem", "errors/503.http", "unused.map"}) {
		t.Errorf("files %v", names)
	}

	if got, err := m.FileRead("errors/503.http"); err != nil || string(got) != string(page) {
		t.Errorf("read the page: %q %v", got, err)
	}
	if _, err := m.FileRead("certs/admin.pem"); !errors.Is(err, ErrSecretFile) {
		t.Errorf("a private key was read back: %v", err)
	}
	if _, err := m.FileRead("nope"); !errors.Is(err, ErrNoFile) {
		t.Errorf("a missing file: %v", err)
	}

	// A broken certificate: refused, the working one kept.
	errs, err := m.FilePut("certs/admin.pem", []byte("not a certificate\n"))
	if err == nil || len(errs) == 0 {
		t.Fatalf("a broken certificate was accepted: %v %v", errs, err)
	}
	if got, _ := os.ReadFile(filepath.Join(m.FilesDir, "certs/admin.pem")); string(got) != string(cert) {
		t.Error("the previous certificate isn't back after a refused change")
	}

	// In use: refused. Unused: removed.
	if errs, err := m.FileDelete("errors/503.http"); err == nil || !strings.Contains(strings.Join(errs, " "), "503") {
		t.Errorf("an error page in use was removed: %v %v", errs, err)
	}
	if _, err := os.Stat(filepath.Join(m.FilesDir, "errors/503.http")); err != nil {
		t.Error("the page in use isn't back")
	}
	if errs, err := m.FileDelete("unused.map"); err != nil {
		t.Errorf("an unused file: %v %v", errs, err)
	}
	if _, err := m.FileDelete("unused.map"); !errors.Is(err, ErrNoFile) {
		t.Errorf("deleting it twice: %v", err)
	}

	for _, bad := range []string{"../x", "/etc/passwd", ".hidden", "a/b/c", "a b", ""} {
		if _, err := m.FilePut(bad, []byte("x")); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("name %q accepted: %v", bad, err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".haproxy-file-*")); len(left) != 0 {
		t.Errorf("scratch files left: %v", left)
	}
}
