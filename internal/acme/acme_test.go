package acme

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme/acmewire"
	"github.com/swenske/Janus/internal/haproxy"
)

// TestMain doubles as a fake janus-acme (fakeACME), run through a script
// the tests point Binary at.
func TestMain(m *testing.M) {
	if os.Getenv("JANUS_ACME_FAKE") == "1" {
		fakeACME()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeACME answers a request like janus-acme would, with a certificate
// from a throwaway CA, valid 90 days. An email of fail@... makes it fail.
func fakeACME() {
	var req acmewire.Request
	res := &acmewire.Result{AccountURI: "https://ca.test/acct/1"}
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		res.Error = err.Error()
	} else if strings.HasPrefix(req.Email, "fail@") {
		res.Error = "urn:ietf:params:acme:error:unauthorized: Invalid response from http://x/.well-known/acme-challenge/t: 404"
	} else if req.Action == acmewire.ActionIssue {
		res.Certificate, res.PrivateKey = issueTestCert(req.Domains, req.KeyType)
	} else if req.Action == acmewire.ActionRenewalInfo {
		res.RenewalStart = time.Now().Add(40 * 24 * time.Hour)
		res.RenewalEnd = time.Now().Add(41 * 24 * time.Hour)
	}
	fmt.Fprintln(os.Stderr, "fake janus-acme:", req.Action)
	_ = json.NewEncoder(os.Stdout).Encode(res)
}

func issueTestCert(domains []string, keyType string) (chain, key string) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Fake CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caKey.Public(), caKey)
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := newKey(keyType)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: domains[0]}, DNSNames: domains,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca, leafKey.Public(), caKey)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(leafKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})) +
			string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

// fakeHAProxy records what the manager asks of HAProxy.
type fakeHAProxy struct {
	mu       sync.Mutex
	started  time.Time
	loaded   map[string]bool // files the running process loaded
	swapped  []string
	reloads  int
	validate func() (bool, []string)
}

func (f *fakeHAProxy) ReplaceCertFile(path string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded[path] {
		return haproxy.ErrCertNotLoaded
	}
	f.swapped = append(f.swapped, path)
	return nil
}

func (f *fakeHAProxy) Reload() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	f.started = time.Now()
	return nil
}

func (f *fakeHAProxy) reloadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reloads
}

func (f *fakeHAProxy) StartedAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started
}

func (f *fakeHAProxy) Validate([]byte) (bool, []string) {
	if f.validate != nil {
		return f.validate()
	}
	return true, nil
}

// setup points the package at a temporary node and the fake janus-acme.
func setup(t *testing.T) (*Manager, *fakeHAProxy) {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "janus-acme")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nJANUS_ACME_FAKE=1 exec "+exe+" -test.run=^$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldBin, oldDir, oldCert := Binary, Dir, CertDir
	Binary, Dir, CertDir = script, filepath.Join(dir, "config"), filepath.Join(dir, "haproxy", "acme")
	t.Cleanup(func() { Binary, Dir, CertDir = oldBin, oldDir, oldCert })
	cfgPath := filepath.Join(dir, "haproxy", "haproxy.cfg")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("global\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hp := &fakeHAProxy{loaded: map[string]bool{}}
	m := New(Options{HAProxy: hp, HAProxyConfig: cfgPath, Healthy: func() bool { return true }, Output: &testLog{t}})
	if err := m.Boot(); err != nil {
		t.Fatal(err)
	}
	return m, hp
}

type testLog struct{ t *testing.T }

func (w *testLog) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func testConfig() *janusv1alpha1.ACMEConfig {
	return &janusv1alpha1.ACMEConfig{
		Account: &janusv1alpha1.ACMEAccount{Directory: "letsencrypt-staging", Email: "ops@example.com", AcceptTerms: true},
		Certificates: []*janusv1alpha1.ACMECertificate{
			{Name: "site", Domains: []string{"Example.COM", "www.example.com"}},
			{Name: "wild", Domains: []string{"*.example.com"}, Challenge: "dns-01", DnsProvider: "gandi", KeyType: "ec384"},
		},
		DnsProviders: []*janusv1alpha1.ACMEDNSProvider{
			{Name: "gandi", Type: "gandiv5", Settings: map[string]string{"GANDIV5_PERSONAL_ACCESS_TOKEN": "secret-token"}},
		},
	}
}

func readBundle(t *testing.T, name string) *bundle {
	t.Helper()
	data, err := os.ReadFile(certPath(name))
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestThumbprintMatchesJOSE(t *testing.T) {
	ec256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ec384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsa2048, _ := rsa.GenerateKey(rand.Reader, 2048)
	for name, key := range map[string]crypto.Signer{"P-256": ec256, "P-384": ec384, "RSA": rsa2048} {
		got, err := thumbprint(key)
		if err != nil {
			t.Fatal(err)
		}
		sum, err := (&jose.JSONWebKey{Key: key.Public()}).Thumbprint(crypto.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if want := b64(sum); got != want {
			t.Errorf("%s: thumbprint %s, go-jose says %s", name, got, want)
		}
	}
}

func TestParseAccountKey(t *testing.T) {
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	sec1, _ := x509.MarshalECPrivateKey(ec)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(ec)
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	for _, tc := range []struct {
		name string
		pem  []byte
		ok   bool
	}{
		{"sec1", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1}), true},
		{"pkcs8", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), true},
		{"rsa1024", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(small)}), false},
		{"certificate", []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), false},
		{"garbage", []byte("nope"), false},
	} {
		_, err := parseAccountKey(tc.pem)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestValidate(t *testing.T) {
	good := testConfig()
	normalize(good)
	if errs := Validate(good); len(errs) > 0 {
		t.Fatalf("a good configuration refused: %v", errs)
	}
	if good.Certificates[0].Domains[0] != "example.com" || good.Certificates[0].KeyType != "ec256" || good.Certificates[0].Challenge != "http-01" {
		t.Errorf("not normalized: %v", good.Certificates[0])
	}
	for _, tc := range []struct {
		name   string
		change func(*janusv1alpha1.ACMEConfig)
		want   string
	}{
		{"wildcard over http-01", func(c *janusv1alpha1.ACMEConfig) {
			c.Certificates[1].Challenge = "http-01"
			c.Certificates[1].DnsProvider = ""
		}, "needs the dns-01"},
		{"unknown provider type", func(c *janusv1alpha1.ACMEConfig) { c.DnsProviders[0].Type = "nope" }, "unknown type"},
		{"foreign setting", func(c *janusv1alpha1.ACMEConfig) { c.DnsProviders[0].Settings["OVH_ENDPOINT"] = "x" }, "isn't one of its settings"},
		{"file setting", func(c *janusv1alpha1.ACMEConfig) { c.DnsProviders[0].Settings["GANDIV5_API_KEY_FILE"] = "/x" }, "file settings"},
		{"empty setting", func(c *janusv1alpha1.ACMEConfig) { c.DnsProviders[0].Settings["GANDIV5_TTL"] = "" }, "has no value"},
		{"missing provider", func(c *janusv1alpha1.ACMEConfig) { c.Certificates[1].DnsProvider = "other" }, "isn't one of dns_providers"},
		{"same name", func(c *janusv1alpha1.ACMEConfig) { c.Certificates[1].Name = "site" }, "used twice"},
		{"bad name", func(c *janusv1alpha1.ACMEConfig) { c.Certificates[0].Name = "../x" }, "name"},
		{"bad domain", func(c *janusv1alpha1.ACMEConfig) { c.Certificates[0].Domains = []string{"exa mple.com"} }, "isn't a domain name"},
		{"ip", func(c *janusv1alpha1.ACMEConfig) { c.Certificates[0].Domains = []string{"10.0.0.1"} }, "IP address"},
		{"single label", func(c *janusv1alpha1.ACMEConfig) { c.Certificates[0].Domains = []string{"localhost"} }, "isn't a domain name"},
		{"key type", func(c *janusv1alpha1.ACMEConfig) { c.Certificates[0].KeyType = "ed25519" }, "key_type"},
		{"http directory", func(c *janusv1alpha1.ACMEConfig) { c.Account.Directory = "http://ca.test/dir" }, "https://"},
		{"half EAB", func(c *janusv1alpha1.ACMEConfig) { c.Account.EabKeyId = "kid" }, "go together"},
		{"dns provider on http-01", func(c *janusv1alpha1.ACMEConfig) { c.Certificates[0].DnsProvider = "gandi" }, "is for the dns-01"},
	} {
		cfg := testConfig()
		tc.change(cfg)
		normalize(cfg)
		errs := Validate(cfg)
		if !slices.ContainsFunc(errs, func(e string) bool { return strings.Contains(e, tc.want) }) {
			t.Errorf("%s: errors %q, want one with %q", tc.name, errs, tc.want)
		}
	}
}

func TestSecretsRedactedAndKept(t *testing.T) {
	m, _ := setup(t)
	cfg := testConfig()
	cfg.Account.EabKeyId, cfg.Account.EabHmacKey = "kid", "hmac-secret"
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatal(errs, err)
	}
	got := m.Config()
	if v := got.DnsProviders[0].Settings["GANDIV5_PERSONAL_ACCESS_TOKEN"]; v != "" {
		t.Errorf("the token came back: %q", v)
	}
	if got.Account.EabHmacKey != "" {
		t.Error("the EAB key came back")
	}
	// Applying what came back keeps the secrets.
	if errs, err := m.Apply(got, "", false); err != nil {
		t.Fatal(errs, err)
	}
	saved, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if v := saved.DnsProviders[0].Settings["GANDIV5_PERSONAL_ACCESS_TOKEN"]; v != "secret-token" {
		t.Errorf("token %q after a round trip", v)
	}
	if saved.Account.EabHmacKey != "hmac-secret" {
		t.Errorf("EAB key %q after a round trip", saved.Account.EabHmacKey)
	}
	if fi, err := os.Stat(filepath.Join(Dir, configFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("configuration file mode: %v %v", fi.Mode(), err)
	}
}

// TestObtainDeployRenew follows a certificate through its life: a
// stand-in at first, the CA's certificate swapped into HAProxy, not
// renewed while valid, renewed when asked, removal refused while
// haproxy.cfg needs it.
func TestObtainDeployRenew(t *testing.T) {
	m, hp := setup(t)
	if m.Thumbprint() == "" {
		t.Fatal("no account key created at boot")
	}
	cfg := testConfig()
	cfg.Certificates = cfg.Certificates[:1]
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatal(errs, err)
	}
	if b := readBundle(t, "site"); !b.Placeholder || !sameNames(b.Leaf, []string{"example.com", "www.example.com"}) {
		t.Fatalf("no stand-in for the new certificate: %+v", b.Leaf.Subject)
	}
	if st := m.Status(); st.Certificates[0].State != "pending" {
		t.Errorf("state %q before it's obtained, want pending", st.Certificates[0].State)
	}

	// HAProxy started before the stand-in was written: the first
	// certificate needs a reload.
	hp.started = time.Now().Add(-time.Hour)
	m.pass()
	b := readBundle(t, "site")
	if b.Placeholder || b.Leaf.Issuer.CommonName != "Fake CA" {
		t.Fatalf("not obtained: issuer %q", b.Leaf.Issuer.CommonName)
	}
	if hp.reloads != 1 {
		t.Errorf("%d reloads for a file HAProxy couldn't have loaded, want 1", hp.reloads)
	}
	st := m.Status()
	if c := st.Certificates[0]; c.State != "valid" || c.Issuer != "Fake CA" || c.LastSuccessUnix == 0 || c.RenewAtUnix == 0 {
		t.Errorf("status after obtaining it: %+v", c)
	}
	if st.AccountUri != "https://ca.test/acct/1" {
		t.Errorf("account %q", st.AccountUri)
	}

	// Valid: the next pass only asks for renewal information.
	serial := serialHex(b.Leaf)
	m.pass()
	if got := serialHex(readBundle(t, "site").Leaf); got != serial {
		t.Error("renewed while valid")
	}
	m.mu.Lock()
	ari := m.st.Certs["site"].ARIRenewAt
	m.mu.Unlock()
	if ari.Before(time.Now().Add(39*24*time.Hour)) || ari.After(time.Now().Add(42*24*time.Hour)) {
		t.Errorf("renewal time from the CA's window: %v", ari)
	}

	// Asked for: renewed, swapped in place this time.
	hp.loaded[certPath("site")] = true
	if _, err := m.Renew(nil); err != nil {
		t.Fatal(err)
	}
	m.pass()
	if serialHex(readBundle(t, "site").Leaf) == serial {
		t.Error("not renewed when asked")
	}
	if len(hp.swapped) != 1 || hp.reloads != 1 {
		t.Errorf("swapped %v, %d reloads - want one swap, no new reload", hp.swapped, hp.reloads)
	}
	if _, err := m.Renew([]string{"nope"}); err == nil {
		t.Error("renewing an unknown certificate accepted")
	}

	// Removal: refused while haproxy.cfg doesn't load without the file.
	hp.validate = func() (bool, []string) {
		if _, err := os.Stat(certPath("site")); err != nil {
			return false, []string{"unable to load " + certPath("site")}
		}
		return true, nil
	}
	empty := testConfig()
	empty.Certificates = nil
	if errs, err := m.Apply(empty, "", false); err == nil || !strings.Contains(strings.Join(errs, " "), "unable to load") {
		t.Fatalf("removal accepted while in use: %v %v", errs, err)
	}
	if _, err := os.Stat(certPath("site")); err != nil {
		t.Fatal("the file isn't back after a refused removal")
	}
	hp.validate = nil
	if errs, err := m.Apply(empty, "", false); err != nil {
		t.Fatal(errs, err)
	}
	if _, err := os.Stat(certPath("site")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the removed certificate's file is still there")
	}
}

func TestFailureBacksOff(t *testing.T) {
	m, _ := setup(t)
	cfg := testConfig()
	cfg.Certificates = cfg.Certificates[:1]
	cfg.Account.Email = "fail@example.com"
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatal(errs, err)
	}
	m.pass()
	c := m.Status().Certificates[0]
	if c.Failures != 1 || !strings.Contains(c.LastError, "404") || c.NextAttemptUnix < time.Now().Add(4*time.Minute).Unix() {
		t.Fatalf("after a failure: %+v", c)
	}
	m.pass() // within the backoff: no new attempt
	if c := m.Status().Certificates[0]; c.Failures != 1 {
		t.Errorf("attempted again within the backoff: %d failures", c.Failures)
	}
	m.HAProxyConfigChanged() // may be what answers the challenges now
	m.pass()
	if c := m.Status().Certificates[0]; c.Failures != 2 {
		t.Errorf("not retried after HAProxy's configuration changed: %d failures", c.Failures)
	}
	if got := backoff(1); got != 5*time.Minute {
		t.Errorf("backoff(1) = %v", got)
	}
	if got := backoff(20); got != 24*time.Hour {
		t.Errorf("backoff(20) = %v", got)
	}
}

func TestHTTP01WaitsForHAProxy(t *testing.T) {
	m, _ := setup(t)
	healthy := false
	m.opts.Healthy = func() bool { return healthy }
	cfg := testConfig()
	cfg.Certificates = cfg.Certificates[:1]
	if errs, err := m.Apply(cfg, "", false); err != nil {
		t.Fatal(errs, err)
	}
	if next := m.pass(); time.Until(next) > time.Minute {
		t.Errorf("next look in %v while waiting for HAProxy", time.Until(next))
	}
	if !readBundle(t, "site").Placeholder {
		t.Fatal("obtained while HAProxy doesn't answer")
	}
	healthy = true
	m.pass()
	if readBundle(t, "site").Placeholder {
		t.Error("not obtained once HAProxy answers")
	}
}

func TestImportedAccountKey(t *testing.T) {
	m, hp := setup(t)
	hp.started = time.Now()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(key)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	want, _ := thumbprint(key)
	if errs, err := m.Apply(testConfig(), keyPEM, false); err != nil {
		t.Fatal(errs, err)
	}
	if m.Thumbprint() != want {
		t.Errorf("thumbprint %s, want the imported key's %s", m.Thumbprint(), want)
	}
	if env := m.Env(); len(env) != 1 || env[0] != ThumbprintEnv+"="+want {
		t.Errorf("HAProxy's environment %v", env)
	}
	// HAProxy is reloaded for its environment (in the background).
	deadline := time.Now().Add(2 * time.Second)
	for hp.reloadCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hp.reloadCount() == 0 {
		t.Error("HAProxy not reloaded for the new thumbprint")
	}
	if errs, _ := m.Apply(testConfig(), "not a key", true); len(errs) == 0 {
		t.Error("a bad account key accepted")
	}
}
