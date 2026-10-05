package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/pki"
)

func TestSelectNodes(t *testing.T) {
	ctx := &cliContext{Nodes: []ctxNode{{ID: "a1", Name: "edge-1", Fleet: true}, {ID: "b2", Name: "edge-2", Fleet: true}, {ID: "c3", Name: "old", Fleet: false}}}
	names := func(nodes []ctxNode) string {
		var out []string
		for _, n := range nodes {
			out = append(out, n.Name)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		names string
		all   bool
		want  string
		err   string
	}{
		{"edge-1", false, "edge-1", ""},
		{"b2, edge-1", false, "edge-2,edge-1", ""},
		{"", true, "edge-1,edge-2", ""},
		{"", false, "", "which node?"},
		{"old", false, "", "doesn't trust the fleet"},
		{"nope", false, "", `no node "nope"`},
	} {
		got, err := selectNodes(ctx, c.names, c.all)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: %v, want %q", c.names, err, c.err)
			}
			continue
		}
		if err != nil || names(got) != c.want {
			t.Errorf("%q all=%v: %s %v, want %s", c.names, c.all, names(got), err, c.want)
		}
	}
	one := &cliContext{Nodes: []ctxNode{{ID: "a1", Name: "edge-1", Fleet: true}, {ID: "c3", Name: "old"}}}
	if got, err := selectNodes(one, "", false); err != nil || names(got) != "edge-1" {
		t.Errorf("the only node of the fleet: %s %v", names(got), err)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{"janus.example": "janus.example:443", "https://janus.example:8443/": "janus.example:8443", "10.0.0.5": "10.0.0.5:443", "[2001:db8::1]": "[2001:db8::1]:443"} {
		if got := normalizeController(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := normalizeFingerprint("SHA256:AB:cd:01"); got != "abcd01" {
		t.Errorf("fingerprint: %q", got)
	}
	if got := colonHex("abcd01"); got != "AB:CD:01" {
		t.Errorf("colonHex: %q", got)
	}
}

// TestControllerPin: the Controller is trusted for its pinned certificate
// exactly, or a chain to a given CA - nothing else.
func TestControllerPin(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	leaf := srv.Certificate()
	pinned := &cliContext{Controller: addr, ControllerCA: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}))}
	var out map[string]bool
	if err := pinned.call("GET", "/", "t", nil, &out); err != nil || !out["ok"] {
		t.Fatalf("the pinned certificate: %v", err)
	}
	other, _ := pki.NewCA("another")
	stranger := &cliContext{Controller: addr, ControllerCA: string(other.CertPEM)}
	if err := stranger.call("GET", "/", "t", nil, &out); err == nil {
		t.Error("trusted with another CA")
	}
	got, err := fetchLeaf(addr)
	if err != nil || certFingerprint(got) != certFingerprint(leaf) {
		t.Errorf("fetchLeaf: %v", err)
	}
}

// TestConfigRoundTrip: contexts saved 0600 under $JANUSCONFIG, their files
// next to it.
func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configEnv, filepath.Join(dir, "janusctl.json"))
	cfg, err := loadConfig()
	if err != nil || len(cfg.Contexts) != 0 {
		t.Fatalf("a fresh config: %+v %v", cfg, err)
	}
	cfg.Contexts["home"] = &cliContext{Controller: "c:443", User: "sam"}
	cfg.Current = "home"
	if err := cfg.save(); err != nil {
		t.Fatal(err)
	}
	again, _ := loadConfig()
	if name, c := currentContext(again, ""); name != "home" || c == nil || c.User != "sam" {
		t.Errorf("reloaded: %q %+v", name, c)
	}
	if contextDir("home") != filepath.Join(dir, "home") {
		t.Errorf("context dir %s", contextDir("home"))
	}
}

// TestNodeTLS: a node is checked against its own CA, with the context's
// certificate.
func TestNodeTLS(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configEnv, filepath.Join(dir, "janusctl.json"))
	ca, _ := pki.NewCA("node")
	certPEM, keyPEM, _ := ca.Issue(pki.IssueOptions{CommonName: "sam", Roles: []string{pki.RoleAdmin}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err := writeFileAtomic(filepath.Join(dir, "home", "cert.pem"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "home", "key.pem"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := nodeTLS("home", ctxNode{Name: "edge-1", CAPEM: string(ca.CertPEM)})
	if err != nil || len(cfg.Certificates) != 1 || cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("nodeTLS: %v", err)
	}
	if _, err := nodeTLS("home", ctxNode{Name: "edge-1"}); err == nil {
		t.Error("a node without a CA")
	}
}
