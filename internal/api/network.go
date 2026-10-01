package api

import (
	"context"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/firewall"
	"github.com/swenske/Janus/internal/netmgr"
	"github.com/swenske/Janus/internal/timesync"
	"github.com/swenske/Janus/internal/vrrp"
)

// Network implements janusv1alpha1.NetworkServiceServer for the optional
// modules (roadmap Phase 5). A module is enabled by its daemon being in
// the node's image at all, not by a runtime switch - so when the binary
// isn't there (every image today), status RPCs report
// MODULE_STATE_NOT_ENABLED and apply RPCs are refused. Management of a
// module that is present isn't built yet and says so.
//
// It also serves the node's own network configuration (netconfig.go),
// through Net and Time.
type Network struct {
	janusv1alpha1.UnimplementedNetworkServiceServer
	Net  *netmgr.Manager
	Time *timesync.Service
	// Firewall manages the nftables extension's ruleset (firewall.go).
	Firewall *firewall.Manager
	// VRRP manages the keepalived extension (vrrp.go); HAProxyHealthy is
	// what its health file reports.
	VRRP           *vrrp.Manager
	HAProxyHealthy func() bool
}

// moduleBinaries is where each module's daemon lives in an image that
// includes it - a var so tests can point it elsewhere.
var moduleBinaries = map[string]string{
	"bgp":      "/usr/local/sbin/bird",
	"vrrp":     "/usr/local/sbin/keepalived",
	"firewall": "/usr/local/sbin/nft",
}

var moduleDaemons = map[string]string{"bgp": "bird", "vrrp": "keepalived", "firewall": "nftables"}

func moduleEnabled(module string) bool {
	_, err := os.Stat(moduleBinaries[module])
	return err == nil
}

// moduleStatus is what every status RPC returns: NOT_ENABLED, or an
// honest Unimplemented if the module is actually present.
func moduleStatus(module string) (janusv1alpha1.ModuleState, error) {
	if !moduleEnabled(module) {
		return janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED, nil
	}
	return 0, status.Errorf(codes.Unimplemented, "%s is in this image, but managing it isn't implemented yet", moduleDaemons[module])
}

func moduleApply(module string) error {
	if !moduleEnabled(module) {
		return status.Errorf(codes.FailedPrecondition, "%s isn't in this node's image - optional modules are chosen when the image is built", moduleDaemons[module])
	}
	return status.Errorf(codes.Unimplemented, "%s is in this image, but managing it isn't implemented yet", moduleDaemons[module])
}

func (n *Network) BGPStatus(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.BGPStatusResponse, error) {
	st, err := moduleStatus("bgp")
	if err != nil {
		return nil, err
	}
	return &janusv1alpha1.BGPStatusResponse{State: st}, nil
}

func (n *Network) BGPApplyConfig(_ context.Context, _ *janusv1alpha1.BGPApplyConfigRequest) (*janusv1alpha1.BGPApplyConfigResponse, error) {
	return nil, moduleApply("bgp")
}
