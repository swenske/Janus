package main

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
)

// --- trusted browsers: skipping the second factor (internal/auth/trust.go) ---
//
// A sign-in that gives its second factor can ask to trust its browser:
// the browser keeps a secret in trustCookieName, which stands for the
// second factor at its next sign-ins until the policy's hours run out -
// the password is still asked. The cookie is scoped to /api/auth/: the
// sign-in reads it, nothing else is ever sent it.

const trustCookieName = "janus_trust"

func (a *app) registerTrustRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/trusted-browsers", a.sessionGate(anyone, anyone, a.handleTrustedList))
	mux.HandleFunc("DELETE /api/auth/trusted-browsers", a.sessionGate(anyone, anyone, a.handleTrustedForget))
	mux.HandleFunc("DELETE /api/auth/trusted-browsers/{id}", a.sessionGate(anyone, anyone, a.handleTrustedForget))
}

// trustBrowser trusts the browser of token's sign-in, which just gave
// its second factor. A failure only costs the browser its trust: the
// sign-in itself went through.
func (a *app) trustBrowser(w http.ResponseWriter, r *http.Request, token string) {
	value, expires, err := a.auth.TrustBrowser(token, browserLabel(r.UserAgent()), clientAddr(r))
	if err != nil {
		if !errors.Is(err, auth.ErrTrustOff) {
			log.Printf("trust a browser: %v", err)
		}
		return
	}
	http.SetCookie(w, trustCookie(value, int(time.Until(expires).Seconds())))
}

func trustCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: trustCookieName, Value: value, Path: "/api/auth/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
}

// trustedSignIn reports whether the request's browser is trusted for
// user's second factor.
func (a *app) trustedSignIn(r *http.Request, user string) bool {
	c, err := r.Cookie(trustCookieName)
	return err == nil && a.auth.TrustedSignIn(user, c.Value)
}

type trustedView struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Client     string     `json:"client"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// Current: the browser asking.
	Current bool `json:"current"`
}

func (a *app) handleTrustedList(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	list, err := a.auth.TrustedBrowsers(p.User)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	current := ""
	if c, err := r.Cookie(trustCookieName); err == nil {
		current, _, _ = strings.Cut(c.Value, "_")
	}
	out := []trustedView{}
	for _, t := range list {
		out = append(out, trustedView{ID: t.ID, Label: t.Label, Client: t.Client, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, Current: t.ID == current})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTrustedForget stops trusting one of the account's browsers -
// every one without an ID. This browser's cookie goes with its trust.
func (a *app) handleTrustedForget(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	id := r.PathValue("id")
	if err := a.auth.ForgetBrowser(p.User, id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if c, err := r.Cookie(trustCookieName); err == nil {
		if mine, _, _ := strings.Cut(c.Value, "_"); id == "" || mine == id {
			http.SetCookie(w, trustCookie("", -1))
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// browserLabel names a browser for its owner's list from its User-Agent
// - "Firefox on Linux" -, coarse on purpose: enough to tell one's
// browsers apart, nothing to identify them by.
func browserLabel(ua string) string {
	browser := ""
	for _, b := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Chromium/", "Chromium"}, {"Safari/", "Safari"},
	} {
		if strings.Contains(ua, b.token) {
			browser = b.name
			break
		}
	}
	system := ""
	for _, o := range []struct{ token, name string }{
		{"Android", "Android"}, {"iPhone", "iOS"}, {"iPad", "iPadOS"}, {"Windows", "Windows"}, {"Mac OS X", "macOS"}, {"CrOS", "ChromeOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, o.token) {
			system = o.name
			break
		}
	}
	switch {
	case browser != "" && system != "":
		return browser + " on " + system
	case browser != "":
		return browser
	case system != "":
		return "a browser on " + system
	}
	return "a browser"
}
