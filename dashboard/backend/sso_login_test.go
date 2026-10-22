package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/dashboard/backend/internal/sso"
	"github.com/swenske/Janus/dashboard/backend/internal/sso/ssotest"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

const ssoHost = "controller.test"

// ssoApp is an app with root (admin), olga (operator) and rita (reader),
// policy nobody, and the provider kc: a fake one, Keycloak-shaped, whose
// rules make /janus-admins admins and /janus-readers readers. Its users:
// alice (/janus-admins), bob (/janus-readers), carol (no group), root (a
// local account's name).
func ssoApp(t *testing.T) (*authApp, *ssotest.IdP) {
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
	if _, err := st.Setup("root", "root-password", auth.MFANobody); err != nil {
		t.Fatal(err)
	}
	for name, role := range map[string]auth.Role{"olga": auth.Operator, "rita": auth.Reader} {
		if _, err := st.CreateUser(name, role, "given-password", nil); err != nil {
			t.Fatal(err)
		}
		if err := st.ChangePassword(name, "given-password", name+"-password"); err != nil {
			t.Fatal(err)
		}
	}
	tokens, _ := auth.OpenTokens(dir)
	log, _ := audit.Open(dir)
	nodes, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ssoStore, err := sso.Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{auth: st, tokens: tokens, loginLimiter: auth.NewLoginLimiter(), audit: log, store: nodes, sso: newSingleSignOn(ssoStore)}
	idp := ssotest.New(t)
	idp.AddUser("alice", map[string]any{"sub": "uuid-alice", "preferred_username": "alice", "groups": []string{"/janus-admins"}})
	idp.AddUser("bob", map[string]any{"sub": "uuid-bob", "preferred_username": "bob", "groups": []string{"/janus-readers"}})
	idp.AddUser("carol", map[string]any{"sub": "uuid-carol", "preferred_username": "carol"})
	idp.AddUser("root", map[string]any{"sub": "uuid-root", "preferred_username": "root", "groups": []string{"/janus-admins"}})
	idp.AddUser("olga.kc", map[string]any{"sub": "uuid-olga", "preferred_username": "olga.kc", "groups": []string{"/janus-readers"}})
	p, _ := sso.Preset(sso.PresetKeycloak)
	p.ID, p.Name, p.Enabled, p.Issuer, p.ClientID = "kc", "Keycloak", true, idp.URL, idp.ClientID
	p.TLS = sso.Trust{CAPEM: idp.CAPEM}
	p.Rules = []sso.Rule{{Group: "/janus-admins", Role: auth.Admin}, {Group: "/janus-readers", Role: auth.Reader}}
	if _, err := ssoStore.Put(p, &idp.ClientSecret); err != nil {
		t.Fatal(err)
	}
	return &authApp{app: a, h: a.audited(a.routes(http.NotFoundHandler()))}, idp
}

// browser is a browser on the Controller's page at https://controller.test:
// its cookies, sent by their Path and SameSite.
type browser struct {
	t       *testing.T
	a       *authApp
	cookies map[string]*http.Cookie
}

func newBrowser(t *testing.T, a *authApp) *browser {
	return &browser{t: t, a: a, cookies: map[string]*http.Cookie{}}
}

// do makes a request from the page - crossSite: one the provider's site
// sent the browser to (only SameSite=Lax cookies along).
func (b *browser) do(method, path string, body any, crossSite bool) *httptest.ResponseRecorder {
	b.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, "https://"+ssoHost+path, &buf)
	r.RemoteAddr = "192.0.2.7:4242"
	for _, c := range b.cookies {
		cp := c.Path
		if cp == "" {
			cp = "/"
		}
		if !strings.HasPrefix(r.URL.Path, cp) || (crossSite && c.SameSite == http.SameSiteStrictMode) {
			continue
		}
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	rec := httptest.NewRecorder()
	b.a.h.ServeHTTP(rec, r)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return rec
}

func (b *browser) session() string {
	if c, ok := b.cookies[sessionCookieName]; ok {
		return c.Value
	}
	return ""
}

// start asks to sign in through provider: the provider's URL.
func (b *browser) start(provider, ret string) string {
	b.t.Helper()
	rec := b.do("POST", "/api/auth/sso/"+provider+"/start", map[string]string{"return": ret}, false)
	if rec.Code != http.StatusOK {
		b.t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var out struct{ Redirect string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Redirect
}

// back is the provider sending the browser back to the callback.
func (b *browser) back(u *url.URL) {
	b.t.Helper()
	if u.Host != ssoHost || u.Path != ssoCallbackPath {
		b.t.Fatalf("sent back to %s", u)
	}
	rec := b.do("GET", u.RequestURI(), nil, true)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != ssoDoneRoute {
		b.t.Fatalf("callback: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

type finished struct {
	Return string `json:"return"`
	Error  string `json:"error"`
	Code   string `json:"code"`
	Ref    string `json:"ref"`
}

// finish is the page finishing.
func (b *browser) finish() (int, finished) {
	b.t.Helper()
	rec := b.do("POST", "/api/auth/sso/finish", nil, false)
	var out finished
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// signIn is user signing in through kc, the whole way.
func (b *browser) signIn(idp *ssotest.IdP, user string) (int, finished) {
	b.t.Helper()
	b.back(idp.SignIn(b.t, b.start("kc", "#/nodes"), user, false))
	return b.finish()
}

type statusView struct {
	Authenticated bool         `json:"authenticated"`
	SSO           []sso.Button `json:"sso"`
	User          struct {
		Name        string          `json:"name"`
		Role        auth.Role       `json:"role"`
		Needs       []string        `json:"needs"`
		HasPassword bool            `json:"has_password"`
		Provider    string          `json:"provider"`
		Identities  []identityView  `json:"identities"`
		SSO         *ssoAccountView `json:"sso"`
	} `json:"user"`
}

func (b *browser) status() statusView {
	b.t.Helper()
	var st statusView
	_ = json.Unmarshal(b.do("GET", "/api/auth/status", nil, false).Body.Bytes(), &st)
	return st
}

func TestSSOSignIn(t *testing.T) {
	a, idp := ssoApp(t)
	b := newBrowser(t, a)
	if st := b.status(); st.Authenticated || len(st.SSO) != 1 || st.SSO[0].ID != "kc" || st.SSO[0].Name != "Keycloak" {
		t.Fatalf("signed out: %+v", st)
	}
	authURL := b.start("kc", "#/cli-login?port=4242&state=abc&key=0a1b")
	c := b.cookies[ssoCookieName]
	if c == nil || c.SameSite != http.SameSiteLaxMode || !c.HttpOnly || !c.Secure || c.Path != ssoCookiePath {
		t.Fatalf("sign-in cookie: %+v", c)
	}
	q, _ := url.Parse(authURL)
	if got := q.Query().Get("redirect_uri"); got != "https://"+ssoHost+ssoCallbackPath {
		t.Fatalf("redirect_uri %q", got)
	}
	b.back(idp.SignIn(t, authURL, "alice", false))
	code, out := b.finish()
	if code != http.StatusOK || out.Return != "#/cli-login?port=4242&state=abc&key=0a1b" {
		t.Fatalf("finish: %d %+v", code, out)
	}
	if _, ok := b.cookies[ssoCookieName]; ok {
		t.Fatal("the sign-in cookie stayed")
	}
	st := b.status()
	if !st.Authenticated || st.User.Name != "alice" || st.User.Role != auth.Admin || st.User.HasPassword || st.User.Provider != "kc" ||
		st.User.SSO == nil || st.User.SSO.Provider != "kc" || len(st.User.Identities) != 1 || len(st.User.Needs) != 0 {
		t.Fatalf("signed in: %+v", st)
	}
	// The sign-in is in the audit, as alice's through kc.
	entries, _ := a.audit.Recent(10, func(e audit.Entry) bool { return e.Path == "/api/auth/sso/finish" })
	if len(entries) != 1 || entries[0].User != "alice" || entries[0].Via != "sso kc" || entries[0].Status != http.StatusOK {
		t.Fatalf("audit: %+v", entries)
	}
	// A reader; then nobody a rule matches; then a local account's name.
	if code, _ := newBrowser(t, a).signIn(idp, "bob"); code != http.StatusOK {
		t.Fatalf("bob: %d", code)
	}
	if u, _ := a.auth.User("bob"); u.Role != auth.Reader {
		t.Fatalf("bob: %+v", u)
	}
	for user, want := range map[string]struct {
		status int
		code   string
	}{"carol": {http.StatusForbidden, "access"}, "root": {http.StatusConflict, "name_taken"}} {
		nb := newBrowser(t, a)
		if code, out := nb.signIn(idp, user); code != want.status || out.Code != want.code || out.Ref == "" || out.Error == "" {
			t.Errorf("%s: %d %+v", user, code, out)
		}
		if nb.session() != "" {
			t.Errorf("%s got a session", user)
		}
	}
	if _, ok := a.auth.User("carol"); ok {
		t.Error("carol got an account")
	}
	if u, _ := a.auth.User("root"); !u.HasPassword || len(u.Identities) != 0 {
		t.Errorf("root was taken over: %+v", u)
	}
	// The provider refusing.
	nb := newBrowser(t, a)
	nb.back(idp.SignIn(t, nb.start("kc", ""), "alice", true))
	if code, out := nb.finish(); code != http.StatusUnauthorized || out.Code != sso.CodeDenied {
		t.Errorf("denied: %d %+v", code, out)
	}
}

// TestSSOBoundToItsBrowser: a sign-in finishes in the browser that
// started it, once, with its own state.
func TestSSOBoundToItsBrowser(t *testing.T) {
	a, idp := ssoApp(t)
	expired := func(b *browser) {
		t.Helper()
		if code, out := b.finish(); code != http.StatusBadRequest && code != http.StatusUnauthorized || out.Code != sso.CodeExpired {
			t.Fatalf("finish: %d %+v", code, out)
		}
		if b.session() != "" {
			t.Fatal("signed in")
		}
	}
	// Nothing started.
	expired(newBrowser(t, a))

	// The victim's browser brought the attacker's code (login CSRF): the
	// state is its own's.
	attacker, victim := newBrowser(t, a), newBrowser(t, a)
	back := idp.SignIn(t, attacker.start("kc", ""), "carol", false)
	victim.start("kc", "")
	victim.back(back)
	expired(victim)

	// A browser without the cookie: nothing to finish.
	stranger := newBrowser(t, a)
	stranger.back(idp.SignIn(t, newBrowser(t, a).start("kc", ""), "alice", false))
	expired(stranger)

	// The callback once: a replay finds nothing.
	b := newBrowser(t, a)
	back = idp.SignIn(t, b.start("kc", ""), "alice", false)
	saved := *b.cookies[ssoCookieName]
	b.back(back)
	if code, _ := b.finish(); code != http.StatusOK {
		t.Fatalf("first: %d", code)
	}
	delete(b.cookies, sessionCookieName)
	b.cookies[ssoCookieName] = &saved
	b.back(back)
	expired(b)

	// Another provider answering (iss, RFC 9207).
	idp.Set(func(k *ssotest.Knobs) { k.ISSParam = "wrong" })
	b = newBrowser(t, a)
	b.back(idp.SignIn(t, b.start("kc", ""), "alice", false))
	if code, out := b.finish(); code != http.StatusUnauthorized || out.Code != sso.CodeToken {
		t.Fatalf("another iss: %d %+v", code, out)
	}
	idp.Set(func(k *ssotest.Knobs) { k.ISSParam = "right" })
	if code, _ := newBrowser(t, a).signIn(idp, "alice"); code != http.StatusOK {
		t.Fatalf("its own iss: %d", code)
	}

	// Disabled under way.
	b = newBrowser(t, a)
	back = idp.SignIn(t, b.start("kc", ""), "alice", false)
	p, _ := a.sso.store.Get("kc")
	p.Enabled = false
	if _, err := a.sso.store.Put(p, nil); err != nil {
		t.Fatal(err)
	}
	b.back(back)
	if code, out := b.finish(); code != http.StatusUnauthorized || out.Code != sso.CodeConfig {
		t.Fatalf("disabled under way: %d %+v", code, out)
	}
	if code := newBrowser(t, a).do("POST", "/api/auth/sso/kc/start", map[string]string{}, false).Code; code != http.StatusNotFound {
		t.Fatalf("start a disabled provider: %d", code)
	}
}

func TestSSOStartRefusals(t *testing.T) {
	a, _ := ssoApp(t)
	b := newBrowser(t, a)
	for _, ret := range []string{"https://evil.example/", "//evil.example", "#nodes", "/#/x", "#/a b"} {
		if code := b.do("POST", "/api/auth/sso/kc/start", map[string]string{"return": ret}, false).Code; code != http.StatusBadRequest {
			t.Errorf("return %q: %d", ret, code)
		}
	}
	if code := b.do("POST", "/api/auth/sso/nope/start", map[string]string{}, false).Code; code != http.StatusNotFound {
		t.Errorf("an unknown provider: %d", code)
	}
	limited := 0
	for range ssoStartBurst + 5 {
		if b.do("POST", "/api/auth/sso/kc/start", map[string]string{}, false).Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Error("starting sign-ins isn't limited")
	}
}

func TestSSOSecondFactor(t *testing.T) {
	a, idp := ssoApp(t)
	if err := a.auth.SetSettings(auth.Settings{SessionIdleMinutes: 30, SessionMaxHours: 12, MFARequired: auth.MFAEveryone}); err != nil {
		t.Fatal(err)
	}
	// The Controller's second factor, by default: alice sets one up.
	b := newBrowser(t, a)
	if code, _ := b.signIn(idp, "alice"); code != http.StatusOK {
		t.Fatal(code)
	}
	if st := b.status(); !slices.Equal(st.User.Needs, []string{"mfa_enroll"}) {
		t.Fatalf("needs %v", st.User.Needs)
	}
	secret, err := a.auth.StartTOTP(b.session())
	if err != nil {
		t.Fatal(err)
	}
	totp, _ := auth.TOTPCode(secret, time.Now())
	if _, err := a.auth.EnableTOTP(b.session(), totp); err != nil {
		t.Fatal(err)
	}
	trust, _, err := a.auth.TrustBrowser(b.session(), "test", "192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	// Next time: the factor - unless the browser is trusted for it (its
	// cookie isn't sent to the callback, it is to finish).
	b = newBrowser(t, a)
	b.signIn(idp, "alice")
	if st := b.status(); !slices.Equal(st.User.Needs, []string{"mfa"}) {
		t.Fatalf("needs %v", st.User.Needs)
	}
	b = newBrowser(t, a)
	b.cookies[trustCookieName] = trustCookie(trust, 3600)
	b.signIn(idp, "alice")
	if st := b.status(); len(st.User.Needs) != 0 {
		t.Fatalf("a trusted browser needs %v", st.User.Needs)
	}
	// The provider's second factor, when its ID token says so.
	p, _ := a.sso.store.Get("kc")
	p.MFA, p.ACR = sso.MFAProvider, []string{"gold"}
	if _, err := a.sso.store.Put(p, nil); err != nil {
		t.Fatal(err)
	}
	idp.AddUser("bob", map[string]any{"sub": "uuid-bob", "preferred_username": "bob", "groups": []string{"/janus-readers"}, "acr": "gold"})
	b = newBrowser(t, a)
	authURL := b.start("kc", "")
	if u, _ := url.Parse(authURL); u.Query().Get("acr_values") != "gold" {
		t.Fatalf("acr_values: %s", authURL)
	}
	b.back(idp.SignIn(t, authURL, "bob", false))
	b.finish()
	if st := b.status(); len(st.User.Needs) != 0 {
		t.Fatalf("the provider's factor: needs %v", st.User.Needs)
	}
	// ... not when it doesn't: the Controller's again.
	idp.AddUser("bob", map[string]any{"sub": "uuid-bob", "preferred_username": "bob", "groups": []string{"/janus-readers"}, "acr": "1"})
	b = newBrowser(t, a)
	b.signIn(idp, "bob")
	if st := b.status(); !slices.Equal(st.User.Needs, []string{"mfa_enroll"}) {
		t.Fatalf("acr 1: needs %v", st.User.Needs)
	}
}

func TestSSOLink(t *testing.T) {
	a, idp := ssoApp(t)
	olga := newBrowser(t, a)
	olga.cookies[sessionCookieName] = &http.Cookie{Name: sessionCookieName, Value: a.login(t, "olga", "olga-password"), Path: "/", SameSite: http.SameSiteStrictMode}
	if code := olga.do("POST", "/api/auth/sso/kc/link", map[string]string{"password": "wrong-password"}, false).Code; code != http.StatusBadRequest {
		t.Fatalf("a wrong password: %d", code)
	}
	rec := olga.do("POST", "/api/auth/sso/kc/link", map[string]string{"password": "olga-password", "return": "#/"}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body)
	}
	var out struct{ Redirect string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if u, _ := url.Parse(out.Redirect); u.Query().Get("prompt") != "login" {
		t.Fatalf("linking doesn't make the provider ask: %s", out.Redirect)
	}
	back := idp.SignIn(t, out.Redirect, "olga.kc", false)
	// Finished from another session than the one that asked: refused.
	saved := *olga.cookies[sessionCookieName]
	olga.cookies[sessionCookieName].Value = a.login(t, "rita", "rita-password")
	olga.back(back)
	if code, out := olga.finish(); code != http.StatusForbidden || out.Code != "link" {
		t.Fatalf("finished by rita: %d %+v", code, out)
	}
	olga.cookies[sessionCookieName] = &saved
	rec = olga.do("POST", "/api/auth/sso/kc/link", map[string]string{"password": "olga-password"}, false)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	olga.back(idp.SignIn(t, out.Redirect, "olga.kc", false))
	if code, out := olga.finish(); code != http.StatusOK {
		t.Fatalf("link: %d %+v", code, out)
	}
	st := olga.status()
	if st.User.Name != "olga" || len(st.User.Identities) != 1 || st.User.Identities[0].Username != "olga.kc" {
		t.Fatalf("olga: %+v", st)
	}
	// Signing in through the provider: olga, an operator still.
	b := newBrowser(t, a)
	if code, _ := b.signIn(idp, "olga.kc"); code != http.StatusOK {
		t.Fatal(code)
	}
	if st := b.status(); st.User.Name != "olga" || st.User.Role != auth.Operator || !st.User.HasPassword {
		t.Fatalf("olga through kc: %+v", st)
	}
	// An account a provider made has nothing to link.
	alice := newBrowser(t, a)
	alice.signIn(idp, "alice")
	if code := alice.do("POST", "/api/auth/sso/kc/link", map[string]string{"password": "x"}, false).Code; code != http.StatusConflict {
		t.Fatalf("alice links: %d", code)
	}
	// Forgetting it.
	if code := olga.do("DELETE", "/api/auth/identities/"+st.User.Identities[0].ID, nil, false).Code; code != http.StatusNoContent {
		t.Fatalf("unlink: %d", code)
	}
	if code := alice.do("DELETE", "/api/auth/identities/x", nil, false).Code; code != http.StatusConflict {
		t.Fatalf("alice unlinks: %d", code)
	}
}

func TestSSOLogout(t *testing.T) {
	a, idp := ssoApp(t)
	b := newBrowser(t, a)
	b.signIn(idp, "alice")
	rec := b.do("POST", "/api/auth/logout", nil, false)
	var out struct{ Redirect string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	u, err := url.Parse(out.Redirect)
	if rec.Code != http.StatusOK || err != nil || u.Path != "/logout" || u.Query().Get("id_token_hint") == "" || u.Query().Get("post_logout_redirect_uri") != "https://"+ssoHost+"/" {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body)
	}
	if b.session() != "" || b.status().Authenticated {
		t.Fatal("still signed in")
	}
	// A provider without an end-session endpoint (a Synology): here only.
	idp.Set(func(k *ssotest.Knobs) { k.NoEndSession = true })
	p, _ := a.sso.store.Get("kc")
	if _, err := a.sso.store.Put(p, nil); err != nil { // a new version: discovery again
		t.Fatal(err)
	}
	b.signIn(idp, "alice")
	if code := b.do("POST", "/api/auth/logout", nil, false).Code; code != http.StatusNoContent {
		t.Fatalf("logout without end session: %d", code)
	}
	// A password sign-in: here only.
	if code, _ := a.req(t, "POST", "/api/auth/logout", a.login(t, "rita", "rita-password"), nil); code != http.StatusNoContent {
		t.Fatalf("rita's logout: %d", code)
	}
}

func TestSSOAccountsWithoutPassword(t *testing.T) {
	a, idp := ssoApp(t)
	b := newBrowser(t, a)
	b.signIn(idp, "alice")
	if code := b.do("POST", "/api/auth/password", map[string]string{"current_password": "", "new_password": "long-enough"}, false).Code; code != http.StatusConflict {
		t.Errorf("own password: %d", code)
	}
	if code, _ := a.req(t, "POST", "/api/auth/login", "", map[string]string{"name": "alice", "password": ""}); code != http.StatusUnauthorized {
		t.Errorf("an empty password: %d", code)
	}
	root := a.login(t, "root", "root-password")
	for _, body := range []map[string]any{{"role": "reader"}, {"grants": []any{}}, {"reset_password": true}} {
		if code, _ := a.req(t, "PATCH", "/api/users/alice", root, body); code != http.StatusConflict {
			t.Errorf("%v: %d", body, code)
		}
	}
	if code, _ := a.req(t, "PATCH", "/api/users/alice", root, map[string]any{"disabled": true}); code != http.StatusOK {
		t.Errorf("disable: %d", code)
	}
	if b.status().Authenticated {
		t.Error("a disabled account's session lives")
	}
	if code, out := newBrowser(t, a).signIn(idp, "alice"); code != http.StatusForbidden || out.Code != "disabled" {
		t.Errorf("a disabled account signs in: %d %+v", code, out)
	}
	var users []userView
	_, body := a.req(t, "GET", "/api/users", root, nil)
	_ = json.Unmarshal([]byte(body), &users)
	i := slices.IndexFunc(users, func(u userView) bool { return u.Name == "alice" })
	if i < 0 || users[i].HasPassword || users[i].SSO == nil || len(users[i].Identities) != 1 {
		t.Fatalf("users: %s", body)
	}
}

func TestSSOSettings(t *testing.T) {
	a, idp := ssoApp(t)
	root := newBrowser(t, a)
	root.cookies[sessionCookieName] = &http.Cookie{Name: sessionCookieName, Value: a.login(t, "root", "root-password"), Path: "/"}
	var list ssoProvidersView
	_ = json.Unmarshal(root.do("GET", "/api/sso/providers", nil, false).Body.Bytes(), &list)
	if len(list.Providers) != 1 || !list.Providers[0].HasSecret || list.RedirectURI != "https://"+ssoHost+ssoCallbackPath || list.PostLogoutURI != "https://"+ssoHost+"/" || len(list.Presets) != 3 {
		t.Fatalf("providers: %+v", list)
	}
	if strings.Contains(root.do("GET", "/api/sso/providers", nil, false).Body.String(), idp.ClientSecret) {
		t.Fatal("the client secret is in the list")
	}
	// alice and bob sign in; then the rules change: bob loses his access
	// at once, his session with it.
	alice, bob := newBrowser(t, a), newBrowser(t, a)
	alice.signIn(idp, "alice")
	bob.signIn(idp, "bob")
	p := list.Providers[0].Provider
	p.Rules = []sso.Rule{{Group: "/janus-admins", Role: auth.Admin}}
	rec := root.do("PUT", "/api/sso/providers/kc", p, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if bob.status().Authenticated || !alice.status().Authenticated {
		t.Fatal("the rules' change: bob still in, or alice out")
	}
	if u, _ := a.auth.User("bob"); u.Role != auth.None {
		t.Fatalf("bob: %+v", u)
	}
	// Someone else's change isn't overwritten.
	if code := root.do("PUT", "/api/sso/providers/kc", p, false).Code; code != http.StatusConflict {
		t.Fatalf("a stale version: %d", code)
	}
	if code := root.do("PUT", "/api/sso/providers/other", p, false).Code; code != http.StatusBadRequest {
		t.Fatalf("another ID than the URL's: %d", code)
	}
	// The test of a draft.
	var report sso.Report
	_ = json.Unmarshal(root.do("POST", "/api/sso/test", p, false).Body.Bytes(), &report)
	if report.Error != "" || !report.IssuerMatches || report.TokenAuth != "client_secret_basic" {
		t.Fatalf("test: %+v", report)
	}
	// alice makes a token; the provider goes: her sessions end, she
	// lapses, her token with her.
	rec = alice.do("POST", "/api/tokens", map[string]any{"name": "t"}, false)
	var tok struct{ Token string }
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	if code, _ := a.req(t, "GET", "/api/version", "Bearer "+tok.Token, nil); code != http.StatusOK {
		t.Fatalf("alice's token: %d (%s)", code, rec.Body)
	}
	if code := root.do("DELETE", "/api/sso/providers/kc", nil, false).Code; code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if alice.status().Authenticated {
		t.Fatal("alice's session outlived her provider")
	}
	if code, body := a.req(t, "GET", "/api/version", "Bearer "+tok.Token, nil); code != http.StatusForbidden || !strings.Contains(body, "sign in there again") {
		t.Fatalf("a lapsed account's token: %d %s", code, body)
	}
	if st := newBrowser(t, a).status(); len(st.SSO) != 0 {
		t.Fatalf("buttons: %+v", st.SSO)
	}
}
