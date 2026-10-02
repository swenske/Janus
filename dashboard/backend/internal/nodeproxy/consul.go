package nodeproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// The consul extension: the agent's status, its configuration (a plain
// string) and the files it names - by name only, their content never
// comes back; a file sent empty keeps the saved one.

func registerConsulRoutes(mux *http.ServeMux, node *store.Node) {
	empty := &emptypb.Empty{}
	net := func(fn func(context.Context, janusv1alpha1.NetworkServiceClient) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
				return fn(ctx, janusv1alpha1.NewNetworkServiceClient(c))
			})
		}
	}
	mux.HandleFunc("GET /api/network/consul", net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
		resp, err := c.ConsulStatus(ctx, empty)
		if err != nil {
			return nil, err
		}
		data, err := protoOut.Marshal(resp)
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, err
		}
		out["state"] = strings.ToLower(strings.TrimPrefix(resp.GetState().String(), "MODULE_STATE_"))
		return out, nil
	}))
	mux.HandleFunc("GET /api/network/consul/config", net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
		resp, err := c.ConsulGetConfig(ctx, empty)
		if err != nil {
			return nil, err
		}
		files := resp.GetFiles()
		if files == nil {
			files = []string{}
		}
		return map[string]any{"config": string(resp.GetConfig()), "is_default": resp.GetIsDefault(), "files": files}, nil
	}))
	for _, action := range []string{"check", "apply"} {
		validate := action == "check"
		mux.HandleFunc("POST /api/network/consul/"+action, func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Config string            `json:"config"`
				Files  map[string]string `json:"files"`
			}
			if !decodeJSON(w, r, &req) {
				return
			}
			files := map[string][]byte{}
			for n, content := range req.Files {
				files[n] = []byte(content)
			}
			net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
				resp, err := c.ConsulApplyConfig(ctx, &janusv1alpha1.ConsulApplyConfigRequest{Config: []byte(req.Config), Files: files, ValidateOnly: validate})
				if err != nil {
					return nil, err
				}
				return moduleConfigResult{Accepted: resp.GetAccepted(), Errors: resp.GetErrors()}, nil
			})(w, r)
		})
	}
}
