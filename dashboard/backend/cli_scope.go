package main

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/rbac"
)

// What a janusctl certificate carries. An account whose permissions are
// the same on every node - its role over all of them, no grant above it,
// no token narrowing it - gets a plain certificate with that role. Any
// other gets a scoped one (pki.Scope): its role over every node, when it
// has one, and what it may do besides on each node it reaches, by the
// node CA's key - as of now: a node labelled later needs a new sign-in.
// Each node checks it itself.

// errNoNode: the account reaches no node - no certificate to make.
var errNoNode = errors.New("reaches no node: there's nothing a certificate of janusctl would open")

// cliIdentity is a janusctl certificate's content.
type cliIdentity struct {
	// Role is a plain certificate's node role.
	Role string
	// Scope is a scoped certificate's.
	Scope *pki.Scope
	// Summary says what it carries (janusctl shows it).
	Summary string
}

// userPrincipal is what account u may do, limited to role limit - an SSH
// key's, a lower role asked at sign-in (none: the account's).
func userPrincipal(u auth.User, limit auth.Role) principal {
	return principal{User: u.Name, Role: u.Role, Base: u.Role, Grants: u.Grants, Max: u.MaxRole()}.capped(limit)
}

// capped is p limited to role limit (none: as it is).
func (p principal) capped(limit auth.Role) principal {
	if limit == auth.None {
		return p
	}
	p.Role, p.Base, p.Max = auth.Lower(limit, p.Role), auth.Lower(limit, p.Base), auth.Lower(limit, p.Max)
	if p.Cap == auth.None {
		p.Cap = limit
	} else {
		p.Cap = auth.Lower(limit, p.Cap)
	}
	return p
}

// cliIdentityFor is the certificate p gets.
func (a *app) cliIdentityFor(p principal) (cliIdentity, error) {
	plain := p.Base != auth.None && len(p.Scope.Selector) == 0 && len(p.Scope.Domains) == 0
	for _, g := range p.Grants {
		r := g.Role
		if p.Cap != auth.None {
			r = auth.Lower(r, p.Cap)
		}
		if roleOrder[r] > roleOrder[p.Base] {
			plain = false
		}
	}
	if plain {
		role := nodeRole(p.Base)
		return cliIdentity{Role: role, Summary: role}, nil
	}

	s := &pki.Scope{}
	if p.Base != auth.None && len(p.Scope.Selector) == 0 {
		s.Any = []pki.ScopePerm{scopePerm(perm{role: p.Base, domains: nilIfEmpty(p.Scope.Domains)})}
	}
	groups := map[string]int{}
	for _, n := range a.store.List() {
		var perms []pki.ScopePerm
		for _, pm := range p.nodePerms(n.LabelSet()) {
			sp := scopePerm(pm)
			if slices.ContainsFunc(s.Any, sameScopePerm(sp)) || slices.ContainsFunc(perms, sameScopePerm(sp)) {
				continue
			}
			perms = append(perms, sp)
		}
		if len(perms) == 0 {
			continue
		}
		key, err := nodeCAKey(n)
		if err != nil {
			log.Printf("node %s (%s): left out of %s's janusctl certificate: %v", n.Name, n.ID, p.User, err)
			continue
		}
		k := scopePermsString(perms)
		if i, ok := groups[k]; ok {
			s.Nodes[i].CAKeys = append(s.Nodes[i].CAKeys, key)
			continue
		}
		groups[k] = len(s.Nodes)
		s.Nodes = append(s.Nodes, pki.ScopeNodes{CAKeys: [][]byte{key}, Perms: perms})
	}
	if len(s.Any) == 0 && len(s.Nodes) == 0 {
		return cliIdentity{}, errNoNode
	}
	if s.Count() > pki.MaxScopeNodes {
		return cliIdentity{}, fmt.Errorf("reaches %d nodes beyond its role over all of them, more than a certificate names (%d)", s.Count(), pki.MaxScopeNodes)
	}
	return cliIdentity{Role: pki.RoleScoped, Scope: s, Summary: scopeSummary(s)}, nil
}

// scopePerm is pm as a node takes it: its node role, its domains among
// the node's - over the machines domain alone, it only observes.
func scopePerm(pm perm) pki.ScopePerm {
	sp := pki.ScopePerm{Role: nodeRole(pm.role)}
	if pm.domains != nil {
		sp.Domains = intersect(pm.domains, rbac.NodeDomains)
		if len(sp.Domains) == 0 {
			sp.Domains = []string{rbac.DomainObserve}
		}
	}
	return sp
}

func sameScopePerm(a pki.ScopePerm) func(pki.ScopePerm) bool {
	return func(b pki.ScopePerm) bool { return a.Role == b.Role && slices.Equal(a.Domains, b.Domains) }
}

func scopePermString(p pki.ScopePerm) string {
	if len(p.Domains) == 0 {
		return p.Role
	}
	return p.Role + " (" + strings.Join(p.Domains, ", ") + ")"
}

func scopePermsString(perms []pki.ScopePerm) string {
	parts := make([]string, len(perms))
	for i, p := range perms {
		parts[i] = scopePermString(p)
	}
	return strings.Join(parts, " + ")
}

// scopeSummary says what s gives: "scoped: os:reader everywhere,
// os:operator (haproxy) on 2 nodes".
func scopeSummary(s *pki.Scope) string {
	var parts []string
	if len(s.Any) > 0 {
		parts = append(parts, scopePermsString(s.Any)+" everywhere")
	}
	for _, e := range s.Nodes {
		on := "on 1 node"
		if len(e.CAKeys) > 1 {
			on = fmt.Sprintf("on %d nodes", len(e.CAKeys))
		}
		parts = append(parts, scopePermsString(e.Perms)+" "+on)
	}
	return "scoped: " + strings.Join(parts, ", ")
}

// nodeCAKey is how a scope names n: its CA's key (pki.CAKey) - the CA
// the Controller pins for it.
func nodeCAKey(n *store.Node) ([]byte, error) {
	block, _ := pem.Decode(n.CA())
	if block == nil {
		return nil, errors.New("no CA certificate")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return pki.CAKey(ca), nil
}
