// Role enforcement: mTLS (internal/pki, wired up in cmd/janusd)
// proves *who* is calling - this file decides *what* they're allowed to
// call, based on the role(s) carried in their verified client
// certificate's Subject.Organization.
//
// requiredRoles (rbac.Required) is fail-closed by design: a method with no entry defaults
// to admin-only rather than being silently open. Every RPC in
// api/proto/janus/v1alpha1 is listed below deliberately, so a new RPC
// that forgets to be added here is caught immediately (it'll be
// admin-only until someone decides otherwise, never accidentally
// reader-accessible).
//
// The roles each RPC needs are in internal/rbac (shared with the
// Controller).
//
// The Controller's certificate (pki.RoleController) has no right of its
// own: it says whom it acts for - janus-as-user, janus-as-roles in the
// call's metadata - and the call gets that user's roles. A scoped
// certificate (pki.Scope) gives the roles and domains it names for this
// node, by its CA's key. Every call that isn't read-only is logged with
// who made it, and kept in the audit with how it ended (audit.go), as is
// every call refused.
package api

import (
	"context"
	"crypto/x509"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/rbac"
)

// requiredRoles is rbac.Required: the tests add to it.
var requiredRoles = rbac.Required

// Caller is whom an RPC runs for.
type Caller struct {
	Name  string // the certificate's common name, or the user the Controller acts for
	Roles []string
	Via   string // the Controller certificate's name, when it acts for Name
	// Fleet: the certificate is the fleet's (janusctl signed in to the
	// Controller), not one the node's own CA issued - an "admin" of each
	// isn't the same.
	Fleet bool
	// Domains narrow what the caller may do (internal/rbac); none: all.
	Domains []string
	// Scoped: the roles and domains are those a scoped certificate
	// gives on this node.
	Scoped bool
	// Member: another member of this node's cluster, by its member
	// certificate (internal/cluster) - Name is its member name.
	Member bool
	// ClusterApply is the cluster a call applies for
	// (pki.ClusterApplyKey) - what lets a write through a member's
	// cluster lock: a Controller's, or an admin's when the locks are
	// advisory (checkClusterLock).
	ClusterApply string
	// Transfer: a new node's state transfer certificate, by its SHA-256
	// - one StateExport while its grant lives (state_grant.go).
	Transfer string
}

func (c Caller) String() string {
	s := c.Name + " (" + strings.Join(c.Roles, ", ")
	if c.Fleet && c.Via == "" {
		s += ", fleet"
	}
	if c.Scoped {
		s += ", scoped"
	}
	if c.Member {
		s += ", member"
	}
	if c.Transfer != "" {
		s += ", state transfer"
	}
	if len(c.Domains) > 0 {
		s += "; " + strings.Join(c.Domains, ", ")
	}
	s += ")"
	if c.Via != "" {
		s += " via " + c.Via
	}
	return s
}

// FleetRoot is the node's fleet's root, if it has one (janusd sets it):
// a certificate chained to it is the fleet's.
var FleetRoot func() *x509.Certificate

// LocalCA is the node's own CA (janusd sets it): what a scoped
// certificate names the node by.
var LocalCA func() *x509.Certificate

// ClusterMember checks a cluster member's certificate chain against the
// node's own cluster (janusd sets it, internal/cluster.Store.AcceptMember):
// the member's name. Nil: no member gets in.
var ClusterMember func(chain []*x509.Certificate) (string, error)

type callerKey struct{}

// CallerFrom is whom the RPC running with ctx is for.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// UnaryAuthInterceptor enforces requiredRoles for unary RPCs - and keeps
// the call in the audit (AuditLog).
func UnaryAuthInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	caller, err := authorize(ctx, info.FullMethod)
	if err == nil {
		err = checkClusterLock(info.FullMethod, req, caller)
	}
	if err != nil {
		recordCall(ctx, info.FullMethod, caller, true, err, 0)
		return nil, err
	}
	audit(caller, info.FullMethod)
	start := time.Now()
	resp, err := handler(context.WithValue(ctx, callerKey{}, caller), req)
	recordCall(ctx, info.FullMethod, caller, false, err, time.Since(start))
	return resp, err
}

// StreamAuthInterceptor enforces requiredRoles for streaming RPCs - and
// keeps the call in the audit once it's done.
func StreamAuthInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	caller, err := authorize(ss.Context(), info.FullMethod)
	if err == nil {
		err = checkClusterLock(info.FullMethod, nil, caller)
	}
	if err != nil {
		recordCall(ss.Context(), info.FullMethod, caller, true, err, 0)
		return err
	}
	audit(caller, info.FullMethod)
	start := time.Now()
	err = handler(srv, callerStream{ss, context.WithValue(ss.Context(), callerKey{}, caller)})
	recordCall(ss.Context(), info.FullMethod, caller, false, err, time.Since(start))
	return err
}

type callerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s callerStream) Context() context.Context { return s.ctx }

// audit logs a call that isn't read-only, with whom it's for.
func audit(c Caller, fullMethod string) {
	if slices.Contains(rolesFor(fullMethod), pki.RoleReader) {
		return
	}
	log.Printf("api: %s: %s", strings.TrimPrefix(fullMethod, "/janus.v1alpha1."), c)
}

func rolesFor(fullMethod string) []string {
	return rbac.RolesFor(fullMethod)
}

func checkRole(ctx context.Context, fullMethod string) error {
	_, err := authorize(ctx, fullMethod)
	return err
}

// humanRoles are the roles the Controller may act with.
var humanRoles = []string{pki.RoleAdmin, pki.RoleOperator, pki.RoleReader}

// userNamePattern is a name the Controller may act for (as a client
// certificate's common name).
var userNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._@:+-]{0,63}$`)

// authorize finds whom the call is for and checks they may make it.
func authorize(ctx context.Context, fullMethod string) (Caller, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return Caller{}, status.Error(codes.Unauthenticated, "no peer information")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return Caller{}, status.Error(codes.Unauthenticated, "no client certificate presented")
	}
	leaf := tlsInfo.State.PeerCertificates[0]
	if slices.Contains(leaf.Subject.Organization, pki.RoleClusterMember) {
		return authorizeMember(tlsInfo.State.VerifiedChains, fullMethod)
	}
	if slices.Contains(leaf.Subject.Organization, pki.RoleStateTransfer) {
		return authorizeTransfer(leaf, tlsInfo.State.VerifiedChains, fullMethod)
	}
	caller := Caller{Name: leaf.Subject.CommonName, Roles: leaf.Subject.Organization}
	md, _ := metadata.FromIncomingContext(ctx)
	if ids := md.Get(pki.ClusterApplyKey); len(ids) == 1 {
		caller.ClusterApply = ids[0]
	}
	if chains := tlsInfo.State.VerifiedChains; len(chains) > 0 && len(chains[0]) > 0 && FleetRoot != nil {
		if root := FleetRoot(); root != nil && chains[0][len(chains[0])-1].Equal(root) {
			caller.Fleet = true
		}
	}

	if slices.Contains(leaf.Subject.Organization, pki.RoleController) {
		users, roles := md.Get(pki.AsUserKey), md.Get(pki.AsRolesKey)
		if len(users) != 1 || len(roles) != 1 || !userNamePattern.MatchString(users[0]) {
			return Caller{}, status.Errorf(codes.PermissionDenied, "a Controller certificate acts for a user: %s and %s are required", pki.AsUserKey, pki.AsRolesKey)
		}
		as := strings.Split(roles[0], ",")
		for _, r := range as {
			if !slices.Contains(humanRoles, r) {
				return Caller{}, status.Errorf(codes.PermissionDenied, "%s: unknown role %q", pki.AsRolesKey, r)
			}
		}
		caller = Caller{Name: users[0], Roles: as, Via: leaf.Subject.CommonName, ClusterApply: caller.ClusterApply}
		if d := md.Get(pki.AsDomainsKey); len(d) > 0 {
			if len(d) != 1 {
				return Caller{}, status.Errorf(codes.PermissionDenied, "%s: once at most", pki.AsDomainsKey)
			}
			for _, dom := range strings.Split(d[0], ",") {
				if !slices.Contains(rbac.NodeDomains, dom) && dom != rbac.DomainObserve {
					return Caller{}, status.Errorf(codes.PermissionDenied, "%s: unknown domain %q", pki.AsDomainsKey, dom)
				}
				caller.Domains = append(caller.Domains, dom)
			}
		}
	}

	if slices.Contains(leaf.Subject.Organization, pki.RoleScoped) && !slices.Contains(leaf.Subject.Organization, pki.RoleController) {
		return authorizeScoped(leaf, caller, fullMethod)
	}

	required := rolesFor(fullMethod)
	for _, have := range caller.Roles {
		if !slices.Contains(required, have) {
			continue
		}
		if !rbac.InDomains(fullMethod, caller.Domains) {
			return Caller{}, status.Errorf(codes.PermissionDenied, "%s is in the %s domain: %s may only %s", fullMethod, rbac.DomainOf(fullMethod), caller.Name, strings.Join(caller.Domains, ", "))
		}
		return caller, nil
	}
	return Caller{}, status.Errorf(codes.PermissionDenied, "%s requires role %v, %s has %v", fullMethod, required, caller.Name, caller.Roles)
}

// authorizeMember checks a cluster member certificate's call: only the
// RPCs for members, and only from a member of this node's own cluster -
// whatever else the certificate's roles say.
func authorizeMember(chains [][]*x509.Certificate, fullMethod string) (Caller, error) {
	if !slices.Contains(rolesFor(fullMethod), pki.RoleClusterMember) {
		return Caller{}, status.Errorf(codes.PermissionDenied, "%s isn't for a cluster member's certificate", fullMethod)
	}
	if ClusterMember == nil || len(chains) == 0 {
		return Caller{}, status.Error(codes.PermissionDenied, "this node is in no cluster")
	}
	name, err := ClusterMember(chains[0])
	if err != nil {
		return Caller{}, status.Errorf(codes.PermissionDenied, "the member certificate: %v", err)
	}
	return Caller{Name: name, Roles: []string{pki.RoleClusterMember}, Member: true}, nil
}

// authorizeScoped checks a scoped certificate's call: the permissions its
// scope gives this node - every node's, and those of the entries naming
// it by its CA's key -, one with a role the method takes, in its domain.
func authorizeScoped(leaf *x509.Certificate, caller Caller, fullMethod string) (Caller, error) {
	if len(leaf.Subject.Organization) != 1 {
		return Caller{}, status.Error(codes.PermissionDenied, "a scoped certificate carries no role of its own")
	}
	scope, ok, err := pki.ParseScope(leaf)
	if err != nil || !ok {
		return Caller{}, status.Errorf(codes.PermissionDenied, "%s's scoped certificate: %v", caller.Name, cmpOr(err, "no scope"))
	}
	var perms []pki.ScopePerm
	if LocalCA != nil {
		if ca := LocalCA(); ca != nil {
			perms = scope.For(pki.CAKey(ca))
		}
	}
	if len(perms) == 0 {
		return Caller{}, status.Errorf(codes.PermissionDenied, "%s's certificate doesn't open this node: its scope names other nodes (sign in again if this node was just added to it, or replaced its CA)", caller.Name)
	}
	required := rolesFor(fullMethod)
	var has []string
	var narrowed *pki.ScopePerm
	for _, p := range perms {
		for _, d := range p.Domains {
			if !slices.Contains(rbac.NodeDomains, d) && d != rbac.DomainObserve {
				return Caller{}, status.Errorf(codes.PermissionDenied, "%s's scope: unknown domain %q", caller.Name, d)
			}
		}
		if !slices.Contains(has, p.Role) {
			has = append(has, p.Role)
		}
		if !slices.Contains(required, p.Role) {
			continue
		}
		if !rbac.InDomains(fullMethod, p.Domains) {
			narrowed = &p
			continue
		}
		caller.Roles, caller.Domains, caller.Scoped = []string{p.Role}, p.Domains, true
		return caller, nil
	}
	if narrowed != nil {
		return Caller{}, status.Errorf(codes.PermissionDenied, "%s is in the %s domain: %s may only %s on this node", fullMethod, rbac.DomainOf(fullMethod), caller.Name, strings.Join(narrowed.Domains, ", "))
	}
	return Caller{}, status.Errorf(codes.PermissionDenied, "%s requires role %v, %s has %v on this node", fullMethod, required, caller.Name, has)
}

func cmpOr(err error, otherwise string) string {
	if err != nil {
		return err.Error()
	}
	return otherwise
}
