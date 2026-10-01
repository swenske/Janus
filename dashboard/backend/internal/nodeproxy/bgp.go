package nodeproxy

import (
	"context"
	"net/http"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// The bird extension (BGP): its protocols and bird.conf, the same shape
// as the VRRP routes.
func registerBGPRoutes(mux *http.ServeMux, node *store.Node) {
	empty := &emptypb.Empty{}
	net := func(fn func(context.Context, janusv1alpha1.NetworkServiceClient) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
				return fn(ctx, janusv1alpha1.NewNetworkServiceClient(c))
			})
		}
	}
	mux.HandleFunc("GET /api/network/bgp", net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
		resp, err := c.BGPStatus(ctx, empty)
		if err != nil {
			return nil, err
		}
		type channel struct {
			Name      string `json:"name"`
			State     string `json:"state"`
			Imported  uint32 `json:"imported"`
			Exported  uint32 `json:"exported"`
			Preferred uint32 `json:"preferred"`
		}
		type protocol struct {
			Name            string    `json:"name"`
			Protocol        string    `json:"protocol"`
			Table           string    `json:"table"`
			State           string    `json:"state"`
			Since           string    `json:"since"`
			Info            string    `json:"info"`
			BGPState        string    `json:"bgp_state,omitempty"`
			NeighborAddress string    `json:"neighbor_address,omitempty"`
			NeighborAS      uint32    `json:"neighbor_as,omitempty"`
			LocalAS         uint32    `json:"local_as,omitempty"`
			LastError       string    `json:"last_error,omitempty"`
			Channels        []channel `json:"channels"`
			HeldDown        bool      `json:"held_down"`
		}
		protocols := []protocol{}
		for _, p := range resp.GetProtocols() {
			pp := protocol{p.GetName(), p.GetProtocol(), p.GetTable(), p.GetState(), p.GetSince(), p.GetInfo(), p.GetBgpState(),
				p.GetNeighborAddress(), p.GetNeighborAs(), p.GetLocalAs(), p.GetLastError(), []channel{}, p.GetHeldDown()}
			for _, ch := range p.GetChannels() {
				pp.Channels = append(pp.Channels, channel{ch.GetName(), ch.GetState(), ch.GetImported(), ch.GetExported(), ch.GetPreferred()})
			}
			protocols = append(protocols, pp)
		}
		return map[string]any{
			"state":           strings.ToLower(strings.TrimPrefix(resp.GetState().String(), "MODULE_STATE_")),
			"configured":      resp.GetConfigured(),
			"version":         resp.GetVersion(),
			"router_id":       resp.GetRouterId(),
			"haproxy_healthy": resp.GetHaproxyHealthy(),
			"error":           resp.GetError(),
			"protocols":       protocols,
		}, nil
	}))
	mux.HandleFunc("GET /api/network/bgp/config", net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
		resp, err := c.BGPGetConfig(ctx, empty)
		if err != nil {
			return nil, err
		}
		return map[string]any{"config": string(resp.GetConfig()), "is_default": resp.GetIsDefault()}, nil
	}))
	for _, action := range []string{"check", "apply"} {
		validate := action == "check"
		mux.HandleFunc("POST /api/network/bgp/"+action, func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Config string `json:"config"`
			}
			if !decodeJSON(w, r, &req) {
				return
			}
			net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
				resp, err := c.BGPApplyConfig(ctx, &janusv1alpha1.BGPApplyConfigRequest{Config: []byte(req.Config), ValidateOnly: validate})
				if err != nil {
					return nil, err
				}
				return moduleConfigResult{Accepted: resp.GetAccepted(), Errors: resp.GetErrors()}, nil
			})(w, r)
		})
	}
}
