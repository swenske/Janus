package main

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/swenske/Janus/internal/acme/acmewire"
)

// connectProxy is an HTTP proxy that only tunnels (CONNECT), recording
// the hosts asked for.
type connectProxy struct {
	mu    sync.Mutex
	hosts []string
}

func (p *connectProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	p.mu.Lock()
	p.hosts = append(p.hosts, r.Host)
	p.mu.Unlock()
	up, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		up.Close()
		return
	}
	go func() { _, _ = io.Copy(up, conn); up.Close() }()
	_, _ = io.Copy(conn, up)
	conn.Close()
}

func (p *connectProxy) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.hosts...)
}

// acmeDirectory is an ACME server's directory, and nothing else.
func acmeDirectory(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(nil)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := srv.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"newNonce": base + "/nonce", "newAccount": base + "/account", "newOrder": base + "/order",
			"revokeCert": base + "/revoke", "keyChange": base + "/key",
			"meta": map[string]any{"termsOfService": base + "/terms"},
		})
	})
	t.Cleanup(srv.Close)
	return srv
}

func caPEM(srv *httptest.Server) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

// TestDirectoryThroughAProxy: the directory fetched through an http://
// proxy, then an https:// one checked against its own CA - the ACME
// server's TLS inside the tunnel, against directory_ca.
func TestDirectoryThroughAProxy(t *testing.T) {
	dir := acmeDirectory(t)
	for _, tls := range []bool{false, true} {
		cp := &connectProxy{}
		var proxy *httptest.Server
		if tls {
			proxy = httptest.NewTLSServer(cp)
		} else {
			proxy = httptest.NewServer(cp)
		}
		req := &acmewire.Request{Action: acmewire.ActionDirectory, Directory: dir.URL + "/dir", DirectoryCA: caPEM(dir), Proxy: proxy.URL}
		if tls {
			req.ProxyCA = caPEM(proxy)
		}
		res := &acmewire.Result{}
		if err := run(req, res); err != nil {
			t.Fatalf("tls %v: %v", tls, err)
		}
		if res.TermsURL != dir.URL+"/terms" {
			t.Errorf("tls %v: terms %q", tls, res.TermsURL)
		}
		if got := cp.asked(); len(got) == 0 || got[0] != strings.TrimPrefix(dir.URL, "https://") {
			t.Errorf("tls %v: the proxy was asked for %v", tls, got)
		}
		proxy.Close()
	}
}

// TestDirectoryProxyRefused: a proxy that isn't there says so.
func TestDirectoryProxyRefused(t *testing.T) {
	dir := acmeDirectory(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	err = run(&acmewire.Request{Action: acmewire.ActionDirectory, Directory: dir.URL + "/dir", DirectoryCA: caPEM(dir), Proxy: "http://" + addr}, &acmewire.Result{})
	if err == nil || !strings.Contains(err.Error(), "proxyconnect") {
		t.Errorf("a proxy not listening: %v", err)
	}
}
