package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// authApp is an app with accounts root (admin), olga (operator) and rita
// (reader), and the main port's handler.
type authApp struct {
	*app
	h http.Handler
}

func newAuthApp(t *testing.T) *authApp {
	t.Helper()
	dir := t.TempDir()
	authStore, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authStore.Setup("root", "root-password", auth.MFANobody); err != nil {
		t.Fatal(err)
	}
	for name, role := range map[string]auth.Role{"olga": auth.Operator, "rita": auth.Reader} {
		if _, err := authStore.CreateUser(name, role, "given-password", nil); err != nil {
			t.Fatal(err)
		}
		if err := authStore.ChangePassword(name, "given-password", name+"-password"); err != nil {
			t.Fatal(err)
		}
	}
	tokens, err := auth.OpenTokens(dir)
	if err != nil {
		t.Fatal(err)
	}
	log, err := audit.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := machines.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{auth: authStore, tokens: tokens, loginLimiter: auth.NewLoginLimiter(), audit: log, store: st, machines: ms}
	return &authApp{app: a, h: a.audited(a.routes(fstest.MapFS{}))}
}

// req makes a request with a session cookie or a bearer token
// ("Bearer ..."), answering the status and body.
func (a *authApp) req(t *testing.T, method, path, cred string, body any) (int, string) {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	r := httptest.NewRequest(method, path, &b)
	r.RemoteAddr = "192.0.2.7:4242"
	if strings.HasPrefix(cred, "Bearer ") {
		r.Header.Set("Authorization", cred)
	} else if cred != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cred})
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// login signs name in through the API, answering the session cookie.
func (a *authApp) login(t *testing.T, name, password string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"name": name, "password": password})
	r := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(b))
	r.RemoteAddr = "192.0.2.7:4242"
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login %s: %d %s", name, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c.Value
		}
	}
	t.Fatalf("login %s: no session cookie", name)
	return ""
}

// TestRoutesNeedTheirRole: what each role is refused, before any handler
// runs - every change for a reader, all of these for an operator.
// Powering machines and their consoles are checked on the machine
// (TestScopes).
func TestRoutesNeedTheirRole(t *testing.T) {
	a := newAuthApp(t)
	rita, olga := a.login(t, "rita", "rita-password"), a.login(t, "olga", "olga-password")
	for _, tc := range []struct {
		method, path string
		need         auth.Role
	}{
		{"POST", "/api/nodes", auth.Admin},
		{"DELETE", "/api/nodes/n1", auth.Admin},
		{"POST", "/api/pending/p1/approve", auth.Admin},
		{"POST", "/api/pending/p1/reject", auth.Admin},
		{"POST", "/api/controller/update", auth.Admin},
		{"POST", "/api/machines", auth.Admin},
		{"PATCH", "/api/machines/m1", auth.Admin},
		{"DELETE", "/api/machines/m1", auth.Admin},
		{"POST", "/api/machines/m1/retry", auth.Admin},
		{"POST", "/api/hypervisors", auth.Admin},
		{"PATCH", "/api/hypervisors/h1", auth.Admin},
		{"DELETE", "/api/hypervisors/h1", auth.Admin},
		{"POST", "/api/hypervisors/h1/probe", auth.Admin},
		{"POST", "/api/hypervisors/h1/trust", auth.Admin},
		{"POST", "/api/hypervisors/preparation", auth.Admin},
		{"POST", "/api/fleet/setup", auth.Admin},
		{"GET", "/api/fleet/recovery-kit", auth.Admin},
		{"POST", "/api/fleet/confirm", auth.Admin},
		{"GET", "/api/users", auth.Admin},
		{"POST", "/api/users", auth.Admin},
		{"PATCH", "/api/users/rita", auth.Admin},
		{"DELETE", "/api/users/rita", auth.Admin},
		{"DELETE", "/api/users/rita/ssh-keys", auth.Admin},
		{"GET", "/api/settings", auth.Admin},
		{"PUT", "/api/settings", auth.Admin},
		{"GET", "/api/audit", auth.Admin},
		{"GET", "/api/backups", auth.Admin},
		{"PUT", "/api/backups/settings", auth.Admin},
		{"POST", "/api/backups/kit", auth.Admin},
		{"GET", "/api/backups/kit", auth.Admin},
		{"POST", "/api/backups/kit/confirm", auth.Admin},
		{"POST", "/api/backups/run", auth.Admin},
		{"GET", "/api/backups/download", auth.Admin},
		{"GET", "/api/backups/list", auth.Admin},
		{"GET", "/api/enroll-tokens", auth.Admin},
		{"POST", "/api/enroll-tokens", auth.Admin},
		{"DELETE", "/api/enroll-tokens/e1", auth.Admin},
	} {
		if code, _ := a.req(t, tc.method, tc.path, "", nil); code != http.StatusUnauthorized {
			t.Errorf("%s %s with nothing: %d", tc.method, tc.path, code)
		}
		if code, _ := a.req(t, tc.method, tc.path, rita, nil); code != http.StatusForbidden {
			t.Errorf("%s %s as a reader: %d", tc.method, tc.path, code)
		}
		if tc.need == auth.Admin {
			if code, _ := a.req(t, tc.method, tc.path, olga, nil); code != http.StatusForbidden {
				t.Errorf("%s %s as an operator: %d", tc.method, tc.path, code)
			}
		}
	}
	if code, body := a.req(t, "GET", "/api/version", rita, nil); code != http.StatusOK || !strings.Contains(body, "version") {
		t.Errorf("a reader reads the version: %d %s", code, body)
	}
}

// TestAuthStatus: who the page is signed in as.
func TestAuthStatus(t *testing.T) {
	a := newAuthApp(t)
	var st struct {
		SetupRequired bool `json:"setup_required"`
		Authenticated bool `json:"authenticated"`
		User          *struct {
			Name  string   `json:"name"`
			Role  string   `json:"role"`
			Needs []string `json:"needs"`
		} `json:"user"`
	}
	_, body := a.req(t, "GET", "/api/auth/status", a.login(t, "olga", "olga-password"), nil)
	if err := json.Unmarshal([]byte(body), &st); err != nil || !st.Authenticated || st.User == nil || st.User.Name != "olga" || st.User.Role != "operator" || st.User.Needs == nil {
		t.Fatalf("status: %s", body)
	}
	if _, body := a.req(t, "GET", "/api/auth/status", "", nil); strings.Contains(body, `"user"`) {
		t.Errorf("signed out: %s", body)
	}
	// The name defaults to admin's, for scripts written for one password.
	if code, _ := a.req(t, "POST", "/api/auth/login", "", map[string]string{"password": "root-password"}); code != http.StatusUnauthorized {
		t.Errorf("no name, and no admin account: %d", code)
	}
}

// TestMustChangePassword: an account whose password an admin set can only
// change it - then everything its role allows.
func TestMustChangePassword(t *testing.T) {
	a := newAuthApp(t)
	root := a.login(t, "root", "root-password")
	code, body := a.req(t, "POST", "/api/users", root, map[string]string{"name": "nina", "role": "reader"})
	var made struct {
		Password string `json:"password"`
	}
	if _ = json.Unmarshal([]byte(body), &made); code != http.StatusCreated || len(made.Password) < 20 {
		t.Fatalf("create: %d %s", code, body)
	}
	nina := a.login(t, "nina", made.Password)
	if code, _ := a.req(t, "GET", "/api/version", nina, nil); code != http.StatusForbidden {
		t.Errorf("before the change: %d", code)
	}
	if code, _ := a.req(t, "POST", "/api/auth/password", nina, map[string]string{"current_password": "wrong", "new_password": "ninas-password"}); code != http.StatusBadRequest {
		t.Errorf("a wrong current password: %d", code)
	}
	b, _ := json.Marshal(map[string]string{"current_password": made.Password, "new_password": "ninas-password"})
	r := httptest.NewRequest("POST", "/api/auth/password", bytes.NewReader(b))
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: nina})
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNoContent || len(rec.Result().Cookies()) == 0 {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}
	if code, _ := a.req(t, "GET", "/api/version", nina, nil); code != http.StatusUnauthorized {
		t.Errorf("the session from before the change: %d", code)
	}
	if code, _ := a.req(t, "GET", "/api/version", rec.Result().Cookies()[0].Value, nil); code != http.StatusOK {
		t.Errorf("the new session: %d", code)
	}

	// A reset: the old password and sessions are over.
	_, body = a.req(t, "PATCH", "/api/users/nina", root, map[string]any{"reset_password": true})
	if _ = json.Unmarshal([]byte(body), &made); len(made.Password) < 20 {
		t.Fatalf("reset: %s", body)
	}
	if code, _ := a.req(t, "POST", "/api/auth/login", "", map[string]string{"name": "nina", "password": "ninas-password"}); code != http.StatusUnauthorized {
		t.Errorf("the password from before the reset: %d", code)
	}
	if code, _ := a.req(t, "PATCH", "/api/users/root", root, map[string]any{"role": "reader"}); code != http.StatusConflict {
		t.Errorf("demoting the last admin: %d", code)
	}
}

// TestAPITokens: a token acts for its account with its role at most -
// lowered or gone with the account -, and is managed from a session only.
func TestAPITokens(t *testing.T) {
	a := newAuthApp(t)
	root, rita := a.login(t, "root", "root-password"), a.login(t, "rita", "rita-password")
	create := func(cred string, body map[string]any) (int, string, string) {
		t.Helper()
		code, out := a.req(t, "POST", "/api/tokens", cred, body)
		var tok struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		_ = json.Unmarshal([]byte(out), &tok)
		return code, "Bearer " + tok.Token, tok.ID
	}
	if code, _, _ := create(rita, map[string]any{"name": "more", "role": "operator"}); code != http.StatusForbidden {
		t.Errorf("a reader's operator token: %d", code)
	}
	_, ritaTok, ritaID := create(rita, map[string]any{"name": "monitoring"})
	_, readTok, _ := create(root, map[string]any{"name": "dashboards", "role": "reader"})
	_, adminTok, adminID := create(root, map[string]any{"name": "terraform"})

	if code, _ := a.req(t, "GET", "/api/version", ritaTok, nil); code != http.StatusOK {
		t.Errorf("rita's token: %d", code)
	}
	if code, _ := a.req(t, "GET", "/api/users", readTok, nil); code != http.StatusForbidden {
		t.Errorf("an admin's reader token reads accounts: %d", code)
	}
	if code, _ := a.req(t, "GET", "/api/users", adminTok, nil); code != http.StatusOK {
		t.Errorf("an admin token reads accounts: %d", code)
	}
	if code, _ := a.req(t, "POST", "/api/users", adminTok, map[string]string{"name": "x", "role": "admin"}); code != http.StatusForbidden {
		t.Errorf("a token makes an account: %d", code)
	}
	if code, _ := a.req(t, "GET", "/api/tokens", adminTok, nil); code != http.StatusForbidden {
		t.Errorf("a token lists tokens: %d", code)
	}
	if _, body := a.req(t, "GET", "/api/tokens", rita, nil); strings.Count(body, `"id"`) != 1 {
		t.Errorf("rita sees: %s", body)
	}
	if _, body := a.req(t, "GET", "/api/tokens", root, nil); strings.Count(body, `"id"`) != 3 {
		t.Errorf("an admin sees: %s", body)
	}
	if code, _ := a.req(t, "DELETE", "/api/tokens/"+adminID, rita, nil); code != http.StatusNotFound {
		t.Errorf("rita revokes root's token: %d", code)
	}

	// Demoted, the account's tokens follow; disabled or deleted, they stop.
	if code, _ := a.req(t, "POST", "/api/users", root, map[string]string{"name": "ada", "role": "admin", "password": "ada-given-pw"}); code != http.StatusCreated {
		t.Fatal(code)
	}
	if err := a.auth.ChangePassword("ada", "ada-given-pw", "ada-password"); err != nil {
		t.Fatal(err)
	}
	ada := a.login(t, "ada", "ada-password")
	_, adaTok, _ := create(ada, map[string]any{"name": "ci"})
	a.req(t, "PATCH", "/api/users/ada", root, map[string]any{"role": "reader"})
	if code, _ := a.req(t, "GET", "/api/users", adaTok, nil); code != http.StatusForbidden {
		t.Errorf("a demoted admin's token: %d", code)
	}
	a.req(t, "PATCH", "/api/users/rita", root, map[string]any{"disabled": true})
	if code, _ := a.req(t, "GET", "/api/version", ritaTok, nil); code != http.StatusUnauthorized {
		t.Errorf("a disabled account's token: %d", code)
	}
	if code, _ := a.req(t, "DELETE", "/api/users/rita", root, nil); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if _, ok := a.tokens.Get(ritaID); ok {
		t.Error("a deleted account's token kept")
	}

	// A wrong token counts against the address, like a wrong password.
	for i := 0; i < 10; i++ {
		a.req(t, "GET", "/api/version", "Bearer janus_x_y", nil)
	}
	if code, _ := a.req(t, "GET", "/api/version", adminTok, nil); code != http.StatusTooManyRequests {
		t.Errorf("after many wrong tokens: %d, want 429", code)
	}
}

// TestAudit: who changed what, and who tried to sign in.
func TestAudit(t *testing.T) {
	a := newAuthApp(t)
	rita := a.login(t, "rita", "rita-password")
	a.req(t, "POST", "/api/auth/login", "", map[string]string{"name": "mallory", "password": "guess-guess"})
	a.req(t, "POST", "/api/hypervisors", rita, map[string]string{"name": "h"})
	a.req(t, "GET", "/api/version", rita, nil)
	root := a.login(t, "root", "root-password")
	a.req(t, "PUT", "/api/settings", root, map[string]int{"session_idle_minutes": 60, "session_max_hours": 8})

	entries, err := a.audit.Recent(100, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.User+" "+e.Via+" "+e.Method+" "+e.Path+" "+http.StatusText(e.Status))
	}
	want := []string{
		"root session PUT /api/settings OK",
		"root  POST /api/auth/login No Content",
		"rita session POST /api/hypervisors Forbidden",
		"mallory  POST /api/auth/login Unauthorized",
		"rita  POST /api/auth/login No Content",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if code, body := a.req(t, "GET", "/api/audit?user=mallory", root, nil); code != http.StatusOK || strings.Count(body, `"time"`) != 1 {
		t.Errorf("the audit, for one name: %d %s", code, body)
	}
}

// tokenFor makes an API token from session with role, answering its ID
// and its "Bearer ..." credential.
func (a *authApp) tokenFor(t *testing.T, session, role string) (string, string) {
	t.Helper()
	code, out := a.req(t, "POST", "/api/tokens", session, map[string]any{"name": "t-" + role, "role": role})
	var tok struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(out), &tok); err != nil || code != http.StatusCreated {
		t.Fatalf("token: %d %s", code, out)
	}
	return tok.ID, "Bearer " + tok.Token
}
