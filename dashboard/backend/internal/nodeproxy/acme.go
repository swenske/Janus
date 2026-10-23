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
	"github.com/swenske/Janus/internal/pki"
)

// The letsencrypt extension (ACME): its status, its configuration (as
// the JSON janusctl haproxy acme get prints - secrets empty, kept when
// applied empty) and renewals asked for.

// RelayInfo is what a node page needs to send its node through the
// Controller's relay for Let's Encrypt (the Controller's acme_relay.go).
type RelayInfo struct {
	Enabled   bool   `json:"enabled"`
	Listening bool   `json:"listening"`
	URL       string `json:"url,omitempty"`
	CACertPEM string `json:"ca_cert_pem,omitempty"`
	Error     string `json:"error,omitempty"`
	// BuiltIn and Allowed are where it lets a node go: the built-in
	// hosts (port 443) by what they're for, and an admin's host:port.
	BuiltIn         map[string][]string `json:"built_in,omitempty"`
	Allowed         []string            `json:"allowed,omitempty"`
	NotThroughRelay map[string]string   `json:"not_through_relay,omitempty"`
}

// OrderingInfo is what a node page needs to choose "Ordered by this
// Controller" (the Controller's acme_order.go).
type OrderingInfo struct {
	Available bool `json:"available"` // this Controller orders for nodes
	// Ready: an admin set it up - the account, the CA's terms; Why, if
	// not.
	Ready        bool            `json:"ready"`
	Why          string          `json:"why,omitempty"`
	Thumbprint   string          `json:"thumbprint,omitempty"` // the account's: the node's HTTP-01 rule answers with it
	DNSProviders []NamedProvider `json:"dns_providers,omitempty"`
}

// NamedProvider is one of the Controller's DNS providers, without its
// settings.
type NamedProvider struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ACMERelay is the relay now, ACMEOrdering the Controller's ordering;
// ACMEChanged tells the Controller a node's Let's Encrypt configuration
// changed or a renewal was asked for. Set by the Controller.
var (
	ACMERelay    func() RelayInfo
	ACMEOrdering func() OrderingInfo
	ACMEChanged  func()
)

// ACMEOrderingUser is the Controller ordering certificates for a node -
// the name its audit shows.
var ACMEOrderingUser = User{Name: "lets-encrypt", Perms: []Perm{{Role: pki.RoleOperator}}}

func acmeChanged() {
	if ACMEChanged != nil {
		ACMEChanged()
	}
}

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
	// The Controller's part, for the page's "Through this Controller"
	// and "Ordered by this Controller".
	mux.HandleFunc("GET /api/haproxy/acme/controller", func(w http.ResponseWriter, r *http.Request) {
		out := struct {
			Relay    RelayInfo    `json:"relay"`
			Ordering OrderingInfo `json:"ordering"`
		}{}
		if ACMERelay != nil {
			out.Relay = ACMERelay()
		}
		if ACMEOrdering != nil {
			out.Ordering = ACMEOrdering()
		}
		writeJSONBody(w, http.StatusOK, out)
	})
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
				if resp.GetAccepted() && !validate {
					acmeChanged()
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
			acmeChanged() // the Controller orders a certificate it orders now
			return map[string]any{"names": resp.GetNames()}, nil
		})(w, r)
	})
}
