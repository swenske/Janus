package main

import (
	"net/http"
	"strings"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
)

// handleNodePage serves /nodes/<id>/...: a node's page and its API
// (internal/nodeproxy), on the Controller's own origin, behind its
// accounts. The page itself (not /api/) is the node app's static files,
// public like the main page's; its API needs a reader for a read and an
// operator for a change, and is relayed for the account - the node
// checks its role itself (os:reader, os:operator, os:admin). A node that
// doesn't trust the fleet yet is reached with its service credential, an
// admin's: only an admin may use its page until it's updated.
func (a *app) handleNodePage(w http.ResponseWriter, r *http.Request) {
	id, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/nodes/"), "/")
	if !ok {
		http.Redirect(w, r, "/nodes/"+id+"/", http.StatusMovedPermanently)
		return
	}
	n, found := a.store.Get(id)
	if !found {
		http.NotFound(w, r)
		return
	}
	h, err := a.nodePage(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	page := http.StripPrefix("/nodes/"+id, h)
	if !strings.HasPrefix(rest, "api/") {
		page.ServeHTTP(w, r)
		return
	}
	a.gate(auth.Reader, auth.Operator, func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalOf(r)
		if !n.TrustsFleet() && p.Role != auth.Admin {
			writeError(w, http.StatusForbidden, "this node doesn't trust the fleet yet - the Controller reaches it as admin: only an admin opens its page until it's updated")
			return
		}
		ctx := nodeproxy.WithUser(r.Context(), nodeproxy.User{Name: p.User, Roles: []string{nodeRole(p.Role)}})
		page.ServeHTTP(w, r.WithContext(ctx))
	})(w, r)
}

// nodePage is node id's page handler, made once.
func (a *app) nodePage(id string) (http.Handler, error) {
	a.pagesMu.Lock()
	defer a.pagesMu.Unlock()
	if h, ok := a.pages[id]; ok {
		return h, nil
	}
	n, _ := a.store.Get(id)
	h, err := nodeproxy.Handler(n, a.store)
	if err != nil {
		return nil, err
	}
	if a.pages == nil {
		a.pages = map[string]http.Handler{}
	}
	a.pages[id] = h
	return h, nil
}

// forgetNodePage drops a removed node's page.
func (a *app) forgetNodePage(id string) {
	a.pagesMu.Lock()
	delete(a.pages, id)
	a.pagesMu.Unlock()
}
