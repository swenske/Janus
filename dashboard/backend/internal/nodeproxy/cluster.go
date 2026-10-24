package nodeproxy

import (
	"context"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
)

// ClusterUser is the Controller's cluster engine acting on a member
// (dashboard/backend: cluster*.go) - the name a node's audit, its
// configuration history and its events show for what a cluster did.
var ClusterUser = User{Name: "cluster", Perms: []Perm{{Role: pki.RoleAdmin}}}

// ForCluster makes the calls under ctx the cluster engine's, for cluster
// id: what lets a write through a member's cluster lock (the node honours
// it from the Controller's certificate only).
func ForCluster(ctx context.Context, id string) context.Context {
	return metadata.AppendToOutgoingContext(WithUser(ctx, ClusterUser), pki.ClusterApplyKey, id)
}

// ConfirmFirewall confirms node's firewall ruleset on trial, over fresh
// connections, until shortly before it reverts at revertAt: a ruleset that
// shuts the Controller out can't be, and reverts by itself.
func ConfirmFirewall(node *store.Node, revertAt time.Time) (bool, string) {
	return confirmFirewall(node, revertAt)
}
