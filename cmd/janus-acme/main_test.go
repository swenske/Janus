package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/swenske/Janus/internal/acme/acmewire"
	"github.com/swenske/Janus/internal/pki"
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
	// Hijacked before answering, and read through its buffer: the server
	// may read the client's first bytes ahead once the answer is out.
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		up.Close()
		conn.Close()
		return
	}
	go func() { _, _ = io.Copy(up, buf); up.Close() }()
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
	keepGlobals(t)
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

// keepGlobals puts back what run changes for the DNS providers: the
// default transport and the proxy variables.
func keepGlobals(t *testing.T) {
	t.Helper()
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY"} {
		t.Setenv(k, "")
	}
}

// TestRelayLikeProxy: an https:// proxy asking for a client certificate
// of a CA it knows - the Controller's relay, knowing the node by one of
// its own CA. Without the certificate it's refused; with it, the
// directory comes through, and so does what the DNS providers' clients
// send (http.DefaultTransport).
func TestRelayLikeProxy(t *testing.T) {
	keepGlobals(t)
	dir := acmeDirectory(t)
	nodeCA, err := pki.NewCA("node")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(nodeCA.CertPEM)
	cp := &connectProxy{}
	proxy := httptest.NewUnstartedServer(cp)
	proxy.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	proxy.StartTLS()
	defer proxy.Close()

	req := &acmewire.Request{Action: acmewire.ActionDirectory, Directory: dir.URL + "/dir", DirectoryCA: caPEM(dir), Proxy: proxy.URL, ProxyCA: caPEM(proxy)}
	if err := run(req, &acmewire.Result{}); err == nil {
		t.Fatal("went through without a client certificate")
	}
	certPEM, keyPEM, err := nodeCA.Issue(pki.IssueOptions{CommonName: "acme-relay", ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	req.ProxyCert, req.ProxyKey = string(certPEM), string(keyPEM)
	res := &acmewire.Result{}
	if err := run(req, res); err != nil {
		t.Fatalf("with the node's certificate: %v", err)
	}
	if res.TermsURL != dir.URL+"/terms" {
		t.Errorf("terms %q", res.TermsURL)
	}
	// A DNS provider's API: lego's clients leave the transport to the
	// default one - the system's trust store for the API, so this test
	// server's CA is added to it here.
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "zone updated") }))
	defer api.Close()
	dt := http.DefaultTransport.(*http.Transport)
	roots := x509.NewCertPool()
	roots.AddCert(api.Certificate())
	dt.TLSClientConfig.RootCAs = roots
	resp, err := (&http.Client{}).Get(api.URL + "/zone")
	if err != nil {
		t.Fatalf("a DNS provider's API through the proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "zone updated" {
		t.Errorf("the API answered %q", body)
	}
	if got := cp.asked(); len(got) < 2 || got[len(got)-1] != strings.TrimPrefix(api.URL, "https://") {
		t.Errorf("the proxy was asked for %v", got)
	}
	if os.Getenv("HTTPS_PROXY") != proxy.URL {
		t.Errorf("HTTPS_PROXY = %q, for the AWS SDK", os.Getenv("HTTPS_PROXY"))
	}
}

// TestDirectoryProxyRefused: a proxy that isn't there says so.
func TestDirectoryProxyRefused(t *testing.T) {
	keepGlobals(t)
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
