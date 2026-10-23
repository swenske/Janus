package acmerelay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/pki"
)

// fixture is a relay with the Controller's identity, one approved node
// (CA a) and a node it doesn't know (CA b).
type fixture struct {
	relay     *Server
	identity  *tls.Certificate
	idPool    *x509.CertPool
	approved  *pki.CA
	stranger  *pki.CA
	relayAddr string
}

func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()
	f := &fixture{}
	ctl, err := pki.NewCA("controller")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ctl.Issue(pki.IssueOptions{CommonName: "dashboard", IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	f.identity = &id
	f.idPool = x509.NewCertPool()
	f.idPool.AppendCertsFromPEM(ctl.CertPEM)
	if f.approved, err = pki.NewCA("node-a"); err != nil {
		t.Fatal(err)
	}
	if f.stranger, err = pki.NewCA("node-b"); err != nil {
		t.Fatal(err)
	}
	approvedPool := x509.NewCertPool()
	approvedPool.AddCert(f.approved.Cert)
	var logs []string
	f.relay = &Server{
		Identity: func() *tls.Certificate { return f.identity },
		NodeOf: func(chain []*x509.Certificate) (Node, bool) {
			if len(chain) == 0 {
				return Node{}, false
			}
			_, err := chain[0].Verify(x509.VerifyOptions{Roots: approvedPool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
			return Node{ID: "n1", Name: "edge-1"}, err == nil
		},
		Logf: func(format string, args ...any) { logs = append(logs, format) },
	}
	cfg.Enabled, cfg.Listen = true, "127.0.0.1:0"
	if err := f.relay.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.relay.Close)
	f.relayAddr = f.relay.Status().Listening
	return f
}

// client goes through the relay with a certificate of ca (none: nil),
// trusting target's CA.
func (f *fixture) client(t *testing.T, ca *pki.CA, target *httptest.Server) *http.Client {
	t.Helper()
	proxyTLS := &tls.Config{RootCAs: f.idPool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	if ca != nil {
		certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{CommonName: "acme-relay", ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		if err != nil {
			t.Fatal(err)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		proxyTLS.Certificates = []tls.Certificate{cert}
	}
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	u, _ := url.Parse("https://" + f.relayAddr)
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(u),
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			tc := tls.Client(c, proxyTLS)
			if err := tc.HandshakeContext(ctx); err != nil {
				c.Close()
				return nil, err
			}
			return tc, nil
		},
	}}
}

func target(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "directory") }))
	t.Cleanup(srv.Close)
	return srv
}

func TestTunnel(t *testing.T) {
	ca := target(t)
	f := newFixture(t, Config{Allowed: []string{strings.TrimPrefix(ca.URL, "https://")}})
	c := f.client(t, f.approved, ca)
	resp, err := c.Get(ca.URL + "/dir")
	if err != nil {
		t.Fatalf("an approved node to an allowed destination: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "directory" {
		t.Errorf("got %q", body)
	}
	// The tunnel is recorded once it ends: the client's idle connection
	// closed.
	c.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := f.relay.Status()
		if len(st.Recent) == 1 && st.Open == 0 {
			tun := st.Recent[0]
			if tun.Node != "edge-1" || tun.Target != strings.TrimPrefix(ca.URL, "https://") || tun.Up == 0 || tun.Down == 0 || tun.Refused != "" {
				t.Errorf("recorded %+v", tun)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no tunnel recorded: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRefusals(t *testing.T) {
	ca := target(t)
	f := newFixture(t, Config{})
	if _, err := f.client(t, f.stranger, ca).Get(ca.URL); err == nil {
		t.Error("a node the Controller doesn't know went through")
	}
	if _, err := f.client(t, nil, ca).Get(ca.URL); err == nil {
		t.Error("no client certificate went through")
	}
	_, err := f.client(t, f.approved, ca).Get(ca.URL)
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("a destination not allowed: %v", err)
	}
	st := f.relay.Status()
	if len(st.Recent) != 1 || !strings.Contains(st.Recent[0].Refused, "not a destination the relay allows") {
		t.Errorf("recent %+v", st.Recent)
	}
	if !st.Config.Enabled || st.Listening == "" || st.BuiltIn["Let's Encrypt"] == nil {
		t.Errorf("status %+v", st)
	}
	// A plain-HTTP request through the relay - an http:// DNS API: refused,
	// and journaled.
	plain := f.client(t, f.approved, ca)
	resp, err := plain.Get("http://api.dns.test:8080/present")
	if err != nil {
		t.Fatalf("a plain-HTTP request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("a plain-HTTP request: %d", resp.StatusCode)
	}
	st = f.relay.Status()
	if len(st.Recent) != 2 || st.Recent[0].Target != "api.dns.test:8080" || !strings.Contains(st.Recent[0].Refused, "only tunnels HTTPS") {
		t.Errorf("the plain-HTTP refusal isn't journaled: %+v", st.Recent)
	}
	if !f.relay.allowed("acme-v02.api.letsencrypt.org:443") || f.relay.allowed("acme-v02.api.letsencrypt.org:80") {
		t.Error("the built-in destinations are port 443 only")
	}
}

func TestApply(t *testing.T) {
	f := newFixture(t, Config{})
	if err := f.relay.Apply(Config{Enabled: true, Listen: "127.0.0.1:0", Allowed: []string{" Pebble.test:14000 ", "pebble.test:14000"}}); err != nil {
		t.Fatal(err)
	}
	st := f.relay.Status()
	if st.Listening != f.relayAddr {
		t.Errorf("relistened for a change of destinations: %s -> %s", f.relayAddr, st.Listening)
	}
	if len(st.Config.Allowed) != 1 || st.Config.Allowed[0] != "pebble.test:14000" {
		t.Errorf("allowed %v", st.Config.Allowed)
	}
	if err := f.relay.Apply(Config{Enabled: true, Allowed: []string{"https://x.test"}}); err == nil {
		t.Error("a URL as a destination")
	}
	busy := f.relay.Status().Listening
	other := &Server{Identity: f.relay.Identity, NodeOf: f.relay.NodeOf, Logf: func(string, ...any) {}}
	if err := other.Apply(Config{Enabled: true, Listen: busy}); err == nil {
		other.Close()
		t.Error("listened on a busy address")
	}
	other.Boot(Config{Enabled: true, Listen: busy})
	if st := other.Status(); st.Error == "" || st.Listening != "" || !st.Config.Enabled {
		t.Errorf("a boot that can't listen: %+v", st)
	}
	if err := f.relay.Apply(Config{}); err != nil {
		t.Fatal(err)
	}
	if st := f.relay.Status(); st.Listening != "" {
		t.Errorf("still listening when off: %+v", st)
	}
	if _, err := net.DialTimeout("tcp", busy, time.Second); err == nil {
		t.Error("the port still answers once off")
	}
}

func TestLimits(t *testing.T) {
	s := &Server{}
	_ = s.Apply(Config{})
	for range maxPerNode {
		if !s.take("n1") {
			t.Fatal("refused under the limit")
		}
	}
	if s.take("n1") {
		t.Error("one more tunnel than the limit per node")
	}
	if !s.take("n2") {
		t.Error("another node refused")
	}
	s.release("n1")
	if !s.take("n1") {
		t.Error("refused once one ended")
	}
}
