package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestCLIBrowserLogin: the page approves janusctl's key; the code is good
// once, for that key only.
func TestCLIBrowserLogin(t *testing.T) {
	a := newAuthApp(t)
	withReadyFleet(t, a.app)
	olga := a.login(t, "olga", "olga-password")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	fp, _ := keyFingerprint(&key.PublicKey)
	csr := csrFor(t, key)

	if code, _ := a.req(t, "POST", "/api/cli/grant", olga, map[string]string{"key_fingerprint": fp, "role": "admin"}); code != http.StatusForbidden {
		t.Errorf("an operator approving an admin certificate: %d", code)
	}
	if code, _ := a.req(t, "POST", "/api/cli/grant", "", map[string]string{"key_fingerprint": fp}); code != http.StatusUnauthorized {
		t.Errorf("a grant signed out: %d", code)
	}
	grantCode := func(role string) string {
		t.Helper()
		_, body := a.req(t, "POST", "/api/cli/grant", olga, map[string]string{"key_fingerprint": fp, "role": role})
		var g struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(body), &g)
		return g.Code
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if code, _ := a.req(t, "POST", "/api/cli/exchange", "", map[string]string{"code": grantCode(""), "csr_pem": csrFor(t, other)}); code != http.StatusUnauthorized {
		t.Errorf("the code with another key: %d", code)
	}
	g := grantCode("reader")
	code, out := a.req(t, "POST", "/api/cli/exchange", "", map[string]string{"code": g, "csr_pem": csr})
	var c cliCertificate
	if _ = json.Unmarshal([]byte(out), &c); code != http.StatusOK || c.User != "olga" || c.Role != "os:reader" {
		t.Fatalf("exchange: %d %s", code, out)
	}
	if code, _ := a.req(t, "POST", "/api/cli/exchange", "", map[string]string{"code": g, "csr_pem": csr}); code != http.StatusUnauthorized {
		t.Errorf("the code again: %d", code)
	}
}

// TestCLIDeviceLogin: a sign-in from a machine without a browser - its
// code entered on the page, approved, the certificate once; or denied.
func TestCLIDeviceLogin(t *testing.T) {
	a := newAuthApp(t)
	withReadyFleet(t, a.app)
	root := a.login(t, "root", "root-password")
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	start := func() (string, string) {
		t.Helper()
		_, body := a.req(t, "POST", "/api/cli/device", "", map[string]string{"csr_pem": csrFor(t, key)})
		var d struct {
			DeviceCode string `json:"device_code"`
			UserCode   string `json:"user_code"`
		}
		if err := json.Unmarshal([]byte(body), &d); err != nil || len(d.UserCode) != 9 {
			t.Fatalf("device: %s", body)
		}
		return d.DeviceCode, d.UserCode
	}
	poll := func(dc string) (int, string) {
		t.Helper()
		return a.req(t, "POST", "/api/cli/device/token", "", map[string]string{"device_code": dc})
	}
	dc, uc := start()
	if code, _ := poll(dc); code != http.StatusPreconditionRequired {
		t.Errorf("pending: %d", code)
	}
	fp, _ := keyFingerprint(&key.PublicKey)
	if code, body := a.req(t, "GET", "/api/cli/device/"+uc, root, nil); code != http.StatusOK || !strings.Contains(body, fp) {
		t.Errorf("show: %d %s", code, body)
	}
	// Typed loosely: lowercase, without its dash.
	if code, _ := a.req(t, "POST", "/api/cli/device/"+strings.ToLower(uc[:4]+uc[5:])+"/approve", root, map[string]string{"role": "operator"}); code != http.StatusNoContent {
		t.Fatalf("approve: %d", code)
	}
	code, out := poll(dc)
	var c cliCertificate
	if _ = json.Unmarshal([]byte(out), &c); code != http.StatusOK || c.User != "root" || c.Role != "os:operator" {
		t.Fatalf("token: %d %s", code, out)
	}
	if code, _ := poll(dc); code != http.StatusGone {
		t.Errorf("the certificate twice: %d", code)
	}

	dc, uc = start()
	if code, _ := a.req(t, "POST", "/api/cli/device/"+uc+"/deny", root, nil); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if code, _ := poll(dc); code != http.StatusForbidden {
		t.Errorf("denied: %d", code)
	}
	if code, _ := a.req(t, "GET", "/api/cli/device/WXYZ-WXYZ", root, nil); code != http.StatusNotFound {
		t.Errorf("an unknown code: %d", code)
	}
	entries, _ := a.audit.Recent(100, nil)
	for _, e := range entries {
		if e.Path == "/api/cli/device/token" && e.Status == http.StatusPreconditionRequired {
			t.Error("a waiting poll in the audit")
		}
	}
}
