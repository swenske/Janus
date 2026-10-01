package nodeproxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
)

// The firewall (the nftables extension): the node's ruleset applied on
// trial and confirmed over a connection opened after the apply - not the
// shared one (dialNode), which is established and survives any ruleset,
// so proves nothing.

type firewallApplyResult struct {
	Accepted     bool     `json:"accepted"`
	Errors       []string `json:"errors,omitempty"`
	RevertAtUnix int64    `json:"revert_at_unix,omitempty"`
	Confirmed    bool     `json:"confirmed"`
	Error        string   `json:"error,omitempty"`
}

func registerFirewallRoutes(mux *http.ServeMux, node *store.Node) {
	empty := &emptypb.Empty{}
	net := func(fn func(context.Context, janusv1alpha1.NetworkServiceClient) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
				return fn(ctx, janusv1alpha1.NewNetworkServiceClient(c))
			})
		}
	}

	mux.HandleFunc("GET /api/network/firewall", net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
		resp, err := c.FirewallList(ctx, empty)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"state":                strings.ToLower(strings.TrimPrefix(resp.GetState().String(), "MODULE_STATE_")),
			"ruleset":              string(resp.GetRuleset()),
			"configured":           resp.GetConfigured(),
			"trial_pending":        resp.GetTrialPending(),
			"trial_revert_at_unix": resp.GetTrialRevertAtUnix(),
		}, nil
	}))
	mux.HandleFunc("GET /api/network/firewall/ruleset", net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
		resp, err := c.FirewallGetRuleset(ctx, empty)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ruleset": string(resp.GetRuleset()), "is_default": resp.GetIsDefault()}, nil
	}))
	mux.HandleFunc("POST /api/network/firewall/check", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Ruleset string `json:"ruleset"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
			resp, err := c.FirewallApplyRuleset(ctx, &janusv1alpha1.FirewallApplyRulesetRequest{Ruleset: []byte(req.Ruleset), ValidateOnly: true})
			if err != nil {
				return nil, err
			}
			return firewallApplyResult{Accepted: resp.GetAccepted(), Errors: resp.GetErrors()}, nil
		})(w, r)
	})
	mux.HandleFunc("POST /api/network/firewall/apply", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Ruleset               string `json:"ruleset"`
			ConfirmTimeoutSeconds uint32 `json:"confirm_timeout_seconds"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		conn, err := dialNode(node)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), unaryTimeout)
		resp, err := janusv1alpha1.NewNetworkServiceClient(conn).FirewallApplyRuleset(ctx, &janusv1alpha1.FirewallApplyRulesetRequest{
			Ruleset: []byte(req.Ruleset), ConfirmTimeoutSeconds: req.ConfirmTimeoutSeconds,
		})
		cancel()
		if err != nil {
			http.Error(w, status.Convert(err).Message(), grpcHTTPStatus(err))
			return
		}
		res := firewallApplyResult{Accepted: resp.GetAccepted(), Errors: resp.GetErrors(), RevertAtUnix: resp.GetRevertAtUnix()}
		if res.Accepted {
			res.Confirmed, res.Error = confirmFirewall(node, time.Unix(res.RevertAtUnix, 0))
		}
		writeJSONBody(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/network/firewall/confirm", func(w http.ResponseWriter, r *http.Request) {
		if err := firewallConfirmFresh(node); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSONBody(w, http.StatusOK, map[string]bool{"confirmed": true})
	})
	mux.HandleFunc("GET /api/network/firewall/sets", net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
		return c.FirewallSets(ctx, empty)
	}))
	mux.HandleFunc("POST /api/network/firewall/sets", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Family string `json:"family"`
			Table  string `json:"table"`
			Set    string `json:"set"`
			Add    []struct {
				Value          string `json:"value"`
				TimeoutSeconds uint32 `json:"timeout_seconds"`
			} `json:"add"`
			Delete []string `json:"delete"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		out := &janusv1alpha1.FirewallSetUpdateRequest{Family: req.Family, Table: req.Table, Set: req.Set, Delete: req.Delete}
		for _, a := range req.Add {
			out.Add = append(out.Add, &janusv1alpha1.FirewallSetElement{Value: a.Value, TimeoutSeconds: a.TimeoutSeconds})
		}
		net(func(ctx context.Context, c janusv1alpha1.NetworkServiceClient) (any, error) {
			return c.FirewallSetUpdate(ctx, out)
		})(w, r)
	})
}

// confirmFirewall confirms the ruleset on trial over fresh connections,
// retrying until shortly before it reverts. A ruleset that shuts the
// Controller out can't be confirmed, and reverts by itself.
func confirmFirewall(node *store.Node, revertAt time.Time) (bool, string) {
	var last error
	for time.Until(revertAt) > 2*time.Second {
		if last = firewallConfirmFresh(node); last == nil {
			return true, ""
		}
		time.Sleep(time.Second)
	}
	msg := "couldn't reach the node over a new connection - the ruleset reverts by itself"
	if last != nil {
		msg += ": " + last.Error()
	}
	return false, msg
}

// firewallConfirmFresh calls FirewallConfirm over a connection of its own.
func firewallConfirmFresh(node *store.Node) error {
	tlsConfig, err := pki.ClientTLSConfig(node.CACertPEM, node.ServiceCertPEM, node.ServiceKeyPEM)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(node.Addr(), grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, err := janusv1alpha1.NewNetworkServiceClient(conn).FirewallConfirm(ctx, &emptypb.Empty{}, grpc.WaitForReady(true)); err != nil {
		return errors.New(status.Convert(err).Message())
	}
	return nil
}
