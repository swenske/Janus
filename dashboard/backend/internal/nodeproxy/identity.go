package nodeproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/rbac"
)

// FleetIdentity is the Controller's own client certificate once its
// fleet is set up (dashboard/backend/internal/fleet) - set by dashboardd.
// A node that trusts the fleet is reached with it; one that doesn't yet,
// with the service credential the Controller got when it added it.
var FleetIdentity func() (*tls.Certificate, error)

// User is whom a call to a node is made for: a node lets a fleet
// certificate act only for the user it names (internal/api/authz.go),
// and logs that user.
type User struct {
	Name string
	// Perms are the ways the user may act on this node - the first that
	// lets a call through makes it; none, and the Controller refuses it
	// before it leaves.
	Perms []Perm
}

// Perm is a node role (os:...) narrowed to some domains (internal/rbac;
// none: every domain).
type Perm struct {
	Role    string
	Domains []string
}

// permFor is the first of u's permissions that lets it call method.
func (u User) permFor(method string) (Perm, bool) {
	for _, p := range u.Perms {
		if rbac.Allowed(method, []string{p.Role}) && rbac.InDomains(method, p.Domains) {
			return p, true
		}
	}
	return Perm{}, false
}

// May is the RPCs u may call ("Service/Method"), sorted.
func (u User) May() []string {
	var out []string
	for m := range rbac.Required {
		if _, ok := u.permFor(m); ok {
			out = append(out, strings.TrimPrefix(m, "/janus.v1alpha1."))
		}
	}
	slices.Sort(out)
	return out
}

// Roles are u's node roles, the highest first.
func (u User) Roles() []string {
	var out []string
	for _, p := range u.Perms {
		if !slices.Contains(out, p.Role) {
			out = append(out, p.Role)
		}
	}
	return out
}

// Automation is the Controller's own work - what no page asked for.
var Automation = User{Name: "automation", Perms: []Perm{{Role: pki.RoleAdmin}}}

type userKey struct{}

// WithUser makes the calls to nodes under ctx for u.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

func userOf(ctx context.Context) User {
	if u, ok := ctx.Value(userKey{}).(User); ok {
		return u
	}
	return Automation
}

// userName is a name a node accepts for a user (its userNamePattern).
var userName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._@:+-]{0,63}$`)

// actingFor is ctx for a call of method made for its user, with the
// permission that lets it through - refused here when none does.
func actingFor(ctx context.Context, method string) (context.Context, error) {
	u := userOf(ctx)
	p, ok := u.permFor(method)
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "%s may not call %s on this node", u.Name, strings.TrimPrefix(method, "/janus.v1alpha1."))
	}
	if !userName.MatchString(u.Name) {
		u.Name = "unnamed"
	}
	kv := []string{pki.AsUserKey, u.Name, pki.AsRolesKey, p.Role}
	if len(p.Domains) > 0 {
		kv = append(kv, pki.AsDomainsKey, strings.Join(p.Domains, ","))
	}
	return metadata.AppendToOutgoingContext(ctx, kv...), nil
}

// nodeTLS is how the Controller authenticates to node, and checks it's
// the node: its fleet certificate (fleet - once the node trusts the
// fleet, or to check it does), else the node's service credential; the
// node's server certificate checked against the node's own CA either
// way.
func nodeTLS(node *store.Node, fleet bool) (*tls.Config, error) {
	var cfg *tls.Config
	if !fleet {
		cert, key := node.ServiceCredential()
		c, err := pki.ClientTLSConfig(node.CA(), cert, key)
		if err != nil {
			return nil, err
		}
		cfg = c
	} else {
		if FleetIdentity == nil {
			return nil, errors.New("the node trusts the fleet, but this Controller has no fleet certificate")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(node.CA()) {
			return nil, fmt.Errorf("node %s: no valid certificates in its CA", node.ID)
		}
		cfg = &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS13,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				return FleetIdentity()
			},
		}
	}
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		followCA(node, cs.VerifiedChains)
		return nil
	}
	return cfg, nil
}

// NodeCA records a node's new CA (PEM) - set by dashboardd (the store).
var NodeCA func(node *store.Node, caPEM []byte) error

// followCA records node's new CA when it replaced it: the chain then
// goes through the new CA, cross-signed by the one pinned
// (pki.ServerCert.Rotate) - pinned in its place, so the Controller keeps
// reaching the node, and follows it again at its next rotation.
func followCA(node *store.Node, chains [][]*x509.Certificate) {
	if NodeCA == nil || len(chains) == 0 || len(chains[0]) < 3 {
		return
	}
	next := chains[0][len(chains[0])-2]
	if !next.IsCA {
		return
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: next.Raw})
	if bytes.Equal(caPEM, node.CA()) {
		return
	}
	if err := NodeCA(node, caPEM); err != nil {
		log.Printf("node %s (%s): replaced its CA, but recording the new one failed: %v", node.Name, node.ID, err)
		return
	}
	log.Printf("node %s (%s): replaced its CA - the Controller now pins the new one", node.Name, node.ID)
}

// dialOptions are what every connection to node takes: nodeTLS, as the
// node trusts the fleet or not, and the user each call is made for
// (WithUser; Automation by default).
func dialOptions(node *store.Node) ([]grpc.DialOption, error) {
	return dialOptionsWith(node, node.TrustsFleet())
}

func dialOptionsWith(node *store.Node, fleet bool) ([]grpc.DialOption, error) {
	tlsConfig, err := nodeTLS(node, fleet)
	if err != nil {
		return nil, fmt.Errorf("build TLS config: %w", err)
	}
	return []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			ctx, err := actingFor(ctx, method)
			if err != nil {
				return err
			}
			return invoker(ctx, method, req, reply, cc, opts...)
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			ctx, err := actingFor(ctx, method)
			if err != nil {
				return nil, err
			}
			return streamer(ctx, desc, cc, method, opts...)
		}),
	}, nil
}

// Reconnect drops node's shared connection: the next call dials again -
// after the node starts trusting the fleet, or its CA changed.
func Reconnect(nodeID string) { closeNodeConn(nodeID) }
