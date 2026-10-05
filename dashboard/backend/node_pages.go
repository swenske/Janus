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
// public like the main page's; its API needs a permission on the node -
// the account's role, or a grant whose labels the node has - an
// operator's for a change, and is relayed for the account, each call
// with the permission that allows it - the node checks its role and
// domains itself (os:reader, os:operator, os:admin). A node that
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
	a.gate(anyone, anyone, func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalOf(r)
		if !p.sees(n) {
			writeError(w, http.StatusForbidden, p.User+" reaches no permission on this node")
			return
		}
		if !safeMethod(r.Method) && !p.mayOn(n, auth.Operator, "") {
			writeError(w, http.StatusForbidden, "changing this node needs the operator role on it: "+p.User+" may only read")
			return
		}
		if !n.TrustsFleet() && p.Role != auth.Admin {
			writeError(w, http.StatusForbidden, "this node doesn't trust the fleet yet - the Controller reaches it as admin: only an admin opens its page until it's updated")
			return
		}
		// Each call it relays goes with the permission that allows it, or
		// is refused (nodeproxy.User) - and the node checks it again.
		ctx := nodeproxy.WithUser(r.Context(), p.nodeUser(n))
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
