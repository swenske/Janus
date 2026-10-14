package nodeproxy

import (
	"context"
	"errors"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// AutoScaling is the Controller's auto-scaling acting on a node - the
// name a node's sysctl history shows for what it changed.
var AutoScaling = User{Name: "auto-scaling", Perms: []Perm{{Role: pki.RoleAdmin}}}

// The node's kernel parameters, for auto-scaling (dashboard/backend:
// autoscale_sysctl.go): what it suggests, who changed what, a trial and
// its confirmation. The caller's context says who acts (WithUser).

// SysctlList is node's parameters, their suggestions and its trial.
func SysctlList(ctx context.Context, node *store.Node) (*janusv1alpha1.SysctlListResponse, error) {
	conn, err := dialNode(node)
	if err != nil {
		return nil, err
	}
	resp, err := janusv1alpha1.NewSystemServiceClient(conn).SysctlList(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, errors.New(status.Convert(err).Message())
	}
	return resp, nil
}

// SysctlHistory is node's newest limit sysctl changes, the newest first.
func SysctlHistory(ctx context.Context, node *store.Node, limit uint32) ([]*janusv1alpha1.SysctlHistoryEntry, error) {
	conn, err := dialNode(node)
	if err != nil {
		return nil, err
	}
	resp, err := janusv1alpha1.NewSystemServiceClient(conn).SysctlHistory(ctx, &janusv1alpha1.SysctlHistoryRequest{Limit: limit})
	if err != nil {
		return nil, errors.New(status.Convert(err).Message())
	}
	return resp.GetEntries(), nil
}

// SysctlApply puts changes on trial on node.
func SysctlApply(ctx context.Context, node *store.Node, req *janusv1alpha1.SysctlApplyRequest) (*janusv1alpha1.SysctlApplyResponse, error) {
	conn, err := dialNode(node)
	if err != nil {
		return nil, err
	}
	resp, err := janusv1alpha1.NewSystemServiceClient(conn).SysctlApply(ctx, req)
	if err != nil {
		return nil, errors.New(status.Convert(err).Message())
	}
	return resp, nil
}

// SysctlConfirm confirms node's trial over a connection opened after it
// - what the node requires, proof it's still reachable.
func SysctlConfirm(ctx context.Context, node *store.Node) error {
	_, err := sysctlConfirmFresh(ctx, node)
	return err
}
