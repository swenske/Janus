package api

import (
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/bgp"
	"github.com/swenske/Janus/internal/firewall"
	"github.com/swenske/Janus/internal/netmgr"
	"github.com/swenske/Janus/internal/timesync"
	"github.com/swenske/Janus/internal/vrrp"
)

// Network implements janusv1alpha1.NetworkServiceServer for the optional
// modules - extensions chosen when the image is built (docs/image-
// factory.md). A module is enabled by its daemon being in the node's
// image at all, not by a runtime switch: without it, status RPCs report
// MODULE_STATE_NOT_ENABLED and apply RPCs are refused.
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
	// BGP manages the bird extension (bgp.go).
	BGP *bgp.Manager
}
