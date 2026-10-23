package acme

import (
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme/acmewire"
)

func proxyPath(url, user, password string) *janusv1alpha1.ACMEPath {
	return &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY, ProxyUrl: url, ProxyUsername: user, ProxyPassword: password}
}

func relayPath(url, ca string) *janusv1alpha1.ACMEPath {
	return &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_CONTROLLER_RELAY, ProxyUrl: url, ProxyCa: ca}
}

func TestValidatePath(t *testing.T) {
	ca := string(issueCAPEM())
	for _, c := range []struct {
		name string
		p    *janusv1alpha1.ACMEPath
		ok   bool
	}{
		{"none", nil, true},
		{"direct", &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_DIRECT}, true},
		{"proxy", proxyPath("http://proxy.example:3128", "", ""), true},
		{"https proxy with its CA", &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY, ProxyUrl: "https://proxy.example", ProxyCa: ca}, true},
		{"credentials", proxyPath("http://proxy.example:3128", "ops", "s3cret"), true},
		{"no URL", proxyPath("", "", ""), false},
		{"socks", proxyPath("socks5://proxy.example:1080", "", ""), false},
		{"credentials in the URL", proxyPath("http://ops:s3cret@proxy.example:3128", "", ""), false},
		{"a path", proxyPath("http://proxy.example:3128/x", "", ""), false},
		{"a CA for plain http", &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY, ProxyUrl: "http://proxy.example", ProxyCa: ca}, false},
		{"a CA that isn't one", &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY, ProxyUrl: "https://proxy.example", ProxyCa: "nope"}, false},
		{"a password without its user", proxyPath("http://proxy.example:3128", "", "s3cret"), false},
		{"an unknown mode", &janusv1alpha1.ACMEPath{Mode: 99}, false},
		{"the Controller's relay", relayPath("https://controller.example:8444", ca), true},
		{"the relay without the Controller's identity", relayPath("https://controller.example:8444", ""), false},
		{"the relay over plain http", relayPath("http://controller.example:8444", ca), false},
		{"the relay with a username", &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_CONTROLLER_RELAY, ProxyUrl: "https://controller.example:8444", ProxyCa: ca, ProxyUsername: "ops"}, false},
	} {
		if errs := validatePath(normalizePath(c.p)); (len(errs) == 0) != c.ok {
			t.Errorf("%s: %v", c.name, errs)
		}
	}
	if got := proxyURL(proxyPath("http://proxy.example:3128", "ops", "s3:cret")); got != "http://ops:s3%3Acret@proxy.example:3128" {
		t.Errorf("proxyURL = %q", got)
	}
}

// TestPathSavedApart: the path goes to acme/path.json - an older janusd,
// strict with letsencrypt.json, still reads that one -, its password
// never comes back and is kept when left empty; a direct path removes
// the file.
func TestPathSavedApart(t *testing.T) {
	m, _ := setup(t)
	cfg := testConfig()
	cfg.Path = proxyPath(" http://proxy.example:3128 ", "ops", "s3cret")
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatalf("apply: %v %v", errs, err)
	}
	main, _ := os.ReadFile(filepath.Join(Dir, configFile))
	if strings.Contains(string(main), "proxy") || strings.Contains(string(main), "path") {
		t.Errorf("letsencrypt.json holds the path:\n%s", main)
	}
	saved, _ := os.ReadFile(filepath.Join(Dir, pathFile))
	if !strings.Contains(string(saved), `"proxy_url": "http://proxy.example:3128"`) || !strings.Contains(string(saved), "s3cret") {
		t.Errorf("path.json:\n%s", saved)
	}
	got := m.Config().GetPath()
	if got.GetProxyUrl() != "http://proxy.example:3128" || got.GetProxyUsername() != "ops" || got.GetProxyPassword() != "" {
		t.Errorf("the configuration read back: %v", got)
	}
	if st := m.Status(); st.PathMode != janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY || st.ProxyUrl != "http://proxy.example:3128" {
		t.Errorf("status: %v %q", st.PathMode, st.ProxyUrl)
	}

	// Applied again as read back: the password is kept.
	again := proto.Clone(m.Config()).(*janusv1alpha1.ACMEConfig)
	if errs, err := m.Apply(again, "", false); err != nil {
		t.Fatalf("apply again: %v %v", errs, err)
	}
	if res := m.CheckPath(nil, "", ""); !res.Ok || !strings.Contains(res.TermsUrl, "proxy=http://ops:s3cret@proxy.example:3128") || res.Via != "through the proxy http://proxy.example:3128" {
		t.Errorf("check through the saved path: %+v", res)
	}
	// Another janusd reads it at boot.
	m2 := New(Options{HAProxy: &fakeHAProxy{loaded: map[string]bool{}}, HAProxyConfig: m.opts.HAProxyConfig, Healthy: func() bool { return true }, Output: &testLog{t}})
	if err := m2.Boot(); err != nil {
		t.Fatal(err)
	}
	if p := m2.cfg.GetPath(); p.GetProxyPassword() != "s3cret" || p.GetProxyUrl() != "http://proxy.example:3128" {
		t.Errorf("the path after a restart: %v", p)
	}

	// Directly again: no file left.
	direct := proto.Clone(m.Config()).(*janusv1alpha1.ACMEConfig)
	direct.Path = nil
	if errs, err := m.Apply(direct, "", false); err != nil {
		t.Fatalf("apply direct: %v %v", errs, err)
	}
	if _, err := os.Stat(filepath.Join(Dir, pathFile)); !os.IsNotExist(err) {
		t.Errorf("path.json left for a direct path: %v", err)
	}
	if st := m.Status(); st.PathMode != janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_DIRECT {
		t.Errorf("status after going direct: %v", st.PathMode)
	}
}

// TestCheckPath: a path given to test, its password the saved one when
// left empty; a refused path or a proxy that doesn't answer says why.
func TestCheckPath(t *testing.T) {
	m, _ := setup(t)
	cfg := testConfig()
	cfg.Path = proxyPath("http://proxy.example:3128", "ops", "s3cret")
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatalf("apply: %v %v", errs, err)
	}
	if res := m.CheckPath(proxyPath("http://proxy.example:3128", "ops", ""), "", ""); !res.Ok || !strings.Contains(res.TermsUrl, "ops:s3cret@") {
		t.Errorf("the saved password: %+v", res)
	}
	if res := m.CheckPath(&janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_DIRECT}, "https://acme.example/dir", ""); !res.Ok || res.Via != "directly" || res.Directory != "https://acme.example/dir" {
		t.Errorf("directly: %+v", res)
	}
	if res := m.CheckPath(proxyPath("http://unreachable.example:3128", "", ""), "", ""); res.Ok || !strings.Contains(res.Error, "proxyconnect") {
		t.Errorf("a proxy that doesn't answer: %+v", res)
	}
	if res := m.CheckPath(proxyPath("ftp://proxy.example", "", ""), "", ""); res.Ok || !strings.Contains(res.Error, "http:// or https://") {
		t.Errorf("a refused path: %+v", res)
	}
}

// TestUnknownFieldsTolerated: what a newer janusd saved in
// letsencrypt.json still reads.
func TestUnknownFieldsTolerated(t *testing.T) {
	setup(t)
	data := `{"account": {"directory": "letsencrypt-staging", "accept_terms": true, "from_the_future": 1}, "certificates": [], "later": {"x": 1}}`
	if err := os.WriteFile(filepath.Join(Dir, configFile), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil || cfg.GetAccount().GetDirectory() != "letsencrypt-staging" {
		t.Fatalf("loadConfig: %v %v", cfg, err)
	}
}

// issueCAPEM is a CA certificate, PEM: the fake CA's, from a test chain.
func issueCAPEM() []byte {
	chain, _ := issueTestCert([]string{"proxy.example"}, "ec256")
	_, rest := pem.Decode([]byte(chain))
	block, _ := pem.Decode(rest)
	return pem.EncodeToMemory(block)
}

// TestPathChangeRetries: a certificate in its backoff - the proxy
// refused it - is tried again as soon as the path changes.
func TestPathChangeRetries(t *testing.T) {
	m, _ := setup(t)
	cfg := testConfig()
	cfg.Path = proxyPath("http://proxy.example:3128", "ops", "wrong")
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatalf("apply: %v %v", errs, err)
	}
	m.mu.Lock()
	m.st.Certs["site"] = &certState{Failures: 2, LastError: "Proxy Authentication Required", NextAttempt: time.Now().Add(time.Hour)}
	m.mu.Unlock()
	fixed := proto.Clone(m.Config()).(*janusv1alpha1.ACMEConfig)
	fixed.Path.ProxyPassword = "right"
	if errs, err := m.Apply(fixed, "", false); err != nil {
		t.Fatalf("apply the fix: %v %v", errs, err)
	}
	m.mu.Lock()
	next := m.st.Certs["site"].NextAttempt
	m.mu.Unlock()
	if !next.IsZero() {
		t.Errorf("still waiting until %s after the path changed", next)
	}
}

// TestRelayPath: through the Controller's relay, each request carries a
// certificate of the node's CA - none until janusd gives the Manager
// how to make one; a proxy's saved password never follows into it.
func TestRelayPath(t *testing.T) {
	m, _ := setup(t)
	ca := string(issueCAPEM())
	cfg := testConfig()
	cfg.Path = proxyPath("https://controller.example:8444", "ops", "s3cret")
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatalf("apply: %v %v", errs, err)
	}
	relay := relayPath("https://controller.example:8444", ca)
	if res := m.CheckPath(relay, "", ""); res.Ok || !strings.Contains(res.Error, "not loaded yet") {
		t.Errorf("before the node's CA: %+v", res)
	}
	made := 0
	m.SetRelayIdentity(func() ([]byte, []byte, error) {
		made++
		return []byte("-----BEGIN CERTIFICATE-----\n"), []byte("-----BEGIN EC PRIVATE KEY-----\n"), nil
	})
	res := m.CheckPath(relay, "", "")
	if !res.Ok || !strings.Contains(res.TermsUrl, "proxy=https://controller.example:8444&client-cert=CERTIFICATE") || strings.Contains(res.TermsUrl, "s3cret") {
		t.Errorf("through the relay: %+v", res)
	}
	if res.Via != "through the Controller's relay https://controller.example:8444" {
		t.Errorf("via %q", res.Via)
	}
	if res := m.CheckPath(proxyPath("http://proxy.example:3128", "", ""), "", ""); strings.Contains(res.TermsUrl, "client-cert") {
		t.Errorf("a plain proxy got the node's certificate: %+v", res)
	}
	if made != 1 {
		t.Errorf("%d certificates made, want 1 - for the relay only", made)
	}
	cfg = proto.Clone(m.Config()).(*janusv1alpha1.ACMEConfig)
	cfg.Path = relay
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatalf("apply the relay: %v %v", errs, err)
	}
	if st := m.Status(); st.PathMode != janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_CONTROLLER_RELAY || st.ProxyUrl != "https://controller.example:8444" {
		t.Errorf("status: %v %q", st.PathMode, st.ProxyUrl)
	}
	saved, _ := os.ReadFile(filepath.Join(Dir, pathFile))
	if strings.Contains(string(saved), "s3cret") || strings.Contains(string(saved), "ops") {
		t.Errorf("the proxy's credentials followed into the relay's path:\n%s", saved)
	}
}

// TestDNSProviderAPIs: every DNS provider janus-acme has says where its
// API is for the relay - or that its settings say, or that it can't go
// through it.
func TestDNSProviderAPIs(t *testing.T) {
	for p := range acmewire.DNSProviders {
		hosts, ok := DNSProviderAPIs[p]
		if !ok {
			t.Errorf("%s: no entry in DNSProviderAPIs", p)
		}
		if _, never := NotThroughRelay[p]; never && len(hosts) > 0 {
			t.Errorf("%s: hosts listed for a provider the relay can't carry", p)
		}
	}
	for p := range DNSProviderAPIs {
		if _, ok := acmewire.DNSProviders[p]; !ok {
			t.Errorf("%s: in DNSProviderAPIs, not a provider janus-acme has", p)
		}
	}
	hosts := RelayHosts()
	for _, want := range []string{"acme-v02.api.letsencrypt.org", "acme-staging-v02.api.letsencrypt.org", "api.cloudflare.com"} {
		if !slices.Contains(hosts, want) {
			t.Errorf("RelayHosts lacks %s: %v", want, hosts)
		}
	}
}
