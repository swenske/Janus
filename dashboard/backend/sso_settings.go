package main

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/sso"
)

// --- single sign-on settings: /api/sso/* (admin) ---
//
// Changing a provider is changing who gets in as what: a session's only,
// never an API token's.

func (a *app) registerSSOSettingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sso/providers", a.gate(auth.Admin, auth.Admin, a.handleSSOProviders))
	mux.HandleFunc("PUT /api/sso/providers/{id}", a.sessionGate(auth.Admin, auth.Admin, a.handleSSOProviderPut))
	mux.HandleFunc("DELETE /api/sso/providers/{id}", a.sessionGate(auth.Admin, auth.Admin, a.handleSSOProviderDelete))
	mux.HandleFunc("POST /api/sso/test", a.sessionGate(auth.Admin, auth.Admin, a.handleSSOTest))
}

type ssoProvidersView struct {
	Providers []sso.View              `json:"providers"`
	Presets   map[string]sso.Provider `json:"presets"`
	// RedirectURI and PostLogoutURI are what to register at a provider,
	// for the Controller as this page opened it.
	RedirectURI   string `json:"redirect_uri"`
	PostLogoutURI string `json:"post_logout_uri"`
}

func (a *app) handleSSOProviders(w http.ResponseWriter, r *http.Request) {
	if a.sso == nil {
		writeError(w, http.StatusNotFound, "no single sign-on here")
		return
	}
	host := strings.ToLower(r.Host)
	writeJSON(w, http.StatusOK, ssoProvidersView{
		Providers: a.sso.store.List(), Presets: sso.Presets(),
		RedirectURI: "https://" + host + ssoCallbackPath, PostLogoutURI: "https://" + host + "/",
	})
}

// ssoProviderRequest is a provider's settings, whole, and its client
// secret: absent keeps it, "" removes it (a public client).
type ssoProviderRequest struct {
	sso.Provider
	ClientSecret *string `json:"client_secret"`
}

// handleSSOProviderPut saves a provider - new, or its settings replaced
// whole. Version, when given, must be the saved one's (someone else's
// change isn't overwritten). The accounts it made get what its rules
// give them at once; disabling it ends its sessions.
func (a *app) handleSSOProviderPut(w http.ResponseWriter, r *http.Request) {
	if a.sso == nil {
		writeError(w, http.StatusNotFound, "no single sign-on here")
		return
	}
	var req ssoProviderRequest
	if !decodeBody(w, r, &req) {
		return
	}
	p := req.Provider
	if p.ID == "" {
		p.ID = r.PathValue("id")
	}
	if p.ID != r.PathValue("id") {
		writeError(w, http.StatusBadRequest, "id: the URL's")
		return
	}
	if old, ok := a.sso.store.Get(p.ID); ok && p.Version != 0 && p.Version != old.Version {
		writeError(w, http.StatusConflict, "someone changed this provider since you opened it: reload")
		return
	}
	if p.Rules == nil {
		p.Rules = []sso.Rule{}
	}
	saved, err := a.sso.store.Put(p, req.ClientSecret)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !saved.Enabled {
		a.auth.EndProviderSessions(saved.ID)
	}
	if n, err := a.auth.ResyncSSO(saved.ID, saved.Evaluate, time.Duration(saved.SignInEveryDays)*24*time.Hour); err != nil {
		log.Printf("sso: %s: apply the rules to its accounts: %v", saved.ID, err)
	} else if n > 0 {
		log.Printf("sso: %s: the rules changed %d of its accounts", saved.ID, n)
	}
	for _, v := range a.sso.store.List() {
		if v.ID == saved.ID {
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
}

// handleSSOProviderDelete removes a provider: its sessions end, the
// accounts it made lapse (an admin can delete them, or make one local
// with dashboardd reset-user); identities linked to accounts stay, for a
// provider added again with the same issuer.
func (a *app) handleSSOProviderDelete(w http.ResponseWriter, r *http.Request) {
	if a.sso == nil {
		writeError(w, http.StatusNotFound, "no single sign-on here")
		return
	}
	id := r.PathValue("id")
	if err := a.sso.store.Delete(id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, sso.ErrNoProvider) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	a.sso.clients.Forget(id)
	a.auth.EndProviderSessions(id)
	if err := a.auth.LapseProvider(id); err != nil {
		log.Printf("sso: %s: its accounts' lapse: %v", id, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSSOTest checks a provider's settings as the page has them - saved
// or not: its discovery and keys fetched the way a sign-in will.
func (a *app) handleSSOTest(w http.ResponseWriter, r *http.Request) {
	if a.sso == nil {
		writeError(w, http.StatusNotFound, "no single sign-on here")
		return
	}
	var req ssoProviderRequest
	if !decodeBody(w, r, &req) {
		return
	}
	hasSecret := req.ClientSecret != nil && *req.ClientSecret != ""
	if req.ClientSecret == nil {
		for _, v := range a.sso.store.List() {
			if v.ID == req.ID {
				hasSecret = v.HasSecret
			}
		}
	}
	writeJSON(w, http.StatusOK, sso.Check(r.Context(), req.Provider, hasSecret))
}
