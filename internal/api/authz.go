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
// call's metadata - and the call gets that user's roles. Every call that
// isn't read-only is logged with who made it.
package api

import (
	"context"
	"crypto/x509"
	"log"
	"regexp"
	"slices"
	"strings"

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
}

func (c Caller) String() string {
	s := c.Name + " (" + strings.Join(c.Roles, ", ")
	if c.Fleet && c.Via == "" {
		s += ", fleet"
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

type callerKey struct{}

// CallerFrom is whom the RPC running with ctx is for.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// UnaryAuthInterceptor enforces requiredRoles for unary RPCs.
func UnaryAuthInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	caller, err := authorize(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	audit(caller, info.FullMethod)
	return handler(context.WithValue(ctx, callerKey{}, caller), req)
}

// StreamAuthInterceptor enforces requiredRoles for streaming RPCs.
func StreamAuthInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	caller, err := authorize(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	audit(caller, info.FullMethod)
	return handler(srv, callerStream{ss, context.WithValue(ss.Context(), callerKey{}, caller)})
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
	caller := Caller{Name: leaf.Subject.CommonName, Roles: leaf.Subject.Organization}
	if chains := tlsInfo.State.VerifiedChains; len(chains) > 0 && len(chains[0]) > 0 && FleetRoot != nil {
		if root := FleetRoot(); root != nil && chains[0][len(chains[0])-1].Equal(root) {
			caller.Fleet = true
		}
	}

	if slices.Contains(leaf.Subject.Organization, pki.RoleController) {
		md, _ := metadata.FromIncomingContext(ctx)
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
		caller = Caller{Name: users[0], Roles: as, Via: leaf.Subject.CommonName}
	}

	required := rolesFor(fullMethod)
	for _, have := range caller.Roles {
		if slices.Contains(required, have) {
			return caller, nil
		}
	}
	return Caller{}, status.Errorf(codes.PermissionDenied, "%s requires role %v, %s has %v", fullMethod, required, caller.Name, caller.Roles)
}
