package main

import (
	"errors"
	"net/http"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
)

// The signed-in account's preferences (internal/auth/preferences.go): the
// Settings tab, and what the Nodes, Hypervisors and node pages follow.
// A session's only - an API token has no pages.
func (a *app) registerPreferenceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/preferences", a.sessionGate(anyone, anyone, a.handlePreferencesGet))
	mux.HandleFunc("PUT /api/me/preferences", a.sessionGate(anyone, anyone, a.handlePreferencesPut))
}

// preferencesView is what GET answers: the account's choices with the
// defaults filled in and raised to the Controller's floors, what it chose
// itself, and what may be chosen under those floors.
type preferencesView struct {
	auth.Preferences
	Chosen   auth.Preferences `json:"chosen"`
	Defaults auth.Preferences `json:"defaults"`
	Choices  map[string][]int `json:"choices"`
	Floors   map[string]int   `json:"floors"`
}

func (a *app) preferencesOf(name string) (preferencesView, bool) {
	u, ok := a.auth.User(name)
	if !ok {
		return preferencesView{}, false
	}
	s := a.auth.Settings()
	return preferencesView{
		Preferences: u.Preferences.Effective().Floored(s),
		Chosen:      u.Preferences,
		Defaults:    auth.DefaultPreferences.Floored(s),
		Choices:     auth.Choices(s),
		Floors:      map[string]int{"nodes": s.NodesRefreshFloorSeconds, "hypervisors": s.HypervisorsRefreshFloorSeconds},
	}, true
}

func (a *app) handlePreferencesGet(w http.ResponseWriter, r *http.Request) {
	v, ok := a.preferencesOf(requestPrincipal(r).User)
	if !ok {
		writeError(w, http.StatusNotFound, "no such account")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handlePreferencesPut replaces the account's preferences: a field left
// out takes its default again (Settings' Reset sends {}).
func (a *app) handlePreferencesPut(w http.ResponseWriter, r *http.Request) {
	var p auth.Preferences
	if !decodeBody(w, r, &p) {
		return
	}
	name := requestPrincipal(r).User
	if err := p.CheckFloors(a.auth.Settings()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := a.auth.SetPreferences(name, p); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, auth.ErrNoUser) {
			code = http.StatusNotFound
		}
		writeError(w, code, err.Error())
		return
	}
	v, _ := a.preferencesOf(name)
	writeJSON(w, http.StatusOK, v)
}
