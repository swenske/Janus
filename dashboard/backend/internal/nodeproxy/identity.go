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
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
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
	Name  string
	Roles []string
}

// Automation is the Controller's own work - what no page asked for.
var Automation = User{Name: "automation", Roles: []string{pki.RoleAdmin}}

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

func actingFor(ctx context.Context) context.Context {
	u := userOf(ctx)
	if !userName.MatchString(u.Name) {
		u.Name = "unnamed"
	}
	return metadata.AppendToOutgoingContext(ctx, pki.AsUserKey, u.Name, pki.AsRolesKey, strings.Join(u.Roles, ","))
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
			return invoker(actingFor(ctx), method, req, reply, cc, opts...)
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(actingFor(ctx), desc, cc, method, opts...)
		}),
	}, nil
}

// Reconnect drops node's shared connection: the next call dials again -
// after the node starts trusting the fleet, or its CA changed.
func Reconnect(nodeID string) { closeNodeConn(nodeID) }
