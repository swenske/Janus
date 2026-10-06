package nodeproxy

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// The node's kernel parameters (System › Sysctl): the whitelist HAProxy
// depends on, changed on trial and confirmed - over a connection opened
// after the apply, as the node requires, and for the page's user - and
// the CIS benchmark's, read-only.

type sysctlChangeJSON struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Reset bool   `json:"reset"`
}

type sysctlApplyJSON struct {
	Changes               []sysctlChangeJSON `json:"changes"`
	ResetAll              bool               `json:"reset_all"`
	ConfirmTimeoutSeconds uint32             `json:"confirm_timeout_seconds"`
	ReloadHAProxy         bool               `json:"reload_haproxy"`
}

func (r sysctlApplyJSON) request(validateOnly bool) *janusv1alpha1.SysctlApplyRequest {
	out := &janusv1alpha1.SysctlApplyRequest{
		ResetAll: r.ResetAll, ConfirmTimeoutSeconds: r.ConfirmTimeoutSeconds,
		ReloadHaproxy: r.ReloadHAProxy, ValidateOnly: validateOnly,
	}
	for _, c := range r.Changes {
		out.Changes = append(out.Changes, &janusv1alpha1.SysctlChangeRequest{Name: c.Name, Value: c.Value, ToDefault: c.Reset})
	}
	return out
}

func registerSysctlRoutes(mux *http.ServeMux, node *store.Node) {
	sys := func(fn func(context.Context, janusv1alpha1.SystemServiceClient, *http.Request) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
				return fn(ctx, janusv1alpha1.NewSystemServiceClient(c), r)
			})
		}
	}
	mux.HandleFunc("GET /api/system/sysctl", sys(func(ctx context.Context, c janusv1alpha1.SystemServiceClient, _ *http.Request) (any, error) {
		resp, err := c.SysctlList(ctx, &emptypb.Empty{})
		if err != nil {
			return nil, err
		}
		return sysctlListJSON(resp), nil
	}))
	apply := func(validateOnly bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var req sysctlApplyJSON
			if !decodeJSON(w, r, &req) {
				return
			}
			sys(func(ctx context.Context, c janusv1alpha1.SystemServiceClient, _ *http.Request) (any, error) {
				resp, err := c.SysctlApply(ctx, req.request(validateOnly))
				if err != nil {
					return nil, err
				}
				var errs []map[string]string
				for _, e := range resp.GetErrors() {
					errs = append(errs, map[string]string{"name": e.GetName(), "message": e.GetMessage()})
				}
				return map[string]any{
					"accepted": resp.GetAccepted(),
					"errors":   errs,
					"changes":  sysctlChangesJSON(resp.GetChanges()),
					"trial":    sysctlTrialJSON(resp.GetTrial()),
				}, nil
			})(w, r)
		}
	}
	mux.HandleFunc("POST /api/system/sysctl/check", apply(true))
	mux.HandleFunc("POST /api/system/sysctl/apply", apply(false))
	mux.HandleFunc("POST /api/system/sysctl/confirm", func(w http.ResponseWriter, r *http.Request) {
		trial, err := sysctlConfirmFresh(r.Context(), node)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSONBody(w, http.StatusOK, map[string]any{"confirmed": true, "trial": sysctlTrialJSON(trial)})
	})
	mux.HandleFunc("POST /api/system/sysctl/cancel", sys(func(ctx context.Context, c janusv1alpha1.SystemServiceClient, _ *http.Request) (any, error) {
		resp, err := c.SysctlCancel(ctx, &emptypb.Empty{})
		if err != nil {
			return nil, err
		}
		return map[string]any{"cancelled": true, "trial": sysctlTrialJSON(resp.GetTrial())}, nil
	}))
	mux.HandleFunc("GET /api/system/sysctl/history", sys(func(ctx context.Context, c janusv1alpha1.SystemServiceClient, r *http.Request) (any, error) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		resp, err := c.SysctlHistory(ctx, &janusv1alpha1.SysctlHistoryRequest{Limit: uint32(max(0, limit))})
		if err != nil {
			return nil, err
		}
		entries := []map[string]any{}
		for _, e := range resp.GetEntries() {
			entries = append(entries, map[string]any{
				"time_unix": e.GetTimeUnix(), "actor": sysctlActorJSON(e.GetActor()), "action": e.GetAction(),
				"changes": sysctlChangesJSON(e.GetChanges()), "detail": e.GetDetail(),
			})
		}
		return map[string]any{"entries": entries}, nil
	}))
}

// sysctlConfirmFresh calls SysctlConfirm over a connection of its own,
// for the page's user: the shared one predates the apply.
func sysctlConfirmFresh(ctx context.Context, node *store.Node) (*janusv1alpha1.SysctlTrial, error) {
	opts, err := dialOptions(node)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(node.Addr(), opts...)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := janusv1alpha1.NewSystemServiceClient(conn).SysctlConfirm(ctx, &emptypb.Empty{}, grpc.WaitForReady(true))
	if err != nil {
		return nil, errors.New(status.Convert(err).Message())
	}
	return resp.GetTrial(), nil
}

// enumWord is an enum value's name as the page uses it:
// SYSCTL_CLASS_READ_ONLY -> "read_only".
func enumWord(s, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(s, prefix))
}

func sysctlListJSON(resp *janusv1alpha1.SysctlListResponse) map[string]any {
	params := []map[string]any{}
	for _, p := range resp.GetParameters() {
		bounds := []map[string]int64{}
		for _, b := range p.GetBounds() {
			bounds = append(bounds, map[string]int64{"min": b.GetMin(), "max": b.GetMax()})
		}
		sources := []map[string]string{}
		for _, s := range p.GetSources() {
			sources = append(sources, map[string]string{"title": s.GetTitle(), "url": s.GetUrl()})
		}
		var rec any
		if r := p.GetRecommendation(); r != nil {
			measured := []map[string]string{}
			for _, m := range r.GetMeasured() {
				measured = append(measured, map[string]string{"name": m.GetName(), "value": m.GetValue(), "window": m.GetWindow()})
			}
			rec = map[string]any{"value": r.GetValue(), "rule_id": r.GetRuleId(), "rule": r.GetRule(), "measured": measured}
		}
		params = append(params, map[string]any{
			"name": p.GetName(), "class": enumWord(p.GetClass().String(), "SYSCTL_CLASS_"),
			"kind": enumWord(p.GetKind().String(), "SYSCTL_KIND_"), "unit": p.GetUnit(),
			"value": p.GetValue(), "missing": p.GetMissing(),
			"default": p.GetDefaultValue(), "default_dynamic": p.GetDefaultDynamic(),
			"saved": p.GetSaved(), "saved_value": p.GetSavedValue(), "on_trial": p.GetOnTrial(),
			"bounds": bounds, "allowed": nonNil(p.GetAllowed()), "max_items": p.GetMaxItems(),
			"applies": enumWord(p.GetApplies().String(), "SYSCTL_APPLIES_"), "requires": p.GetRequires(),
			"summary": p.GetSummary(), "effect": p.GetEffect(), "risk": p.GetRisk(), "why": p.GetWhy(),
			"kernel_default": p.GetKernelDefault(), "haproxy_value": p.GetHaproxyValue(),
			"sources": sources, "warnings": nonNil(p.GetWarnings()), "recommendation": rec,
		})
	}
	cis := resp.GetCis()
	controls := []map[string]any{}
	for _, c := range cis.GetControls() {
		controls = append(controls, map[string]any{
			"ids": c.GetIds(), "level": c.GetLevel(), "key": c.GetKey(), "value": c.GetValue(),
			"want": c.GetWant(), "compliant": c.GetCompliant(), "problems": nonNil(c.GetProblems()), "interfaces": c.GetInterfaces(),
		})
	}
	return map[string]any{
		"managed":    resp.GetManaged(),
		"parameters": params,
		"cis": map[string]any{
			"benchmark": cis.GetBenchmark(), "profile": cis.GetProfile(),
			"compliant": cis.GetCompliant(), "total": len(cis.GetControls()), "controls": controls,
		},
		"trial": sysctlTrialJSON(resp.GetTrial()),
	}
}

// nonNil keeps an empty list a list in JSON.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func sysctlChangesJSON(in []*janusv1alpha1.SysctlChange) []map[string]any {
	out := []map[string]any{}
	for _, c := range in {
		out = append(out, map[string]any{"name": c.GetName(), "old": c.GetOldValue(), "new": c.GetNewValue(), "reset": c.GetToDefault()})
	}
	return out
}

func sysctlActorJSON(a *janusv1alpha1.SysctlActor) map[string]any {
	return map[string]any{"name": a.GetName(), "roles": nonNil(a.GetRoles()), "via": a.GetVia()}
}

func sysctlTrialJSON(t *janusv1alpha1.SysctlTrial) any {
	if t == nil {
		return nil
	}
	return map[string]any{
		"started_unix": t.GetStartedUnix(), "revert_at_unix": t.GetRevertAtUnix(),
		"changes": sysctlChangesJSON(t.GetChanges()), "actor": sysctlActorJSON(t.GetActor()),
		"haproxy_reloaded": t.GetHaproxyReloaded(),
	}
}
