package nodeproxy

import (
	"context"

	"google.golang.org/grpc"
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
	return janusv1alpha1.NewAccessServiceClient(conn).TrustGet(ctx, &emptypb.Empty{})
}

// TrustSet has node trust the fleet: its root (PEM) and the bundle it
// signed.
func TrustSet(ctx context.Context, node *store.Node, rootPEM, bundle []byte) (*janusv1alpha1.TrustState, error) {
	conn, err := dialNode(node)
	if err != nil {
		return nil, err
	}
	return janusv1alpha1.NewAccessServiceClient(conn).TrustSet(ctx, &janusv1alpha1.TrustSetRequest{RootCert: rootPEM, Bundle: bundle})
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
