package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// janusctl goes through the proxy the environment names - HTTPS_PROXY,
// NO_PROXY - to the Controller (its API, login's handshakes) and to the
// nodes (gRPC): Claude Code's sandbox lets commands out that way only.
// The environment is read once per process (http.ProxyFromEnvironment):
// each case runs in a child process of the test binary, against a
// CONNECT proxy here that tunnels made-up names to local servers -
// loopback addresses are never proxied.

const (
	proxyChildEnv  = "JANUSCTL_TEST_PROXY_CHILD"
	controllerName = "controller.janus.test:443"
	nodeName       = "node.janus.test:9505"
)

// connectProxy is an HTTP proxy that only tunnels (CONNECT), to the
// addresses routes gives for the names asked - noting each.
type connectProxy struct {
	ln     net.Listener
	routes map[string]string
	mu     sync.Mutex
	seen   []string
}

func newConnectProxy(t *testing.T, routes map[string]string) *connectProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &connectProxy{ln: ln, routes: routes}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return p
}

func (p *connectProxy) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.seen = append(p.seen, req.Method+" "+req.Host)
	p.mu.Unlock()
	to, ok := p.routes[req.Host]
	if req.Method != http.MethodConnect || !ok {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		return
	}
	up, err := net.Dial("tcp", to)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer up.Close()
	_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	go func() { _, _ = io.Copy(up, br); _ = up.(*net.TCPConn).CloseWrite() }()
	_, _ = io.Copy(c, up)
}

func (p *connectProxy) connects() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

// proxyFixture is what a child process gets: the Controller's and the
// node's certificates, the client's.
type proxyFixture struct {
	ControllerCert string `json:"controller_cert"`
	NodeCA         string `json:"node_ca"`
	ClientCert     string `json:"client_cert"`
	ClientKey      string `json:"client_key"`
}

func TestThroughTheProxy(t *testing.T) {
	if os.Getenv(proxyChildEnv) != "" {
		proxyChild(t, os.Getenv(proxyChildEnv))
		return
	}
	// The Controller: an HTTPS API.
	controller := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	defer controller.Close()
	// A node: gRPC over mTLS, nothing implemented - Unimplemented is an
	// answer from it.
	ca, err := pki.NewCA("node")
	if err != nil {
		t.Fatal(err)
	}
	srvCert, srvKey, err := ca.Issue(pki.IssueOptions{CommonName: "node", DNSNames: []string{"node.janus.test"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		t.Fatal(err)
	}
	cliCert, cliKey, err := ca.Issue(pki.IssueOptions{CommonName: "sam", Roles: []string{pki.RoleReader}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13})))
	janusv1alpha1.RegisterSystemServiceServer(gs, janusv1alpha1.UnimplementedSystemServiceServer{})
	gl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(gl) }()
	defer gs.Stop()

	proxy := newConnectProxy(t, map[string]string{
		controllerName: strings.TrimPrefix(controller.URL, "https://"),
		nodeName:       gl.Addr().String(),
	})
	fixture, _ := json.Marshal(proxyFixture{
		ControllerCert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: controller.Certificate().Raw})),
		NodeCA:         string(ca.CertPEM),
		ClientCert:     string(cliCert),
		ClientKey:      string(cliKey),
	})
	file := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(file, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	child := func(mode string, env ...string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestThroughTheProxy$", "-test.v")
		cmd.Env = append(slices.DeleteFunc(os.Environ(), func(e string) bool {
			name, _, _ := strings.Cut(e, "=")
			return slices.Contains([]string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy"}, name)
		}), append(env, proxyChildEnv+"="+mode, "JANUSCTL_TEST_FIXTURE="+file)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", mode, err, out)
		}
	}
	proxyURL := "http://" + proxy.ln.Addr().String()

	// Through the proxy: the API call, login's handshakes, the node.
	child("through", "HTTPS_PROXY="+proxyURL)
	want := []string{"CONNECT " + controllerName, "CONNECT " + controllerName, "CONNECT " + nodeName}
	if got := proxy.connects(); !slices.Equal(got, want) {
		t.Errorf("the proxy saw %v, want %v", got, want)
	}

	// NO_PROXY names them: nothing through the proxy (and no name like
	// these resolves directly).
	child("direct", "HTTPS_PROXY="+proxyURL, "NO_PROXY=.janus.test")
	if got := proxy.connects(); len(got) != len(want) {
		t.Errorf("NO_PROXY: the proxy saw %v", got[len(want):])
	}

	// JANUS_NO_PROXY replaces NO_PROXY: Claude Code's sandbox sets the
	// private ranges there, where it has no route.
	child("janus-no-proxy", "HTTPS_PROXY="+proxyURL, "NO_PROXY=.janus.test", noProxyEnv+"=localhost")
	if got := proxy.connects(); len(got) != 2*len(want) {
		t.Errorf("JANUS_NO_PROXY: the proxy saw %v", got[len(want):])
	}

	// A proxy that refuses: said.
	refusing := newConnectProxy(t, map[string]string{})
	child("refused", "HTTPS_PROXY=http://"+refusing.ln.Addr().String())
}

func proxyChild(t *testing.T, mode string) {
	applyNoProxy()
	data, err := os.ReadFile(os.Getenv("JANUSCTL_TEST_FIXTURE"))
	if err != nil {
		t.Fatal(err)
	}
	var f proxyFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	c := &cliContext{Controller: controllerName, ControllerCA: f.ControllerCert}
	var out map[string]bool
	callErr := c.call("GET", "/api/cli/inventory", "t", nil, &out)
	leaf, leafErr := fetchLeaf(controllerName)

	tlsConfig, err := pki.ClientTLSConfig([]byte(f.NodeCA), []byte(f.ClientCert), []byte(f.ClientKey))
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig.ServerName = "node.janus.test"
	conn, err := dialTLS(nodeName, tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	wait := 3 * time.Second
	if mode == "through" {
		wait = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, nodeErr := janusv1alpha1.NewSystemServiceClient(conn).Version(ctx, &emptypb.Empty{})

	switch mode {
	case "through", "janus-no-proxy":
		if callErr != nil || !out["ok"] {
			t.Errorf("the Controller's API: %v", callErr)
		}
		if leafErr != nil || certFingerprint(leaf) != certFingerprint(mustParsePEM(t, f.ControllerCert)) {
			t.Errorf("the Controller's certificate: %v", leafErr)
		}
		if status.Code(nodeErr) != codes.Unimplemented {
			t.Errorf("the node: %v, want its Unimplemented", nodeErr)
		}
	case "direct", "refused":
		if callErr == nil || leafErr == nil || status.Code(nodeErr) == codes.Unimplemented {
			t.Errorf("%s: reached through no proxy: %v, %v, %v", mode, callErr, leafErr, nodeErr)
		}
		if mode == "refused" && !strings.Contains(leafErr.Error(), "refused "+controllerName+": 403") {
			t.Errorf("a refusing proxy: %v", leafErr)
		}
	}
}

func mustParsePEM(t *testing.T, s string) *x509.Certificate {
	b, _ := pem.Decode([]byte(s))
	if b == nil {
		t.Fatal("no PEM")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
