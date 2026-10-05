package main

import (
	"net/http"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
)

// --- API tokens: /api/tokens ---
//
// Every account manages its own tokens, from its session only - never
// with a token; an admin sees and revokes everyone's.

type tokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Owner      string     `json:"owner"`
	Role       auth.Role  `json:"role"`
	Scope      tokenScope `json:"scope"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Expired    bool       `json:"expired"`
}

// tokenScope is a token's scope as the API shows it: never null.
type tokenScope struct {
	Selector map[string]string `json:"selector"`
	Domains  []string          `json:"domains"`
}

func viewToken(t auth.Token) tokenView {
	sel := t.Scope.Selector
	if sel == nil {
		sel = map[string]string{}
	}
	return tokenView{ID: t.ID, Name: t.Name, Owner: t.Owner, Role: t.Role, Scope: tokenScope{Selector: sel, Domains: nonNil(t.Scope.Domains)}, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, Expired: t.Expired(time.Now())}
}

func (a *app) registerTokenRoutes(mux *http.ServeMux) {
	// Every account manages its own - a scoped one too (its tokens for
	// Terraform).
	mux.HandleFunc("GET /api/tokens", a.sessionGate(anyone, anyone, a.handleTokenList))
	mux.HandleFunc("POST /api/tokens", a.sessionGate(anyone, anyone, a.handleTokenCreate))
	mux.HandleFunc("DELETE /api/tokens/{id}", a.sessionGate(anyone, anyone, a.handleTokenRevoke))
}

func (a *app) handleTokenList(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	owner := p.User
	if p.Role == auth.Admin {
		owner = ""
	}
	out := []tokenView{}
	for _, t := range a.tokens.List(owner) {
		out = append(out, viewToken(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	p, _ := principalOf(r)
	var req struct {
		Name string `json:"name"`
		// 0: until revoked.
		ExpiresInDays int `json:"expires_in_days"`
		// Role defaults to the account's; never more.
		Role auth.Role `json:"role"`
		// Scope narrows it to some nodes (labels) and domains: then it
		// reaches only those nodes, none of the Controller's own routes.
		Scope auth.TokenScope `json:"scope"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
		writeError(w, http.StatusBadRequest, "expires_in_days: 0 (never) to 3650")
		return
	}
	if req.Role == "" {
		req.Role = p.Max
	}
	if _, err := auth.ParseRole(string(req.Role)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !p.Max.AtLeast(req.Role) {
		writeError(w, http.StatusForbidden, "a token can't do more than its account: "+p.User+" is "+string(p.Max)+" at most")
		return
	}
	secret, t, err := a.tokens.Create(p.User, req.Role, req.Name, time.Duration(req.ExpiresInDays)*24*time.Hour, req.Scope)
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
	p, _ := principalOf(r)
	t, ok := a.tokens.Get(r.PathValue("id"))
	if !ok || (t.Owner != p.User && p.Role != auth.Admin) {
		writeError(w, http.StatusNotFound, "no such token")
		return
	}
	if err := a.tokens.Revoke(t.ID); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
