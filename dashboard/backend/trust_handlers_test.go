package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
)

// do makes a request with cookies, answering the response (its cookies
// included).
func (a *authApp) do(t *testing.T, method, path string, body any, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	r := httptest.NewRequest(method, path, &b)
	r.RemoteAddr = "192.0.2.7:4242"
	r.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0")
	for _, c := range cookies {
		if c != nil {
			r.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, r)
	return rec.Result()
}

func cookieNamed(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestTrustedBrowserSignIn: a sign-in that gives its code and asks to
// trust its browser gets a cookie that skips the code next time - for
// that account, until a password change; adding an SSH key from such a
// sign-in asks for the code again.
func TestTrustedBrowserSignIn(t *testing.T) {
	a := newMFAApp(t)
	root := a.login(t, "root", "root-password")
	_, body := a.req(t, "POST", "/api/auth/mfa/totp/setup", root, nil)
	var setup struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal([]byte(body), &setup)
	step := 0
	nextCode := func() string {
		code, _ := auth.TOTPCode(setup.Secret, time.Now().Add(time.Duration(step)*30*time.Second))
		step++
		return code
	}
	// A code is good once and only near its time: two of them here
	// (setting the app up, confirming), recovery codes for the rest.
	st, body := a.req(t, "POST", "/api/auth/mfa/totp/enable", root, map[string]string{"code": nextCode()})
	var rc struct {
		Codes []string `json:"recovery_codes"`
	}
	if _ = json.Unmarshal([]byte(body), &rc); st != http.StatusOK || len(rc.Codes) != 10 {
		t.Fatal(st, body)
	}
	recovery := func() string {
		c := rc.Codes[0]
		rc.Codes = rc.Codes[1:]
		return c
	}
	signIn := func(trust *http.Cookie) (*http.Cookie, *http.Response) {
		resp := a.do(t, "POST", "/api/auth/login", map[string]string{"name": "root", "password": "root-password"}, trust)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("login: %d", resp.StatusCode)
		}
		return cookieNamed(resp, sessionCookieName), resp
	}

	// A code without asking: no cookie.
	sess, _ := signIn(nil)
	resp := a.do(t, "POST", "/api/auth/mfa/recovery", map[string]any{"code": recovery()}, sess)
	if resp.StatusCode != http.StatusNoContent || cookieNamed(resp, trustCookieName) != nil {
		t.Fatalf("a code without trust: %d, cookies %v", resp.StatusCode, resp.Cookies())
	}

	// Asking: the cookie, scoped to the sign-in, as long as the policy.
	sess, _ = signIn(nil)
	resp = a.do(t, "POST", "/api/auth/mfa/recovery", map[string]any{"code": recovery(), "trust": true}, sess)
	trust := cookieNamed(resp, trustCookieName)
	if resp.StatusCode != http.StatusNoContent || trust == nil {
		t.Fatalf("a code with trust: %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
	if trust.Path != "/api/auth/" || !trust.HttpOnly || !trust.Secure || trust.SameSite != http.SameSiteStrictMode || trust.MaxAge < 12*3600-60 || trust.MaxAge > 12*3600 {
		t.Errorf("trust cookie = %+v", trust)
	}

	// The next sign-in needs no code - the password still checked.
	if resp := a.do(t, "POST", "/api/auth/login", map[string]string{"name": "root", "password": "wrong"}, trust); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong password with a trusted browser: %d", resp.StatusCode)
	}
	sess, _ = signIn(trust)
	if got := a.needs(t, sess.Value); got != "" {
		t.Fatalf("a trusted browser's sign-in needs %q", got)
	}
	_, body = a.req(t, "GET", "/api/auth/status", sess.Value, nil)
	if !strings.Contains(body, `"trusted":true`) || !strings.Contains(body, `"trust_hours":12`) {
		t.Errorf("status of a trusted sign-in: %s", body)
	}
	if code, _ := a.req(t, "GET", "/api/users", sess.Value, nil); code != http.StatusOK {
		t.Errorf("a trusted sign-in reads the accounts: %d", code)
	}

	// Its list, this browser marked.
	resp = a.do(t, "GET", "/api/auth/trusted-browsers", nil, sess, trust)
	var list []trustedView
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || !list[0].Current || list[0].Label != "Firefox on Linux" || list[0].Client != "192.0.2.7" {
		t.Errorf("trusted browsers: %+v", list)
	}

	// An SSH key needs the code given now: confirmed, it's added.
	key := map[string]any{"name": "laptop", "public_key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test"}
	if code, body := a.req(t, "POST", "/api/auth/ssh-keys", sess.Value, key); code != http.StatusForbidden || !strings.Contains(body, "trusted browser") {
		t.Errorf("an SSH key from a trusted sign-in: %d %s", code, body)
	}
	if code, body := a.req(t, "POST", "/api/auth/mfa/totp", sess.Value, map[string]string{"code": nextCode()}); code != http.StatusNoContent {
		t.Fatalf("confirming the second factor: %d %s", code, body)
	}
	if code, body := a.req(t, "POST", "/api/auth/ssh-keys", sess.Value, key); code != http.StatusCreated && code != http.StatusOK {
		t.Errorf("an SSH key after confirming: %d %s", code, body)
	}

	// Another account isn't helped by root's browser.
	resp = a.do(t, "POST", "/api/auth/login", map[string]string{"name": "olga", "password": "olga-password"}, trust)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal(resp.StatusCode)
	}

	// The audit says how the sign-in went.
	_, body = a.req(t, "GET", "/api/audit", sess.Value, nil)
	if !strings.Contains(body, `"via":"trusted browser"`) {
		t.Errorf("the audit doesn't show the trusted sign-in: %s", body)
	}

	// A password change forgets every browser.
	if code, _ := a.req(t, "POST", "/api/auth/password", sess.Value, map[string]string{"current_password": "root-password", "new_password": "root-password-2"}); code != http.StatusNoContent {
		t.Fatal(code)
	}
	resp = a.do(t, "POST", "/api/auth/login", map[string]string{"name": "root", "password": "root-password-2"}, trust)
	if got := a.needs(t, cookieNamed(resp, sessionCookieName).Value); got != "mfa" {
		t.Errorf("after a password change, a trusted browser's sign-in needs %q", got)
	}

	// Forgetting this browser takes its cookie away.
	sess = cookieNamed(resp, sessionCookieName)
	resp = a.do(t, "POST", "/api/auth/mfa/recovery", map[string]any{"code": recovery(), "trust": true}, sess)
	trust = cookieNamed(resp, trustCookieName)
	resp = a.do(t, "GET", "/api/auth/trusted-browsers", nil, sess)
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 {
		t.Fatalf("trusted browsers after trusting again: %+v", list)
	}
	resp = a.do(t, "DELETE", "/api/auth/trusted-browsers", nil, sess, trust)
	if c := cookieNamed(resp, trustCookieName); resp.StatusCode != http.StatusNoContent || c == nil || c.MaxAge >= 0 {
		t.Errorf("forgetting every browser: %d, cookie %+v", resp.StatusCode, c)
	}
	resp = a.do(t, "GET", "/api/auth/trusted-browsers", nil, sess)
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 0 {
		t.Errorf("trusted browsers after forgetting them all: %+v", list)
	}
}

// TestTrustPolicyOff: the policy at 0 - asking to trust does nothing.
func TestTrustPolicyOff(t *testing.T) {
	a := newMFAApp(t)
	root := a.login(t, "root", "root-password")
	_, body := a.req(t, "POST", "/api/auth/mfa/totp/setup", root, nil)
	var setup struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal([]byte(body), &setup)
	code, _ := auth.TOTPCode(setup.Secret, time.Now())
	a.req(t, "POST", "/api/auth/mfa/totp/enable", root, map[string]string{"code": code})
	if code, body := a.req(t, "PUT", "/api/settings", root, map[string]any{"trust_browser_hours": 0}); code != http.StatusOK || !strings.Contains(body, `"trust_browser_hours":0`) {
		t.Fatalf("settings: %d %s", code, body)
	}
	if code, body := a.req(t, "PUT", "/api/settings", root, map[string]any{"trust_browser_hours": 1000}); code != http.StatusBadRequest {
		t.Errorf("1000 hours: %d %s", code, body)
	}
	sess := cookieNamed(a.do(t, "POST", "/api/auth/login", map[string]string{"name": "root", "password": "root-password"}), sessionCookieName)
	next, _ := auth.TOTPCode(setup.Secret, time.Now().Add(30*time.Second))
	resp := a.do(t, "POST", "/api/auth/mfa/totp", map[string]any{"code": next, "trust": true}, sess)
	if resp.StatusCode != http.StatusNoContent || cookieNamed(resp, trustCookieName) != nil {
		t.Errorf("trust with the policy at 0: %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
}

func TestBrowserLabel(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0": "Edge on Windows",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15":          "Safari on macOS",
		"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36":                    "Chrome on Android",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile Safari/604.1": "Safari on iOS",
		"curl/8.5.0": "a browser",
	} {
		if got := browserLabel(ua); got != want {
			t.Errorf("browserLabel(%q) = %q, want %q", ua, got, want)
		}
	}
}
