package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/pki"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
)

const sessionCookieName = "janus_session"

// backgroundHeader marks a request the page makes by itself - a periodic
// refresh -, which doesn't keep an idle session alive.
const backgroundHeader = "X-Janus-Background"

// principal is who a request is from: an account, through its session or
// one of its API tokens.
type principal struct {
	User string
	// Role is what the request may do over everything - the Controller's
	// own routes, every node: the account's, or less for a token made
	// with less; none for an account without one, or a scoped token.
	Role auth.Role
	// Base is the account's role over every node, a token's cap applied
	// (a scoped token's too); Grants, its roles on some nodes (access.go).
	Base   auth.Role
	Grants []auth.Grant
	// Cap is a token's role (none for a session), Scope its selector and
	// domains.
	Cap   auth.Role
	Scope auth.TokenScope
	// Max is the most the account can do anywhere.
	Max auth.Role
	// Token is the API token's ID - empty for a session.
	Token string
	// Needs is what the session must do before anything else
	// (auth.Store.Needs): "mfa", "password", "mfa_enroll".
	Needs []string
	// MFA: a session that gave a second factor.
	MFA bool
}

type principalKey struct{}

func principalOf(r *http.Request) (principal, bool) {
	p, ok := r.Context().Value(principalKey{}).(principal)
	return p, ok
}

func (p principal) via() string {
	if p.Token != "" {
		return "token " + p.Token
	}
	return "session"
}

// nodeRole is the role a node gives the calls the Controller makes for an
// account (nodeproxy's impersonation): the same, by its name there.
func nodeRole(r auth.Role) string {
	switch r {
	case auth.Admin:
		return pki.RoleAdmin
	case auth.Operator:
		return pki.RoleOperator
	}
	return pki.RoleReader
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// gate lets a request in for an account whose role is at least read for a
// read (GET), write for anything else - by its session or an API token.
func (a *app) gate(read, write auth.Role, next http.HandlerFunc) http.HandlerFunc {
	return a.gated(read, write, false, next)
}

// sessionGate is gate for a session only, never an API token: what
// manages credentials - tokens, accounts, the fleet's.
func (a *app) sessionGate(read, write auth.Role, next http.HandlerFunc) http.HandlerFunc {
	return a.gated(read, write, true, next)
}

func (a *app) gated(read, write auth.Role, sessionOnly bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.authenticate(w, r)
		if !ok {
			return
		}
		noteAudit(r, p.User, p.via())
		if sessionOnly && p.Token != "" {
			writeError(w, http.StatusForbidden, "this needs a session on the Controller's page, not an API token")
			return
		}
		if len(p.Needs) > 0 {
			writeError(w, http.StatusForbidden, needsMessage[p.Needs[0]])
			return
		}
		need := write
		if safeMethod(r.Method) {
			need = read
		}
		if need != anyone && !p.Role.AtLeast(need) {
			have := string(p.Role)
			if have == "" {
				have = "only scoped"
			}
			writeError(w, http.StatusForbidden, fmt.Sprintf("this needs the %s role: %s is %s", need, p.User, have))
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		kind := authSession
		if p.Token != "" {
			kind = authToken
		}
		ctx = context.WithValue(ctx, authKindKey{}, kind)
		if !safeMethod(r.Method) && p.Role != auth.None {
			// What the request changes on a node, the node logs as this
			// account's.
			ctx = nodeproxy.WithUser(ctx, nodeproxy.User{Name: p.User, Perms: []nodeproxy.Perm{{Role: nodeRole(p.Role)}}})
		}
		next(w, r.WithContext(ctx))
	}
}

// authenticate finds the request's account - "Authorization: Bearer
// <token>" (a program: the Terraform provider) or the session cookie -,
// answering 401 (or 429) itself when there's none. A wrong token counts
// against the same per-address limit as a wrong password.
func (a *app) authenticate(w http.ResponseWriter, r *http.Request) (principal, bool) {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		client := clientAddr(r)
		if ok, wait := a.loginLimiter.Allow(client); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			http.Error(w, "too many failed attempts - try again later", http.StatusTooManyRequests)
			return principal{}, false
		}
		t, ok := a.tokens.Verify(strings.TrimSpace(bearer))
		var u auth.User
		if ok {
			u, ok = a.auth.User(t.Owner)
			ok = ok && !u.Disabled
		}
		if !ok {
			a.loginLimiter.Fail(client)
			http.Error(w, "invalid or expired API token", http.StatusUnauthorized)
			return principal{}, false
		}
		p := principal{User: u.Name, Role: auth.Lower(t.Role, u.Role), Base: auth.Lower(t.Role, u.Role), Grants: u.Grants, Cap: t.Role, Scope: t.Scope, Max: auth.Lower(t.Role, u.MaxRole()), Token: t.ID}
		if len(t.Scope.Selector) > 0 || len(t.Scope.Domains) > 0 {
			p.Role = auth.None // a scoped token reaches its nodes only
		}
		return p, true
	}
	u, ss, ok := a.session(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return principal{}, false
	}
	return principal{User: u.Name, Role: u.Role, Base: u.Role, Grants: u.Grants, Max: u.MaxRole(), Needs: a.auth.Needs(u, ss), MFA: ss.MFA}, true
}

var needsMessage = map[string]string{
	"mfa":        "finish signing in with your second factor",
	"password":   "change your password first",
	"mfa_enroll": "set up a second factor first: your role needs one",
}

// session is the request's live session - kept alive unless the page
// made the request by itself.
func (a *app) session(r *http.Request) (auth.User, auth.Session, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return auth.User{}, auth.Session{}, false
	}
	return a.auth.Session(c.Value, r.Header.Get(backgroundHeader) == "")
}

// --- signing in: /api/auth/* ---

type meView struct {
	Name string `json:"name"`
	// Role is over everything ("" for none); Grants on some nodes; Max
	// the most anywhere.
	Role   auth.Role    `json:"role"`
	Grants []auth.Grant `json:"grants"`
	Max    auth.Role    `json:"max_role"`
	Needs  []string     `json:"needs"`
	// ExpiresAt is when the session ends if it isn't used again.
	ExpiresAt time.Time `json:"expires_at"`
	MFA       mfaView   `json:"mfa"`
}

// handleAuthStatus never requires auth itself - the SPA calls this on
// load to decide whether to show the first-run setup screen, the sign-in
// screen, or the real app.
func (a *app) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	out := struct {
		SetupRequired bool    `json:"setup_required"`
		Authenticated bool    `json:"authenticated"`
		User          *meView `json:"user,omitempty"`
	}{SetupRequired: a.auth.SetupRequired()}
	if u, ss, ok := a.session(r); ok {
		out.Authenticated = true
		out.User = &meView{Name: u.Name, Role: u.Role, Grants: nonNil(u.Grants), Max: u.MaxRole(), Needs: nonNil(a.auth.Needs(u, ss)), ExpiresAt: ss.Expires(a.auth.Settings()), MFA: a.viewMFA(r, u)}
	}
	writeJSON(w, http.StatusOK, out)
}

type signIn struct {
	// Name defaults to "admin" - the account of a Controller from before
	// accounts, and what a script written for it signs in as.
	Name     string `json:"name"`
	Password string `json:"password"`
	// MFARequired is the policy for second factors, at setup only
	// (auth.Settings.MFARequired; empty: admins).
	MFARequired string `json:"mfa_required"`
}

func (c *signIn) name() string {
	if c.Name == "" {
		return auth.LegacyAdmin
	}
	return strings.TrimSpace(c.Name)
}

// handleAuthSetup makes the first account, an admin, on first run only -
// Store.Setup itself refuses once any account exists, so this can't be
// used to take over a Controller already set up.
func (a *app) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var req signIn
	if !decodeBody(w, r, &req) {
		return
	}
	noteAudit(r, req.name(), "")
	u, err := a.auth.Setup(req.name(), req.Password, req.MFARequired)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.startSession(w, u.Name, false)
}

func (a *app) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	// Throttled per client address (see auth.LoginLimiter), checked
	// before anything else so a locked-out address costs no bcrypt work.
	// Behind a reverse proxy every request shares the proxy's address,
	// making this effectively a global limit.
	client := clientAddr(r)
	if ok, wait := a.loginLimiter.Allow(client); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		http.Error(w, "too many failed attempts - try again later", http.StatusTooManyRequests)
		return
	}
	var req signIn
	if !decodeBody(w, r, &req) {
		return
	}
	noteAudit(r, req.name(), "")
	u, err := a.auth.Authenticate(req.name(), req.Password)
	if err != nil {
		a.loginLimiter.Fail(client)
		// Deliberately generic: never which of the name or the password
		// was wrong.
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	a.loginLimiter.Succeed(client)
	a.startSession(w, u.Name, false)
}

func (a *app) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if u, _, ok := a.auth.Session(c.Value, false); ok {
			noteAudit(r, u.Name, "session")
		}
		a.auth.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleAuthPassword changes the session's own password - the one thing
// a session that must change it may do. Every session of the account
// ends; this one starts again.
func (a *app) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	u, ss, ok := a.session(r)
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	noteAudit(r, u.Name, "session")
	if slices.Contains(a.auth.Needs(u, ss), "mfa") {
		writeError(w, http.StatusForbidden, needsMessage["mfa"])
		return
	}
	var req struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	client := clientAddr(r)
	if ok, wait := a.loginLimiter.Allow(client); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		http.Error(w, "too many failed attempts - try again later", http.StatusTooManyRequests)
		return
	}
	if err := a.auth.ChangePassword(u.Name, req.Current, req.New); err != nil {
		if strings.Contains(err.Error(), "current password") {
			a.loginLimiter.Fail(client)
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.startSession(w, u.Name, ss.MFA)
}

// startSession signs user in - mfa: the sign-in gave its second factor.
func (a *app) startSession(w http.ResponseWriter, user string, mfa bool) {
	token, err := a.auth.NewSession(user, mfa)
	if err != nil {
		http.Error(w, "create session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Secure is safe (not just cosmetic) now that this port is
	// HTTPS-only (see dashboard/backend/main.go's own doc comment) - a
	// browser never sends this cookie in the clear. The cookie lasts as
	// long as a session can; the Controller ends it sooner when idle.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", MaxAge: int(a.auth.Settings().Max().Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func clientAddr(r *http.Request) string {
	client, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return client
}

// --- accounts: /api/users (admin) ---

type userView struct {
	Name               string     `json:"name"`
	Role               auth.Role  `json:"role"`
	Disabled           bool       `json:"disabled"`
	MustChangePassword bool       `json:"must_change_password"`
	CreatedAt          time.Time  `json:"created_at"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
	Tokens             int        `json:"tokens"`
	// SSHKeys are the account's keys for janusctl.
	SSHKeys []auth.SSHKey `json:"ssh_keys"`
	// Grants are its roles on some nodes.
	Grants []auth.Grant `json:"grants"`
	// MFA: the account has a second factor.
	MFA bool `json:"mfa"`
	// Password is set once: the one the Controller made for a new
	// account or a reset, to hand over.
	Password string `json:"password,omitempty"`
}

func (a *app) viewUser(u auth.User) userView {
	return userView{Name: u.Name, Role: u.Role, Disabled: u.Disabled, MustChangePassword: u.MustChangePassword, CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt, Tokens: len(a.tokens.List(u.Name)), SSHKeys: nonNil(u.SSHKeys), Grants: nonNil(u.Grants), MFA: u.MFA.Enabled()}
}

func (a *app) registerUserRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/users", a.gate(auth.Admin, auth.Admin, a.handleUserList))
	mux.HandleFunc("POST /api/users", a.sessionGate(auth.Admin, auth.Admin, a.handleUserCreate))
	mux.HandleFunc("PATCH /api/users/{name}", a.sessionGate(auth.Admin, auth.Admin, a.handleUserUpdate))
	mux.HandleFunc("DELETE /api/users/{name}", a.sessionGate(auth.Admin, auth.Admin, a.handleUserDelete))
	mux.HandleFunc("DELETE /api/users/{name}/ssh-keys", a.sessionGate(auth.Admin, auth.Admin, a.handleUserSSHKeysRevoke))
	mux.HandleFunc("GET /api/settings", a.gate(auth.Admin, auth.Admin, a.handleSettingsGet))
	mux.HandleFunc("PUT /api/settings", a.sessionGate(auth.Admin, auth.Admin, a.handleSettingsSet))
	mux.HandleFunc("GET /api/audit", a.gate(auth.Admin, auth.Admin, a.handleAudit))
}

func (a *app) handleUserList(w http.ResponseWriter, _ *http.Request) {
	out := []userView{}
	for _, u := range a.auth.Users() {
		out = append(out, a.viewUser(u))
	}
	writeJSON(w, http.StatusOK, out)
}

// newPassword is one the Controller makes for an account, to hand over:
// 20 characters, 120 bits.
func newPassword() (string, error) {
	raw := make([]byte, 15)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (a *app) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		// Role over everything: reader, operator, admin, or "none" -
		// then only its grants.
		Role   string       `json:"role"`
		Grants []auth.Grant `json:"grants"`
		// Password is optional: without one, the Controller makes one,
		// answered once. Either way its owner changes it at the first
		// sign-in.
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	role, err := auth.ParseAccountRole(req.Role)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	made := req.Password == ""
	if made {
		var err error
		if req.Password, err = newPassword(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	u, err := a.auth.CreateUser(strings.TrimSpace(req.Name), role, req.Password, req.Grants)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	v := a.viewUser(u)
	if made {
		v.Password = req.Password
	}
	writeJSON(w, http.StatusCreated, v)
}

func (a *app) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role     *string       `json:"role"`
		Grants   *[]auth.Grant `json:"grants"`
		Disabled *bool         `json:"disabled"`
		// ResetPassword gives the account a new password the Controller
		// makes, answered once - its sessions end, and its owner changes
		// it at the next sign-in.
		ResetPassword bool `json:"reset_password"`
		// ResetMFA takes its second factors away (a lost phone or key):
		// its sessions end; it sets one up again if its role needs one.
		ResetMFA bool `json:"reset_mfa"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ResetMFA {
		if err := a.auth.ResetMFA(r.PathValue("name")); err != nil {
			writeError(w, userErrorStatus(err), err.Error())
			return
		}
	}
	c := auth.Change{Grants: req.Grants, Disabled: req.Disabled}
	if req.Role != nil {
		role, err := auth.ParseAccountRole(*req.Role)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		c.Role = &role
	}
	var password string
	if req.ResetPassword {
		var err error
		if password, err = newPassword(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		c.Password = &password
	}
	u, err := a.auth.UpdateUser(r.PathValue("name"), c)
	if err != nil {
		writeError(w, userErrorStatus(err), err.Error())
		return
	}
	v := a.viewUser(u)
	v.Password = password
	writeJSON(w, http.StatusOK, v)
}

func userErrorStatus(err error) int {
	switch {
	case errors.Is(err, auth.ErrNoUser):
		return http.StatusNotFound
	case errors.Is(err, auth.ErrLastAdmin):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// handleUserSSHKeysRevoke takes every SSH key away from an account (a
// lost laptop): janusctl signs in with none of them any more; the
// certificates they got end within twelve hours.
func (a *app) handleUserSSHKeysRevoke(w http.ResponseWriter, r *http.Request) {
	n, err := a.auth.RemoveSSHKeys(r.PathValue("name"))
	if err != nil {
		writeError(w, userErrorStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"removed": n})
}

// handleUserDelete removes an account - its tokens with it.
func (a *app) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := a.auth.DeleteUser(name); err != nil {
		writeError(w, userErrorStatus(err), err.Error())
		return
	}
	if err := a.tokens.RevokeOwner(name); err != nil {
		// The account is gone: its tokens can't act any more anyway
		// (authenticate checks their owner) - only left on disk.
		log.Printf("revoke %s's API tokens: %v", name, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleSettingsGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, settingsView(a.auth.Settings()))
}

// settingsView is the policy with its MFA default spelled out.
func settingsView(p auth.Settings) auth.Settings {
	p.MFARequired = p.MFAPolicy()
	return p
}

// handleSettingsSet changes the fields given, the others kept.
func (a *app) handleSettingsSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionIdleMinutes *int    `json:"session_idle_minutes"`
		SessionMaxHours    *int    `json:"session_max_hours"`
		MFARequired        *string `json:"mfa_required"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	next := a.auth.Settings()
	if req.SessionIdleMinutes != nil {
		next.SessionIdleMinutes = *req.SessionIdleMinutes
	}
	if req.SessionMaxHours != nil {
		next.SessionMaxHours = *req.SessionMaxHours
	}
	if req.MFARequired != nil {
		next.MFARequired = *req.MFARequired
	}
	if err := a.auth.SetSettings(next); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settingsView(a.auth.Settings()))
}

func (a *app) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	user := r.URL.Query().Get("user")
	var keep func(audit.Entry) bool
	if user != "" {
		keep = func(e audit.Entry) bool { return e.User == user }
	}
	out, err := a.audit.Recent(limit, keep)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, nonNil(out))
}

// --- the audit: every request that changes something ---

type auditNote struct{ user, via string }

type auditNoteKey struct{}

// noteAudit tells the audit who made r, as soon as it's known.
func noteAudit(r *http.Request, user, via string) {
	if n, ok := r.Context().Value(auditNoteKey{}).(*auditNote); ok {
		n.user, n.via = user, via
	}
}

// auditedReads are reads worth recording: a secret leaving the Controller.
var auditedReads = map[string]bool{"/api/fleet/recovery-kit": true}

// audited records every API request that isn't a read - the
// Controller's and its node pages' - and the reads of auditedReads, with
// its account and outcome, in the audit and the process's log.
func (a *app) audited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := strings.HasPrefix(r.URL.Path, "/api/") || (strings.HasPrefix(r.URL.Path, "/nodes/") && strings.Contains(r.URL.Path, "/api/"))
		if !api || (safeMethod(r.Method) && !auditedReads[r.URL.Path]) {
			next.ServeHTTP(w, r)
			return
		}
		n := &auditNote{}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), auditNoteKey{}, n)))
		if rec.status == http.StatusPreconditionRequired {
			return // a poll still waiting (janusctl login -device): nothing happened
		}
		e := audit.Entry{Time: time.Now().UTC(), User: n.user, Via: n.via, Method: r.Method, Path: r.URL.Path, Status: rec.status, Client: clientAddr(r)}
		log.Printf("audit: %s %s %d: %s (%s) from %s", e.Method, e.Path, e.Status, or(e.User, "-"), or(e.Via, "no credential"), e.Client)
		if a.audit == nil {
			return
		}
		if err := a.audit.Append(e); err != nil {
			log.Printf("audit: write: %v", err)
		}
	})
}

// statusRecorder keeps the status a handler answered.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection (flushing a
// stream).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// nonNil is s, or an empty slice - JSON [] rather than null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
