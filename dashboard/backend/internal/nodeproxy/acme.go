package nodeproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme"
)

// The letsencrypt extension (ACME): its status, its configuration (as
// the JSON janusctl haproxy acme get prints - secrets empty, kept when
// applied empty) and renewals asked for.

func registerACMERoutes(mux *http.ServeMux, node *store.Node) {
	empty := &emptypb.Empty{}
	hapWithin := func(timeout time.Duration, fn func(context.Context, janusv1alpha1.HAProxyServiceClient) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			unary(w, r, node, timeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
				return fn(ctx, janusv1alpha1.NewHAProxyServiceClient(c))
			})
		}
	}
	hap := func(fn func(context.Context, janusv1alpha1.HAProxyServiceClient) (any, error)) http.HandlerFunc {
		return hapWithin(unaryTimeout, fn)
	}
	mux.HandleFunc("GET /api/haproxy/acme", hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
		resp, err := c.ACMEStatus(ctx, empty)
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
		// direct, proxy - or unspecified: a node older than the paths.
		out["path_mode"] = strings.ToLower(strings.TrimPrefix(resp.GetPathMode().String(), "ACME_PATH_MODE_"))
		return out, nil
	}))
	mux.HandleFunc("GET /api/haproxy/acme/config", hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
		resp, err := c.ACMEGetConfig(ctx, empty)
		if err != nil {
			return nil, err
		}
		cfg, err := acme.Marshal(resp.GetConfig())
		if err != nil {
			return nil, err
		}
		return map[string]any{"config": json.RawMessage(cfg), "is_default": resp.GetIsDefault()}, nil
	}))
	for _, action := range []string{"check", "apply"} {
		validate := action == "check"
		mux.HandleFunc("POST /api/haproxy/acme/"+action, func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Config     json.RawMessage `json:"config"`
				AccountKey string          `json:"account_key"`
			}
			if !decodeJSON(w, r, &req) {
				return
			}
			cfg := &janusv1alpha1.ACMEConfig{}
			if len(req.Config) > 0 {
				if err := protojson.Unmarshal(req.Config, cfg); err != nil {
					writeJSONBody(w, http.StatusOK, moduleConfigResult{Errors: []string{"the configuration: " + err.Error()}})
					return
				}
			}
			hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
				resp, err := c.ACMEApplyConfig(ctx, &janusv1alpha1.ACMEApplyConfigRequest{Config: cfg, AccountKey: req.AccountKey, ValidateOnly: validate})
				if err != nil {
					return nil, err
				}
				return moduleConfigResult{Accepted: resp.GetAccepted(), Errors: resp.GetErrors()}, nil
			})(w, r)
		})
	}
	mux.HandleFunc("POST /api/haproxy/acme/check-path", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path        json.RawMessage `json:"path"`
			Directory   string          `json:"directory"`
			DirectoryCA string          `json:"directory_ca"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		var path *janusv1alpha1.ACMEPath
		if len(req.Path) > 0 && string(req.Path) != "null" {
			path = &janusv1alpha1.ACMEPath{}
			if err := protojson.Unmarshal(req.Path, path); err != nil {
				writeJSONBody(w, http.StatusOK, map[string]any{"ok": false, "error": "the path: " + err.Error()})
				return
			}
		}
		// The node gives fetching the directory 45 s.
		hapWithin(time.Minute, func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
			return c.ACMECheckPath(ctx, &janusv1alpha1.ACMECheckPathRequest{Path: path, Directory: req.Directory, DirectoryCa: req.DirectoryCA})
		})(w, r)
	})
	mux.HandleFunc("POST /api/haproxy/acme/renew", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Names []string `json:"names"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
			resp, err := c.ACMERenew(ctx, &janusv1alpha1.ACMERenewRequest{Names: req.Names})
			if err != nil {
				return nil, err
			}
			return map[string]any{"names": resp.GetNames()}, nil
		})(w, r)
	})
}
