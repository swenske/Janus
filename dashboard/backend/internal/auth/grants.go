package auth

import (
	"errors"
	"fmt"
	"slices"

	"github.com/swenske/Janus/dashboard/backend/internal/labels"
	"github.com/swenske/Janus/internal/rbac"
)

// Scoped permissions: besides its role over everything (or none), an
// account can have grants - a role on the nodes whose labels match a
// selector, narrowed to some domains: what a node's RPC is about
// (internal/rbac) and the Controller's own "machines" (their power,
// console, size, network, version). A token narrows its account's
// permissions further: a role at most, a selector, domains.

// None is an account without a role over everything: it reaches only
// what its grants give it.
const None Role = ""

// DomainMachines is the Controller's domain: a machine's power, console,
// size, network and version.
const DomainMachines = "machines"

// Domains are the domains a grant or a token may name.
var Domains = append(slices.Clone(rbac.NodeDomains), DomainMachines)

// MaxGrants is the most grants an account has.
const MaxGrants = 32

// Grant is a role on the nodes whose labels match Selector (all of them
// when empty), narrowed to Domains (none: every domain).
type Grant struct {
	Role     Role              `json:"role"`
	Selector map[string]string `json:"selector,omitempty"`
	Domains  []string          `json:"domains,omitempty"`
}

// ParseAccountRole is a role an account may have: one of the three, or
// none ("" or "none").
func ParseAccountRole(s string) (Role, error) {
	if s == "" || s == "none" {
		return None, nil
	}
	return ParseRole(s)
}

// CheckDomains refuses a domain nobody knows, or one twice.
func CheckDomains(domains []string) error {
	for i, d := range domains {
		if !slices.Contains(Domains, d) {
			return fmt.Errorf("unknown domain %q (%v)", d, Domains)
		}
		if slices.Contains(domains[:i], d) {
			return fmt.Errorf("domain %q twice", d)
		}
	}
	return nil
}

// CheckGrants refuses grants that aren't well made.
func CheckGrants(grants []Grant) error {
	if len(grants) > MaxGrants {
		return fmt.Errorf("%d grants: %d at most", len(grants), MaxGrants)
	}
	for i, g := range grants {
		if _, err := ParseRole(string(g.Role)); err != nil {
			return fmt.Errorf("grant %d: %w", i+1, err)
		}
		if err := labels.Check(g.Selector); err != nil {
			return fmt.Errorf("grant %d: %w", i+1, err)
		}
		if err := CheckDomains(g.Domains); err != nil {
			return fmt.Errorf("grant %d: %w", i+1, err)
		}
	}
	return nil
}

// MaxRole is the most the account can do anywhere: its role, or a
// grant's.
func (u User) MaxRole() Role {
	r := u.Role
	for _, g := range u.Grants {
		if roleRank[g.Role] > roleRank[r] {
			r = g.Role
		}
	}
	return r
}

// TokenScope narrows a token beyond its role: only nodes matching
// Selector (all when empty), only Domains (all when none).
type TokenScope struct {
	Selector map[string]string `json:"selector,omitempty"`
	Domains  []string          `json:"domains,omitempty"`
}

// Check refuses a scope that isn't well made.
func (s TokenScope) Check() error {
	if err := labels.Check(s.Selector); err != nil {
		return err
	}
	if err := CheckDomains(s.Domains); err != nil {
		return err
	}
	return nil
}

// ErrNoRole is an account's role "none" where one is needed.
var ErrNoRole = errors.New("no role")
