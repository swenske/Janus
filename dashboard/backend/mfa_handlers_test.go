package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// newMFAApp is an app whose admins must have a second factor: root
// (admin) and olga (operator).
func newMFAApp(t *testing.T) *authApp {
	t.Helper()
	dir := t.TempDir()
	st, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := secrets.LoadOrCreate("", dir)
	if err != nil {
		t.Fatal(err)
	}
	st.SetSealer(key)
	if _, err := st.Setup("root", "root-password", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser("olga", auth.Operator, "given-password"); err != nil {
		t.Fatal(err)
	}
	if err := st.ChangePassword("olga", "given-password", "olga-password"); err != nil {
		t.Fatal(err)
	}
	tokens, _ := auth.OpenTokens(dir)
	log, _ := audit.Open(dir)
	nodes, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := loadOrCreateDashboardIdentity(dir, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{auth: st, tokens: tokens, loginLimiter: auth.NewLoginLimiter(), audit: log, store: nodes, serverCert: cert}
	return &authApp{app: a, h: a.audited(a.routes(fstest.MapFS{}))}
}

func (a *authApp) needs(t *testing.T, session string) string {
	t.Helper()
	_, body := a.req(t, "GET", "/api/auth/status", session, nil)
	var st struct {
		User struct {
			Needs []string `json:"needs"`
		} `json:"user"`
	}
	_ = json.Unmarshal([]byte(body), &st)
	return strings.Join(st.User.Needs, ",")
}

// TestSecondFactorSignIn: an admin sets an authenticator app up before
// anything else, then gives a code at every sign-in; an operator needs
// none; an admin's reset takes the factors away.
func TestSecondFactorSignIn(t *testing.T) {
	a := newMFAApp(t)
	root := a.login(t, "root", "root-password")
	if got := a.needs(t, root); got != "mfa_enroll" {
		t.Fatalf("an admin without a factor needs %q", got)
	}
	if code, body := a.req(t, "GET", "/api/version", root, nil); code != http.StatusForbidden || !strings.Contains(body, "second factor") {
		t.Errorf("before setting one up: %d %s", code, body)
	}
	_, body := a.req(t, "POST", "/api/auth/mfa/totp/setup", root, nil)
	var setup struct {
		Secret string `json:"secret"`
		URI    string `json:"uri"`
		QR     string `json:"qr_svg"`
	}
	if err := json.Unmarshal([]byte(body), &setup); err != nil || setup.Secret == "" || !strings.HasPrefix(setup.QR, "<svg") || !strings.Contains(setup.URI, ":root@example.com?") {
		t.Fatalf("setup: %s", body)
	}
	code, _ := auth.TOTPCode(setup.Secret, time.Now())
	st, body := a.req(t, "POST", "/api/auth/mfa/totp/enable", root, map[string]string{"code": code})
	var rc struct {
		Codes []string `json:"recovery_codes"`
	}
	if _ = json.Unmarshal([]byte(body), &rc); st != http.StatusOK || len(rc.Codes) != 10 {
		t.Fatalf("enable: %d %s", st, body)
	}
	if code, _ := a.req(t, "GET", "/api/version", root, nil); code != http.StatusOK {
		t.Errorf("the session that set it up: %d", code)
	}

	// The next sign-in waits for a code: a wrong one, then the next step's.
	root = a.login(t, "root", "root-password")
	if got := a.needs(t, root); got != "mfa" {
		t.Fatalf("a new sign-in needs %q", got)
	}
	if code, _ := a.req(t, "POST", "/api/auth/password", root, map[string]string{"current_password": "root-password", "new_password": "another-one"}); code != http.StatusForbidden {
		t.Errorf("a password change before the second factor: %d", code)
	}
	if code, _ := a.req(t, "POST", "/api/auth/mfa/totp", root, map[string]string{"code": "000000"}); code != http.StatusBadRequest {
		t.Errorf("a wrong code: %d", code)
	}
	next, _ := auth.TOTPCode(setup.Secret, time.Now().Add(30*time.Second))
	if code, body := a.req(t, "POST", "/api/auth/mfa/totp", root, map[string]string{"code": next}); code != http.StatusNoContent {
		t.Fatalf("the code: %d %s", code, body)
	}
	if code, _ := a.req(t, "GET", "/api/users", root, nil); code != http.StatusOK {
		t.Errorf("after the code: %d", code)
	}
	if code, _ := a.req(t, "POST", "/api/auth/mfa/totp", root, map[string]string{"code": next}); code != http.StatusConflict {
		t.Errorf("a code for a session that gave one: %d", code)
	}

	// A recovery code, once.
	r2 := a.login(t, "root", "root-password")
	if code, _ := a.req(t, "POST", "/api/auth/mfa/recovery", r2, map[string]string{"code": strings.ToLower(rc.Codes[3])}); code != http.StatusNoContent {
		t.Errorf("a recovery code: %d", code)
	}
	r3 := a.login(t, "root", "root-password")
	if code, _ := a.req(t, "POST", "/api/auth/mfa/recovery", r3, map[string]string{"code": rc.Codes[3]}); code != http.StatusBadRequest {
		t.Errorf("a recovery code used already: %d", code)
	}

	// The last factor of an admin stays; an operator needs none.
	if code, _ := a.req(t, "DELETE", "/api/auth/mfa/totp", root, map[string]string{"password": "root-password"}); code != http.StatusConflict {
		t.Errorf("removing an admin's last factor: %d", code)
	}
	olga := a.login(t, "olga", "olga-password")
	if got := a.needs(t, olga); got != "" {
		t.Errorf("an operator needs %q", got)
	}
	if code, _ := a.req(t, "POST", "/api/auth/mfa/totp", olga, map[string]string{"code": "123456"}); code != http.StatusConflict {
		t.Errorf("an operator with no factor gives a code: %d", code)
	}

	// The policy, everyone's now: the operator sets one up first.
	if code, body := a.req(t, "PUT", "/api/settings", root, map[string]any{"session_idle_minutes": 30, "session_max_hours": 12, "mfa_required": "everyone"}); code != http.StatusOK {
		t.Fatalf("settings: %d %s", code, body)
	}
	if got := a.needs(t, olga); got != "mfa_enroll" {
		t.Errorf("an operator under everyone's policy needs %q", got)
	}

	// An admin's reset: the factors, and the account's sessions, gone.
	if code, _ := a.req(t, "POST", "/api/users", root, map[string]string{"name": "ada", "role": "admin", "password": "ada-given-pw"}); code != http.StatusCreated {
		t.Fatal(code)
	}
	if code, body := a.req(t, "PATCH", "/api/users/root", root, map[string]any{"reset_mfa": true}); code != http.StatusOK || !strings.Contains(body, `"mfa":false`) {
		t.Errorf("reset: %d %s", code, body)
	}
	if code, _ := a.req(t, "GET", "/api/version", root, nil); code != http.StatusUnauthorized {
		t.Errorf("a session after its account's reset: %d", code)
	}
}

// TestSecondFactorTries: five wrong codes end the sign-in, and count
// against the address.
func TestSecondFactorTries(t *testing.T) {
	a := newMFAApp(t)
	root := a.login(t, "root", "root-password")
	_, body := a.req(t, "POST", "/api/auth/mfa/totp/setup", root, nil)
	var setup struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal([]byte(body), &setup)
	code, _ := auth.TOTPCode(setup.Secret, time.Now())
	a.req(t, "POST", "/api/auth/mfa/totp/enable", root, map[string]string{"code": code})

	root = a.login(t, "root", "root-password")
	var last int
	for range 5 {
		last, _ = a.req(t, "POST", "/api/auth/mfa/totp", root, map[string]string{"code": "000000"})
	}
	if last != http.StatusUnauthorized {
		t.Errorf("the fifth wrong code: %d", last)
	}
	if got := a.needs(t, root); got != "" {
		t.Errorf("the sign-in survived: %q", got)
	}
}

func TestRelyingParty(t *testing.T) {
	for host, want := range map[string]string{"janus.example.com": "janus.example.com", "Janus.Example.com:8443": "janus.example.com", "localhost:18440": "localhost", "10.0.0.5:443": "", "[2001:db8::1]:443": "", "": ""} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		if _, got, _ := relyingParty(r); got != want {
			t.Errorf("%q: %q, want %q", host, got, want)
		}
	}
}
