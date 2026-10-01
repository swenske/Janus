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

// The keepalived extension (VRRP): its state and keepalived.conf.
// Configurations are plain strings in JSON, not protojson's base64.

type moduleConfigResult struct {
	Accepted bool     `json:"accepted"`
	Errors   []string `json:"errors,omitempty"`
}

func registerVRRPRoutes(mux *http.ServeMux, node *store.Node) {
	empty := &emptypb.Empty{}
	net := func(fn func(context.Context, janusv1alpha1.NetworkServiceClient) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
				return fn(ctx, janusv1alpha1.NewNetworkServiceClient(c))
			})
		}
	}
	mux.HandleFunc("GET /api/network/vrrp", net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
		resp, err := c.VRRPStatus(ctx, empty)
		if err != nil {
			return nil, err
		}
		type instance struct {
			Name              string   `json:"name"`
			State             string   `json:"state"`
			Interface         string   `json:"interface"`
			VRID              uint32   `json:"vrid"`
			Priority          uint32   `json:"priority"`
			EffectivePriority uint32   `json:"effective_priority"`
			VirtualIPs        []string `json:"virtual_ips"`
			LastTransition    int64    `json:"last_transition_unix"`
			BecameMaster      uint64   `json:"became_master"`
		}
		out := map[string]any{
			"state":               strings.ToLower(strings.TrimPrefix(resp.GetState().String(), "MODULE_STATE_")),
			"configured":          resp.GetConfigured(),
			"haproxy_health_file": resp.GetHaproxyHealthFile(),
			"haproxy_healthy":     resp.GetHaproxyHealthy(),
			"error":               resp.GetError(),
		}
		instances := []instance{}
		for _, in := range resp.GetInstances() {
			instances = append(instances, instance{in.GetName(), in.GetRole(), in.GetInterface(), in.GetVirtualRouterId(), in.GetPriority(),
				in.GetEffectivePriority(), in.GetVirtualIps(), in.GetLastTransitionUnix(), in.GetBecameMaster()})
		}
		out["instances"] = instances
		return out, nil
	}))
	mux.HandleFunc("GET /api/network/vrrp/config", net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
		resp, err := c.VRRPGetConfig(ctx, empty)
		if err != nil {
			return nil, err
		}
		return map[string]any{"config": string(resp.GetConfig()), "is_default": resp.GetIsDefault()}, nil
	}))
	for _, action := range []string{"check", "apply"} {
		validate := action == "check"
		mux.HandleFunc("POST /api/network/vrrp/"+action, func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Config string `json:"config"`
			}
			if !decodeJSON(w, r, &req) {
				return
			}
			net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
				resp, err := c.VRRPApplyConfig(ctx, &janusv1alpha1.VRRPApplyConfigRequest{Config: []byte(req.Config), ValidateOnly: validate})
				if err != nil {
					return nil, err
				}
				return moduleConfigResult{Accepted: resp.GetAccepted(), Errors: resp.GetErrors()}, nil
			})(w, r)
		})
	}
}
