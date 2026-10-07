package nodeproxy

import (
	"context"

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
func onceMore[T any](call func(opts ...grpc.CallOption) (T, error)) (T, error) {
	out, err := call()
	if status.Code(err) != codes.Unavailable {
		return out, err
	}
	return call(grpc.WaitForReady(true))
}

// ProbeFleet checks that the Controller's fleet certificate lets it in to
// node - over a connection of its own, before the Controller relies on
// it.
func ProbeFleet(ctx context.Context, node *store.Node) error {
	opts, err := dialOptionsWith(node, true)
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
