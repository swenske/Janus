package main

import (
	"net/http"
	"slices"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/labels"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/rbac"
)

// What a request may do on a node: its account's role over everything -
// none for a scoped token -, and each of its grants whose selector the
// node's labels match, narrowed to the grant's domains; a token caps them
// at its role, and narrows them to its own selector and domains.

// anyone is a gate's role for a route every signed-in account may call:
// the handler shows only what the account reaches.
const anyone = auth.None

// perm is one way a request may act on a node: a role, narrowed to
// domains (nil: every domain).
type perm struct {
	role    auth.Role
	domains []string
}

var roleOrder = map[auth.Role]int{auth.Reader: 1, auth.Operator: 2, auth.Admin: 3}

// nodePerms is what p may do on a node with labels l, the strongest
// first; none when it may not reach it.
func (p principal) nodePerms(l map[string]string) []perm {
	if !labels.Match(p.Scope.Selector, l) {
		return nil
	}
	var out []perm
	if p.Base != auth.None {
		out = append(out, perm{role: p.Base})
	}
	for _, g := range p.Grants {
		if !labels.Match(g.Selector, l) {
			continue
		}
		r := g.Role
		if p.Cap != auth.None {
			r = auth.Lower(r, p.Cap)
		}
		out = append(out, perm{role: r, domains: g.Domains})
	}
	if len(p.Scope.Domains) > 0 {
		narrowed := out[:0]
		for _, pm := range out {
			d := p.Scope.Domains
			if pm.domains != nil {
				d = intersect(pm.domains, p.Scope.Domains)
			}
			if len(d) > 0 {
				narrowed = append(narrowed, perm{role: pm.role, domains: d})
			}
		}
		out = narrowed
	}
	slices.SortStableFunc(out, func(a, b perm) int { return roleOrder[b.role] - roleOrder[a.role] })
	return out
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

// sees reports whether p reaches the node at all.
func (p principal) sees(n *store.Node) bool {
	return len(p.nodePerms(n.LabelSet())) > 0
}

// mayOn reports whether p may act on the node with role at least need,
// in domain (empty: whatever the domain).
func (p principal) mayOn(n *store.Node, need auth.Role, domain string) bool {
	for _, pm := range p.nodePerms(n.LabelSet()) {
		if pm.role.AtLeast(need) && (domain == "" || pm.domains == nil || slices.Contains(pm.domains, domain)) {
			return true
		}
	}
	return false
}

// nodeUser is whom the Controller's calls to the node are made for: p's
// permissions there, as node roles and the node's domains - a permission
// over the machines domain alone only observes the node.
func (p principal) nodeUser(n *store.Node) nodeproxy.User {
	u := nodeproxy.User{Name: p.User}
	for _, pm := range p.nodePerms(n.LabelSet()) {
		np := nodeproxy.Perm{Role: nodeRole(pm.role)}
		if pm.domains != nil {
			np.Domains = intersect(pm.domains, rbac.NodeDomains)
			if len(np.Domains) == 0 {
				np.Domains = []string{rbac.DomainObserve}
			}
		}
		u.Perms = append(u.Perms, np)
	}
	return u
}

// visibleNodes are the nodes p reaches.
func (a *app) visibleNodes(p principal) []*store.Node {
	var out []*store.Node
	for _, n := range a.store.List() {
		if p.sees(n) {
			out = append(out, n)
		}
	}
	return out
}

// mayMachine reports whether p may act on machine m with role at least
// need: its role over everything, or one on the node the machine became,
// over the machines domain.
func (a *app) mayMachine(p principal, m *machines.Machine, need auth.Role) bool {
	if p.Role.AtLeast(need) {
		return true
	}
	n, ok := a.store.Get(m.NodeID)
	return m.NodeID != "" && ok && p.mayOn(n, need, auth.DomainMachines)
}

// seesMachine reports whether p sees machine m: a reader over
// everything, or whoever reaches the node it became.
func (a *app) seesMachine(p principal, m *machines.Machine) bool {
	if p.Role.AtLeast(auth.Reader) {
		return true
	}
	n, ok := a.store.Get(m.NodeID)
	return m.NodeID != "" && ok && p.sees(n)
}

// requestPrincipal is r's principal - set by the gates.
func requestPrincipal(r *http.Request) principal {
	p, _ := principalOf(r)
	return p
}
