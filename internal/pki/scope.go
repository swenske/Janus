package pki

import (
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"slices"
)

// Scoped certificates: a janusctl certificate for an account whose
// permissions differ from node to node - grants on labelled nodes, a
// token narrowed to some -, signed by the fleet, carries them: what it
// may do on every node, and on each node it names, by the SHA-256 of its
// CA's key (CAKey - the same through a CA rotation, whose cross-signed
// certificate the Controller pins: same key, another certificate).
//
// Its Subject.Organization is RoleScoped alone, no role: a node of an
// older release, which reads only that, refuses everything it asks. The
// extension is not critical: a critical extension Go doesn't know fails
// the TLS handshake before the node reads it - on this node as on older
// ones.

// RoleScoped stands in a scoped certificate's Subject.Organization.
const RoleScoped = "janus:scoped"

// OIDScope is the scope's extension.
//
// Under 2.999 (X.660's arc for examples, assigned to no one) until Janus
// has an enterprise number of its own: only Janus reads it, and only in
// certificates its fleets sign.
var OIDScope = asn1.ObjectIdentifier{2, 999, 1, 1}

// MaxScopeNodes is how many nodes one scoped certificate names at most -
// 32 bytes each, in a TLS handshake.
const MaxScopeNodes = 1000

const scopeVersion = 1

// ScopePerm is a role, narrowed to domains (internal/rbac; none: every
// domain).
type ScopePerm struct {
	Role    string
	Domains []string
}

// ScopeNodes are the nodes, by CAKey, with the same permissions.
type ScopeNodes struct {
	CAKeys [][]byte
	Perms  []ScopePerm
}

// Scope is what a scoped certificate may do: Any on every node (the
// account's role over all of them, when it has one), Nodes on those it
// names, besides.
type Scope struct {
	Version int
	Any     []ScopePerm
	Nodes   []ScopeNodes
}

// CAKey identifies a node in a scope: the SHA-256 of its CA's
// SubjectPublicKeyInfo.
func CAKey(ca *x509.Certificate) []byte {
	sum := sha256.Sum256(ca.RawSubjectPublicKeyInfo)
	return sum[:]
}

var humanRoles = []string{RoleAdmin, RoleOperator, RoleReader}

func (s *Scope) check() error {
	if s.Version != scopeVersion {
		return fmt.Errorf("scope version %d, this node knows %d", s.Version, scopeVersion)
	}
	n := 0
	for _, e := range s.Nodes {
		n += len(e.CAKeys)
		for _, k := range e.CAKeys {
			if len(k) != sha256.Size {
				return errors.New("a scope names a node by a key that isn't a SHA-256")
			}
		}
	}
	if n > MaxScopeNodes {
		return fmt.Errorf("a scope names %d nodes, %d at most", n, MaxScopeNodes)
	}
	for _, p := range s.perms() {
		if !slices.Contains(humanRoles, p.Role) {
			return fmt.Errorf("a scope gives the role %q", p.Role)
		}
	}
	return nil
}

func (s *Scope) perms() []ScopePerm {
	out := slices.Clone(s.Any)
	for _, e := range s.Nodes {
		out = append(out, e.Perms...)
	}
	return out
}

// Extension is s as the certificate extension.
func (s *Scope) Extension() (pkix.Extension, error) {
	c := *s
	c.Version = scopeVersion
	if err := c.check(); err != nil {
		return pkix.Extension{}, err
	}
	der, err := asn1.Marshal(c)
	if err != nil {
		return pkix.Extension{}, err
	}
	return pkix.Extension{Id: OIDScope, Value: der}, nil
}

// ParseScope reads cert's scope: ok false when it has none.
func ParseScope(cert *x509.Certificate) (s *Scope, ok bool, err error) {
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(OIDScope) {
			continue
		}
		if ok {
			return nil, true, errors.New("a certificate with two scopes")
		}
		ok = true
		s = &Scope{}
		rest, err := asn1.Unmarshal(ext.Value, s)
		if err != nil {
			return nil, true, fmt.Errorf("the certificate's scope: %w", err)
		}
		if len(rest) > 0 {
			return nil, true, errors.New("the certificate's scope: trailing data")
		}
		if err := s.check(); err != nil {
			return nil, true, err
		}
	}
	return s, ok, nil
}

// For is what s may do on the node whose CA's key is caKey (CAKey): Any,
// and the permissions of the entries naming it.
func (s *Scope) For(caKey []byte) []ScopePerm {
	out := slices.Clone(s.Any)
	for _, e := range s.Nodes {
		if slices.ContainsFunc(e.CAKeys, func(k []byte) bool { return string(k) == string(caKey) }) {
			out = append(out, e.Perms...)
		}
	}
	return out
}

// Roles are every role s gives, anywhere.
func (s *Scope) Roles() []string {
	var out []string
	for _, p := range s.perms() {
		if !slices.Contains(out, p.Role) {
			out = append(out, p.Role)
		}
	}
	return out
}

// Count is how many nodes s names.
func (s *Scope) Count() int {
	n := 0
	for _, e := range s.Nodes {
		n += len(e.CAKeys)
	}
	return n
}

// rolesOf are the roles leaf carries: Subject.Organization's, or, for a
// scoped certificate, its scope's - every role it gives somewhere.
func rolesOf(leaf *x509.Certificate) ([]string, error) {
	roles := leaf.Subject.Organization
	if !slices.Contains(roles, RoleScoped) {
		return roles, nil
	}
	if len(roles) != 1 {
		return nil, errors.New("a scoped certificate carries no role of its own")
	}
	s, ok, err := ParseScope(leaf)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("a scoped certificate without its scope")
	}
	return s.Roles(), nil
}
