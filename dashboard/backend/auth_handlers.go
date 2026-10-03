package main

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
)

const sessionCookieName = "janus_session"

// handleAuthStatus never requires auth itself - the SPA calls this on
// load to decide whether to show the first-run setup screen, the login
// screen, or the real app.
func (a *app) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	authenticated := false
	if c, err := r.Cookie(sessionCookieName); err == nil {
		authenticated = a.auth.ValidSession(c.Value)
	}
	writeJSON(w, http.StatusOK, struct {
		SetupRequired bool `json:"setup_required"`
		Authenticated bool `json:"authenticated"`
	}{SetupRequired: a.auth.SetupRequired(), Authenticated: authenticated})
}

// handleAuthSetup sets the admin password once, on first run only -
// Store.Setup itself refuses a second call, so this can't be used to
// silently reset an existing password.
func (a *app) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.auth.Setup(req.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.startSession(w)
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

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !a.auth.Verify(req.Password) {
		a.loginLimiter.Fail(client)
		// Deliberately generic - not "wrong password" vs "no such
		// account" (there's only ever one account anyway), no point
		// giving an attacker anything to distinguish.
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	a.loginLimiter.Succeed(client)
	a.startSession(w)
}

func (a *app) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		a.auth.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) startSession(w http.ResponseWriter) {
	token, err := a.auth.NewSession()
	if err != nil {
		http.Error(w, "create session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Secure is safe (not just cosmetic) now that this port is
	// HTTPS-only (see dashboard/backend/main.go's own doc comment) - a
	// browser never sends this cookie in the clear.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", MaxAge: 24 * 60 * 60,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// requireAuth gates a handler behind a valid session cookie or API token
// - wraps every /api/nodes* route, never the /api/auth/* routes
// themselves or static asset serving (the SPA has to load
// unauthenticated so it can render the login/setup screen in the first
// place). A program (a Terraform provider) sends "Authorization: Bearer
// <token>" (see internal/auth's TokenStore); a wrong one counts against
// the same per-address limit as a wrong password.
func (a *app) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			client := clientAddr(r)
			if ok, wait := a.loginLimiter.Allow(client); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
				http.Error(w, "too many failed attempts - try again later", http.StatusTooManyRequests)
				return
			}
			if _, ok := a.tokens.Verify(strings.TrimSpace(bearer)); !ok {
				a.loginLimiter.Fail(client)
				http.Error(w, "invalid or expired API token", http.StatusUnauthorized)
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), authKindKey{}, authToken)))
			return
		}
		if !a.validSession(r) {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authKindKey{}, authSession)))
	}
}

// requireSession gates a handler behind the admin's session only, never
// an API token: what manages the tokens themselves.
func (a *app) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.validSession(r) {
			http.Error(w, "this needs the admin's session, not an API token", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (a *app) validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	return err == nil && a.auth.ValidSession(c.Value)
}

func clientAddr(r *http.Request) string {
	client, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return client
}

// --- API tokens: /api/tokens ---

type tokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Expired    bool       `json:"expired"`
}

func viewToken(t auth.Token) tokenView {
	return tokenView{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, Expired: t.Expired(time.Now())}
}

func (a *app) handleTokenList(w http.ResponseWriter, _ *http.Request) {
	out := []tokenView{}
	for _, t := range a.tokens.List() {
		out = append(out, viewToken(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		// 0: until revoked.
		ExpiresInDays int `json:"expires_in_days"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
		writeError(w, http.StatusBadRequest, "expires_in_days: 0 (never) to 3650")
		return
	}
	secret, t, err := a.tokens.Create(req.Name, time.Duration(req.ExpiresInDays)*24*time.Hour)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		tokenView
		// Token is shown this once.
		Token string `json:"token"`
	}{viewToken(*t), secret})
}

func (a *app) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if err := a.tokens.Revoke(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
