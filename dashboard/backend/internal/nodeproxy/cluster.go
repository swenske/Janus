package nodeproxy

import (
	"context"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/clusters"
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

// RouterRoles is a member's routes on its cluster's router, its page's
// "router:" badge: one role per service prefix (internal/clusters
// RouterRoles), when the cluster's RouterOS hook reads the router.
type RouterRoles struct {
	Available bool                  `json:"available"`
	ReadAt    *time.Time            `json:"read_at,omitempty"`
	Roles     []clusters.RouterRole `json:"roles,omitempty"`
	Error     string                `json:"error,omitempty"`
}

// ClusterRouter is node's RouterRoles. Set by the Controller.
var ClusterRouter func(ctx context.Context, node *store.Node) RouterRoles

// clusterMembership is what a node's page shows of its cluster
// membership (ClusterGet): the cluster, this member, the domains it
// manages here - its locks - and who manages it. Nothing secret: no CA,
// no peers section.
type clusterMembership struct {
	Member      bool     `json:"member"`
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name,omitempty"`
	MemberName  string   `json:"member_name,omitempty"`
	Managed     []string `json:"managed"`
	ManagedBy   string   `json:"managed_by,omitempty"`
	Advisory    bool     `json:"advisory,omitempty"`
	Drained     bool     `json:"drained,omitempty"`
	Members     []string `json:"members"`
	ACMEOrderer string   `json:"acme_orderer,omitempty"`
}

func membershipOf(in *janusv1alpha1.ClusterInfo) clusterMembership {
	m := clusterMembership{
		Member: in.GetMember(), ID: in.GetId(), Name: in.GetName(), MemberName: in.GetMemberName(),
		Managed: nonNilStrings(in.GetManaged()), ManagedBy: in.GetManagedBy(), Advisory: in.GetAdvisory(),
		Drained: in.GetDrained(), Members: []string{}, ACMEOrderer: in.GetAcmeOrderer(),
	}
	for _, p := range in.GetMembers() {
		m.Members = append(m.Members, p.GetName())
	}
	return m
}

// registerClusterRoutes: the node's cluster membership, and the release
// of its locks - an emergency change on a member: the cluster then
// reports it drifted and takes the domains back with its next apply.
func registerClusterRoutes(mux *http.ServeMux, node *store.Node) {
	cl := func(fn func(context.Context, janusv1alpha1.ClusterServiceClient) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
				return fn(ctx, janusv1alpha1.NewClusterServiceClient(c))
			})
		}
	}
	// The router's view of this member, the Controller's to give.
	mux.HandleFunc("GET /api/cluster/router", func(w http.ResponseWriter, r *http.Request) {
		out := RouterRoles{}
		if ClusterRouter != nil {
			out = ClusterRouter(r.Context(), node)
		}
		writeJSONBody(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/cluster", cl(func(ctx context.Context, c janusv1alpha1.ClusterServiceClient) (any, error) {
		in, err := c.ClusterGet(ctx, &emptypb.Empty{})
		if err != nil {
			return nil, err
		}
		return membershipOf(in), nil
	}))
	mux.HandleFunc("POST /api/cluster/release", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Domains []string `json:"domains"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		cl(func(ctx context.Context, c janusv1alpha1.ClusterServiceClient) (any, error) {
			in, err := c.ClusterRelease(ctx, &janusv1alpha1.ClusterReleaseRequest{Domains: req.Domains})
			if err != nil {
				return nil, err
			}
			return membershipOf(in), nil
		})(w, r)
	})
}
