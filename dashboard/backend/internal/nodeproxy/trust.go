package nodeproxy

import (
	"context"
	"crypto/tls"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// TrustGet is the fleet node trusts (AccessService).
func TrustGet(ctx context.Context, node *store.Node) (*janusv1alpha1.TrustState, error) {
	conn, err := dialNode(node)
	if err != nil {
		return nil, err
	}
	return onceMore(func(opts ...grpc.CallOption) (*janusv1alpha1.TrustState, error) {
		return janusv1alpha1.NewAccessServiceClient(conn).TrustGet(ctx, &emptypb.Empty{}, opts...)
	})
}

// TrustSet has node trust the fleet: its root (PEM) and the bundle it
// signed.
func TrustSet(ctx context.Context, node *store.Node, rootPEM, bundle []byte) (*janusv1alpha1.TrustState, error) {
	conn, err := dialNode(node)
	if err != nil {
		return nil, err
	}
	return onceMore(func(opts ...grpc.CallOption) (*janusv1alpha1.TrustState, error) {
		return janusv1alpha1.NewAccessServiceClient(conn).TrustSet(ctx, &janusv1alpha1.TrustSetRequest{RootCert: rootPEM, Bundle: bundle}, opts...)
	})
}

// onceMore runs call, and once more if the connection turned out dead:
// the shared connection to a node that rebooted since it was last used
// looks fine until the first bytes sent on it come back as a reset (no
// keepalive pings it meanwhile - a node from before the enforcement
// policy answers them with a GOAWAY), so that first call fails with
// Unavailable and grpc reconnects underneath. The second call waits for
// the new connection - the node is back, or the caller's deadline says.
// For the calls nothing retries by itself (a trust loop's next turn is
// ten minutes away); a live view just polls again.
// A node refusing the certificate isn't a dead connection: that answer
// stands (waiting would only end on the caller's deadline, the reason
// lost).
func onceMore[T any](call func(opts ...grpc.CallOption) (T, error)) (T, error) {
	out, err := call()
	if status.Code(err) != codes.Unavailable || RefusedCertificate(err) {
		return out, err
	}
	return call(grpc.WaitForReady(true))
}

// RefusedCertificate reports whether err is a node refusing the
// Controller's client certificate - its TLS alert, as gRPC reports it.
func RefusedCertificate(err error) bool {
	msg := status.Convert(err).Message()
	for _, alert := range []string{"bad certificate", "unknown certificate authority", "certificate required", "expired certificate", "certificate expired", "certificate unknown"} {
		if strings.Contains(msg, alert) {
			return true
		}
	}
	return false
}

// ProbeFleet checks that the Controller's fleet certificate lets it in to
// node - over a connection of its own, before the Controller relies on
// it.
func ProbeFleet(ctx context.Context, node *store.Node) error {
	opts, err := dialOptionsWith(node, true, nil)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(node.Addr(), opts...)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = janusv1alpha1.NewSystemServiceClient(conn).Version(ctx, &emptypb.Empty{}, grpc.WaitForReady(true))
	return err
}

// TrustSyncWith brings node to bundle (version, signed by the root
// rootPEM) over a connection of its own whose client certificate is
// cert - not the shared connection's: a certificate the node's bundle
// still lets in, when the Controller's usual one for it doesn't (a
// renewal of the issuing CA it missed, or one that expired). It answers
// the version the node has afterwards.
func TrustSyncWith(ctx context.Context, node *store.Node, cert *tls.Certificate, rootPEM, bundle []byte, version uint64) (uint64, error) {
	opts, err := dialOptionsWith(node, true, cert)
	if err != nil {
		return 0, err
	}
	conn, err := grpc.NewClient(node.Addr(), opts...)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	c := janusv1alpha1.NewAccessServiceClient(conn)
	st, err := onceMore(func(opts ...grpc.CallOption) (*janusv1alpha1.TrustState, error) {
		return c.TrustGet(ctx, &emptypb.Empty{}, opts...)
	})
	if err != nil {
		return 0, err
	}
	if st.GetBundleVersion() >= version {
		return st.GetBundleVersion(), nil
	}
	if st, err = c.TrustSet(ctx, &janusv1alpha1.TrustSetRequest{RootCert: rootPEM, Bundle: bundle}); err != nil {
		return 0, err
	}
	return st.GetBundleVersion(), nil
}
