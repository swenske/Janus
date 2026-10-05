package main

import (
	"net/http"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/enroll"
)

// Enrollment tokens (internal/enroll): an admin's, from a session - each
// admits a batch of nodes without the approval step, labelled.

type enrollView struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	MaxUses   int               `json:"max_uses"`
	Uses      int               `json:"uses"`
	Labels    map[string]string `json:"labels"`
	CreatedBy string            `json:"created_by"`
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt time.Time         `json:"expires_at"`
	Usable    bool              `json:"usable"`
	Nodes     []string          `json:"nodes"`
}

func viewEnroll(t enroll.Token) enrollView {
	l := t.Labels
	if l == nil {
		l = map[string]string{}
	}
	return enrollView{ID: t.ID, Name: t.Name, MaxUses: t.MaxUses, Uses: t.Uses, Labels: l, CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, Usable: t.Usable(time.Now()), Nodes: nonNil(t.Nodes)}
}

func (a *app) registerEnrollRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/enroll-tokens", a.sessionGate(auth.Admin, auth.Admin, func(w http.ResponseWriter, _ *http.Request) {
		out := []enrollView{}
		for _, t := range a.enroll.List() {
			out = append(out, viewEnroll(t))
		}
		writeJSON(w, http.StatusOK, out)
	}))
	mux.HandleFunc("POST /api/enroll-tokens", a.sessionGate(auth.Admin, auth.Admin, a.handleEnrollCreate))
	mux.HandleFunc("DELETE /api/enroll-tokens/{id}", a.sessionGate(auth.Admin, auth.Admin, func(w http.ResponseWriter, r *http.Request) {
		if err := a.enroll.Revoke(r.PathValue("id")); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (a *app) handleEnrollCreate(w http.ResponseWriter, r *http.Request) {
	p := requestPrincipal(r)
	var req struct {
		Name          string            `json:"name"`
		MaxUses       int               `json:"max_uses"`
		ExpiresInDays int               `json:"expires_in_days"`
		Labels        map[string]string `json:"labels"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	secret, t, err := a.enroll.Create(req.Name, p.User, req.MaxUses, req.ExpiresInDays, req.Labels)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		enrollView
		// Token is shown this once.
		Token string `json:"token"`
	}{viewEnroll(t), secret})
}
