package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/ratelimit"
	"github.com/swenske/Janus/dashboard/backend/internal/sso"
)

// --- single sign-on: /api/auth/sso/* ---
//
// A sign-in through a provider goes in three steps, so the cookies the
// Controller's sign-in relies on (SameSite=Strict: never sent when
// another site sends the browser here) are there when it matters:
//
//  1. the page asks to start (POST .../start): the Controller keeps the
//     sign-in under a random key - in the cookie janus_sso, SameSite=Lax
//     - and answers the provider's URL, which the page goes to;
//  2. the provider sends the browser back (GET .../callback, from its
//     site: only janus_sso comes along): the Controller exchanges the
//     code, checks the ID token, keeps who it is and sends the browser
//     to the page (#/sso-done);
//  3. the page finishes (POST .../finish, from this site: the session
//     and trusted-browser cookies come along): the account signs in, or
//     the identity is linked to the session's.

const (
	ssoCookieName   = "janus_sso"
	ssoCookiePath   = "/api/auth/sso/"
	ssoCallbackPath = "/api/auth/sso/callback"
	ssoDoneRoute    = "/#/sso-done"
	// ssoStartBurst and ssoStartEvery bound the sign-ins an address
	// starts: each fetches from the provider.
	ssoStartBurst = 10
	ssoStartEvery = 6 * time.Second
)

// singleSignOn is the Controller's single sign-on: the providers'
// settings, their discovery, the sign-ins under way.
type singleSignOn struct {
	store   *sso.Store
	clients *sso.Clients
	logins  *sso.Logins
	limiter *ratelimit.Limiter

	// logged are the providers' versions whose claim names were logged.
	mu     sync.Mutex
	logged map[string]bool
}

func newSingleSignOn(store *sso.Store) *singleSignOn {
	return &singleSignOn{store: store, clients: sso.NewClients(), logins: sso.NewLogins(), limiter: ratelimit.New(ssoStartBurst, ssoStartEvery), logged: map[string]bool{}}
}

func (a *app) registerSSORoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/sso/{id}/start", a.handleSSOStart)
	mux.HandleFunc("POST /api/auth/sso/{id}/link", a.sessionGate(anyone, anyone, a.handleSSOLink))
	mux.HandleFunc("GET "+ssoCallbackPath, a.handleSSOCallback)
	mux.HandleFunc("POST /api/auth/sso/finish", a.handleSSOFinish)
	mux.HandleFunc("DELETE /api/auth/identities/{id}", a.sessionGate(anyone, anyone, a.handleIdentityUnlink))
	mux.HandleFunc("DELETE /api/users/{name}/identities/{id}", a.sessionGate(auth.Admin, auth.Admin, a.handleUserIdentityUnlink))
	a.registerSSOSettingsRoutes(mux)
}

// ssoMessages are what the page tells of a failed sign-in, by code - the
// details are in the Controller's log, under the reference it shows.
var ssoMessages = map[string]string{
	sso.CodeConfig:   "This single sign-on isn't set up right: an admin can test it (Accounts, Single sign-on).",
	sso.CodeProvider: "The provider didn't answer as it should: try again, or ask an admin.",
	sso.CodeDenied:   "The provider didn't sign you in.",
	sso.CodeToken:    "The provider's answer didn't check out: ask an admin, with the reference below.",
	sso.CodeExpired:  "This sign-in is over, or another one started in this browser since: start again.",
	"busy":           "Too many sign-ins are under way: try again in a few minutes.",
	"access":         "The provider's rules give you no access to this Controller: ask an admin.",
	"name_taken":     "An account of this name already exists on this Controller: sign in with its password, then link this identity to it - your name at the top right, Single sign-on.",
	"name_invalid":   "The provider's name for you isn't one an account can have (1 to 64 lowercase letters, digits and . _ @ -).",
	"disabled":       "This account is disabled.",
	"identity_taken": "This identity is linked to an account already.",
	"link":           "Linking stopped: sign in again on this page, then link from Your account.",
}

func newRef() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ssoFail answers a failed sign-in: its code, the page's message and a
// reference - the same in the log, with what went wrong -, and the
// provider (the page offers to link an identity whose name is taken).
func ssoFail(w http.ResponseWriter, status int, provider, code string, err error) {
	ref := newRef()
	log.Printf("sso: ref %s: %s: %s: %v", ref, or(provider, "-"), code, err)
	msg := ssoMessages[code]
	if msg == "" {
		msg = ssoMessages[sso.CodeProvider]
	}
	out := map[string]string{"error": msg, "code": code, "ref": ref}
	if provider != "" {
		out["provider"] = provider
	}
	writeJSON(w, status, out)
}

// ssoRedirectURI is where the provider sends the browser back: this
// Controller as the browser opened it - a host the TLS handshake didn't
// name is refused, and the provider checks the URI against its own list.
func ssoRedirectURI(r *http.Request) (string, error) {
	host := strings.ToLower(r.Host)
	if host == "" {
		return "", errors.New("no Host")
	}
	if r.TLS != nil && r.TLS.ServerName != "" {
		name := host
		if h, _, err := net.SplitHostPort(host); err == nil {
			name = h
		}
		if !strings.EqualFold(strings.TrimSuffix(name, "."), r.TLS.ServerName) {
			return "", fmt.Errorf("the Host %q isn't the name the connection was opened for (%q)", r.Host, r.TLS.ServerName)
		}
	}
	return "https://" + host + ssoCallbackPath, nil
}

func ssoCookie(value string, maxAge int) *http.Cookie {
	// Lax, not Strict: the provider's site sends the browser back with it.
	return &http.Cookie{Name: ssoCookieName, Value: value, Path: ssoCookiePath, MaxAge: maxAge, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
}

// ssoClient is the provider's client, its secret opened.
func (a *app) ssoClient(r *http.Request, p sso.Provider) (*sso.Client, error) {
	secret, err := a.sso.store.Secret(p.ID)
	if err != nil {
		return nil, &sso.Error{Code: sso.CodeConfig, Err: err}
	}
	return a.sso.clients.Get(r.Context(), p, secret)
}

func errCode(err error, fallback string) string {
	var e *sso.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return fallback
}

// handleSSOStart starts signing in through a provider: {"return": a
// route of the page to go back to} - answering {"redirect": the
// provider's URL}.
func (a *app) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	a.ssoBegin(w, r, sso.IntentLogin, "", "")
}

// handleSSOLink starts linking an identity at a provider to the session's
// account: {"password", "return"}. Like an SSH key, a new way in: a
// sign-in that gave the account's second factor (if it has one) - not a
// trusted browser's -, and its password again.
func (a *app) handleSSOLink(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	u, ok := a.auth.User(p.User)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !u.HasPassword || u.SSO != nil {
		writeError(w, http.StatusConflict, "this account signs in through its provider: it has nothing to link")
		return
	}
	if u.MFA.Enabled() && (!p.MFA || p.Trusted) {
		writeError(w, http.StatusForbidden, "this sign-in skipped the second factor (a trusted browser): confirm it with your second factor first")
		return
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	sum := sha256.Sum256([]byte(c.Value))
	a.ssoBegin(w, r, sso.IntentLink, p.User, hex.EncodeToString(sum[:]))
}

func (a *app) ssoBegin(w http.ResponseWriter, r *http.Request, intent, user, session string) {
	id := r.PathValue("id")
	noteAudit(r, user, "sso "+id)
	if a.sso == nil {
		writeError(w, http.StatusNotFound, "no single sign-on here")
		return
	}
	client := clientAddr(r)
	if ok, wait := a.sso.limiter.Allow(client); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		writeError(w, http.StatusTooManyRequests, "too many sign-ins started - try again later")
		return
	}
	var req struct {
		Return   string `json:"return"`
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Return == "" {
		req.Return = "#/"
	}
	if !sso.ValidReturn(req.Return) {
		writeError(w, http.StatusBadRequest, "return: a route of the page (#/...)")
		return
	}
	if intent == sso.IntentLink {
		if ok, wait := a.loginLimiter.Allow(client); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			writeError(w, http.StatusTooManyRequests, "too many failed attempts - try again later")
			return
		}
		if _, err := a.auth.Authenticate(user, req.Password); err != nil {
			a.loginLimiter.Fail(client)
			writeError(w, http.StatusBadRequest, auth.ErrWrongPassword.Error())
			return
		}
		a.loginLimiter.Succeed(client)
	}
	if a.auth.SetupRequired() {
		writeError(w, http.StatusConflict, "set the Controller up first")
		return
	}
	p, ok := a.sso.store.Get(id)
	if !ok || !p.Enabled {
		ssoFail(w, http.StatusNotFound, id, sso.CodeConfig, errors.New("no such provider, or disabled"))
		return
	}
	redirectURI, err := ssoRedirectURI(r)
	if err != nil {
		ssoFail(w, http.StatusBadRequest, id, sso.CodeConfig, err)
		return
	}
	c, err := a.ssoClient(r, p)
	if err != nil {
		ssoFail(w, http.StatusBadGateway, id, errCode(err, sso.CodeProvider), err)
		return
	}
	login := sso.Login{Provider: p.ID, Version: p.Version, State: sso.Random(32), Nonce: sso.Random(32), Verifier: oauth2.GenerateVerifier(),
		RedirectURI: redirectURI, Return: req.Return, Intent: intent, Session: session}
	key, err := a.sso.logins.Start(login)
	if err != nil {
		ssoFail(w, http.StatusServiceUnavailable, id, "busy", err)
		return
	}
	prompt := ""
	if intent == sso.IntentLink {
		// Whoever is signed in at the provider in this browser isn't
		// linked silently: the provider asks.
		prompt = "login"
	}
	http.SetCookie(w, ssoCookie(key, 600))
	writeJSON(w, http.StatusOK, map[string]string{"redirect": c.AuthURL(redirectURI, login.State, login.Nonce, login.Verifier, prompt)})
}

// handleSSOCallback is the provider sending the browser back: the code
// exchanged, the ID token checked - what came of it kept for the page,
// which finishes (handleSSOFinish).
func (a *app) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	back := func() { http.Redirect(w, r, ssoDoneRoute, http.StatusSeeOther) }
	c, err := r.Cookie(ssoCookieName)
	if err != nil || a.sso == nil {
		back() // finishing says it's over
		return
	}
	login, ok := a.sso.logins.Take(c.Value)
	if !ok {
		back()
		return
	}
	done := sso.Done{Login: login}
	failed := func(code string, err error) {
		done.Err = &sso.Error{Code: code, Err: err}
		a.sso.logins.Finish(c.Value, done)
		back()
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		// Its error_description isn't logged: free text, anyone's.
		failed(sso.CodeDenied, fmt.Errorf("the provider answered %s", strconv.Quote(truncate(e, 64))))
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(login.State)) != 1 {
		failed(sso.CodeExpired, errors.New("the state isn't this sign-in's"))
		return
	}
	if "https://"+strings.ToLower(r.Host)+ssoCallbackPath != login.RedirectURI {
		failed(sso.CodeConfig, fmt.Errorf("came back to %s, not %s", r.Host, login.RedirectURI))
		return
	}
	p, ok := a.sso.store.Get(login.Provider)
	switch {
	case !ok || !p.Enabled:
		failed(sso.CodeConfig, errors.New("the provider was removed or disabled during the sign-in"))
		return
	case p.Version != login.Version:
		failed(sso.CodeExpired, errors.New("the provider's settings changed during the sign-in"))
		return
	}
	// RFC 9207: a provider that names itself must be the one this
	// sign-in went to (a mix-up between providers).
	if iss := q.Get("iss"); iss != "" && iss != p.Issuer {
		failed(sso.CodeToken, fmt.Errorf("the answer is from %s", strconv.Quote(truncate(iss, 200))))
		return
	}
	client, err := a.ssoClient(r, p)
	if err != nil {
		failed(errCode(err, sso.CodeProvider), err)
		return
	}
	res, err := client.Exchange(r.Context(), login.RedirectURI, q.Get("code"), login.Verifier, login.Nonce)
	if err != nil {
		failed(errCode(err, sso.CodeProvider), err)
		return
	}
	a.logClaims(p, res)
	done.Result = res
	a.sso.logins.Finish(c.Value, done)
	back()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// logClaims logs, once per version of a provider's settings, the names
// of the claims its ID token carried - never their values: what to look
// at when a provider doesn't send what its settings expect.
func (a *app) logClaims(p sso.Provider, res *sso.Result) {
	key := p.ID + "\x00" + strconv.Itoa(p.Version)
	a.sso.mu.Lock()
	seen := a.sso.logged[key]
	a.sso.logged[key] = true
	a.sso.mu.Unlock()
	if seen {
		return
	}
	extra := ""
	if len(res.UserinfoClaims) > 0 {
		extra = "; from user info: " + strings.Join(res.UserinfoClaims, " ")
	}
	log.Printf("sso: %s: the ID token's claims: %s%s (%d groups)", p.ID, strings.Join(res.Claims, " "), extra, len(res.Groups))
}

// handleSSOFinish finishes a sign-in the provider sent the browser back
// from: the account signed in - or the identity linked - answering
// {"return": the route of the page to go back to}.
func (a *app) handleSSOFinish(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, ssoCookie("", -1))
	c, err := r.Cookie(ssoCookieName)
	if err != nil || a.sso == nil {
		ssoFail(w, http.StatusBadRequest, "", sso.CodeExpired, errors.New("no sign-in under way in this browser"))
		return
	}
	done, ok := a.sso.logins.TakeDone(c.Value)
	if !ok {
		ssoFail(w, http.StatusBadRequest, "", sso.CodeExpired, errors.New("no sign-in came back for this browser's"))
		return
	}
	id := done.Login.Provider
	if done.Err != nil {
		noteAudit(r, "", "sso "+id)
		ssoFail(w, http.StatusUnauthorized, id, done.Err.Code, done.Err.Err)
		return
	}
	res := done.Result
	noteAudit(r, res.Username, "sso "+id)
	p, ok := a.sso.store.Get(id)
	if !ok || !p.Enabled || p.Version != done.Login.Version {
		ssoFail(w, http.StatusConflict, id, sso.CodeExpired, errors.New("the provider's settings changed during the sign-in"))
		return
	}
	identity := auth.Identity{Provider: p.ID, Issuer: res.Issuer, Subject: res.Subject, Username: res.Username}
	access := p.Evaluate(res.Groups)
	if done.Login.Intent == sso.IntentLink {
		a.ssoFinishLink(w, r, done, identity, access)
		return
	}
	u, err := a.auth.SSOSignIn(identity, res.Groups, access, time.Duration(p.SignInEveryDays)*24*time.Hour)
	if err != nil {
		status, code := ssoAccountError(err)
		ssoFail(w, status, id, code, fmt.Errorf("sub %s, %d groups: %w", strconv.Quote(res.Subject), len(res.Groups), err))
		return
	}
	noteAudit(r, u.Name, "sso "+id)
	providerMFA := p.MFAMet(res.ACR, res.AMR)
	trusted := !providerMFA && a.trustedSignIn(r, u.Name)
	if old, err := r.Cookie(sessionCookieName); err == nil {
		a.auth.Revoke(old.Value) // a new sign-in, a new session
	}
	token, err := a.auth.NewSSOSession(u.Name, auth.SSOSession{Provider: p.ID, IDToken: res.IDToken, ProviderMFA: providerMFA, Trusted: trusted})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create session: "+err.Error())
		return
	}
	http.SetCookie(w, a.sessionCookie(token))
	writeJSON(w, http.StatusOK, map[string]string{"return": done.Login.Return})
}

func ssoAccountError(err error) (int, string) {
	switch {
	case errors.Is(err, auth.ErrNoAccess):
		return http.StatusForbidden, "access"
	case errors.Is(err, auth.ErrNameTaken):
		return http.StatusConflict, "name_taken"
	case errors.Is(err, auth.ErrNameInvalid):
		return http.StatusBadRequest, "name_invalid"
	case errors.Is(err, auth.ErrDisabled):
		return http.StatusForbidden, "disabled"
	case errors.Is(err, auth.ErrIdentityTaken):
		return http.StatusConflict, "identity_taken"
	case errors.Is(err, auth.ErrManaged):
		return http.StatusConflict, "link"
	}
	return http.StatusInternalServerError, sso.CodeConfig
}

// ssoFinishLink links the identity to the account of the session that
// asked - this browser's, still signed in.
func (a *app) ssoFinishLink(w http.ResponseWriter, r *http.Request, done sso.Done, identity auth.Identity, access auth.SSOAccess) {
	id := done.Login.Provider
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		ssoFail(w, http.StatusUnauthorized, id, "link", errors.New("no session to link to"))
		return
	}
	sum := sha256.Sum256([]byte(c.Value))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(done.Login.Session)) != 1 {
		ssoFail(w, http.StatusForbidden, id, "link", errors.New("finished by another session than the one that asked"))
		return
	}
	u, ss, ok := a.auth.Session(c.Value, true)
	if !ok || len(a.auth.Needs(u, ss)) > 0 {
		ssoFail(w, http.StatusUnauthorized, id, "link", errors.New("the session that asked is over"))
		return
	}
	noteAudit(r, u.Name, "sso "+id)
	if !access.Matched {
		ssoFail(w, http.StatusForbidden, id, "access", fmt.Errorf("link to %s: sub %s: no rule matches", u.Name, strconv.Quote(identity.Subject)))
		return
	}
	if _, err := a.auth.LinkIdentity(u.Name, identity); err != nil {
		status, code := ssoAccountError(err)
		ssoFail(w, status, id, code, fmt.Errorf("link to %s: %w", u.Name, err))
		return
	}
	log.Printf("sso: %s: %s linked the identity %s (%s)", id, u.Name, strconv.Quote(identity.Username), identity.Issuer)
	writeJSON(w, http.StatusOK, map[string]string{"return": done.Login.Return})
}

// handleIdentityUnlink forgets one of the identities linked to the
// session's own account.
func (a *app) handleIdentityUnlink(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	a.unlink(w, p.User, r.PathValue("id"))
}

// handleUserIdentityUnlink is an admin forgetting an identity linked to
// an account.
func (a *app) handleUserIdentityUnlink(w http.ResponseWriter, r *http.Request) {
	a.unlink(w, r.PathValue("name"), r.PathValue("id"))
}

func (a *app) unlink(w http.ResponseWriter, user, id string) {
	if err := a.auth.UnlinkIdentity(user, id); err != nil {
		writeError(w, userErrorStatus(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ssoLogoutURL is where the browser signs out of the session's provider
// too, if it says how ("" otherwise).
func (a *app) ssoLogoutURL(r *http.Request, ss auth.Session) string {
	if a.sso == nil || ss.Provider == "" {
		return ""
	}
	p, ok := a.sso.store.Get(ss.Provider)
	if !ok || !p.Enabled || p.LocalLogout {
		return ""
	}
	c, err := a.ssoClient(r, p)
	if err != nil {
		log.Printf("sso: %s: sign out at the provider: %v", p.ID, err)
		return ""
	}
	return c.EndSessionURL(ss.IDToken, "https://"+strings.ToLower(r.Host)+"/")
}
