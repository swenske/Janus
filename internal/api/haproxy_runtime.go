package api

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/haproxy"
)

// Runtime diagnostics: map and ACL lookups, stick tables, streams, the
// errors HAProxy captured - internal/haproxy's runtime_*.go.

// listLimit is how many table entries or streams a call returns: 500
// unless asked, 5000 at most - a busy node has hundreds of thousands.
func listLimit(asked uint32) int {
	switch {
	case asked == 0:
		return 500
	case asked > 5000:
		return 5000
	}
	return int(asked)
}

func (h *HAProxy) MapLookup(_ context.Context, req *janusv1alpha1.PatternLookupRequest) (*janusv1alpha1.PatternLookupResponse, error) {
	m, err := h.Manager.MapLookup(req.GetName(), req.GetValue())
	if err != nil {
		return nil, haproxyError(err)
	}
	return patternLookupResponse(m), nil
}

func (h *HAProxy) ACLLookup(_ context.Context, req *janusv1alpha1.PatternLookupRequest) (*janusv1alpha1.PatternLookupResponse, error) {
	m, err := h.Manager.ACLLookup(req.GetName(), req.GetValue())
	if err != nil {
		return nil, haproxyError(err)
	}
	return patternLookupResponse(m), nil
}

func patternLookupResponse(m haproxy.PatternMatch) *janusv1alpha1.PatternLookupResponse {
	return &janusv1alpha1.PatternLookupResponse{Found: m.Found, Method: m.Method, CaseInsensitive: m.CaseInsensitive, Pattern: m.Pattern, Value: m.Value}
}

func (h *HAProxy) TableList(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.TableListResponse, error) {
	tables, err := h.Manager.TableList()
	if err != nil {
		return nil, haproxyError(err)
	}
	resp := &janusv1alpha1.TableListResponse{}
	for _, t := range tables {
		resp.Tables = append(resp.Tables, stickTable(t))
	}
	return resp, nil
}

func (h *HAProxy) TableShow(_ context.Context, req *janusv1alpha1.TableShowRequest) (*janusv1alpha1.TableShowResponse, error) {
	t, entries, truncated, err := h.Manager.TableShow(req.GetTable(), req.GetKey(), tableFilters(req.GetFilters()), listLimit(req.GetLimit()))
	if err != nil {
		return nil, haproxyError(err)
	}
	resp := &janusv1alpha1.TableShowResponse{Table: stickTable(t), Truncated: truncated}
	for _, e := range entries {
		entry := &janusv1alpha1.TableEntry{Key: e.Key, Use: e.Use, ExpiresMs: e.ExpiresMS}
		for _, d := range e.Data {
			entry.Data = append(entry.Data, &janusv1alpha1.TableData{Name: d.Name, Value: d.Value})
		}
		resp.Entries = append(resp.Entries, entry)
	}
	return resp, nil
}

func (h *HAProxy) TableClear(_ context.Context, req *janusv1alpha1.TableClearRequest) (*emptypb.Empty, error) {
	filters := tableFilters(req.GetFilters())
	if err := h.Manager.TableClear(req.GetTable(), req.GetKey(), filters, req.GetAll()); err != nil {
		return nil, haproxyError(err)
	}
	events.Publish("haproxy.table.cleared", map[string]any{"table": req.GetTable(), "key": req.GetKey(), "filters": filters, "all": req.GetAll()})
	return &emptypb.Empty{}, nil
}

func stickTable(t haproxy.StickTable) *janusv1alpha1.StickTable {
	return &janusv1alpha1.StickTable{Name: t.Name, Type: t.Type, Size: t.Size, Used: t.Used}
}

func tableFilters(in []*janusv1alpha1.TableFilter) []haproxy.TableFilter {
	var out []haproxy.TableFilter
	for _, f := range in {
		out = append(out, haproxy.TableFilter{Data: f.GetData(), Op: f.GetOp(), Value: f.GetValue()})
	}
	return out
}

func (h *HAProxy) SessionList(_ context.Context, req *janusv1alpha1.SessionListRequest) (*janusv1alpha1.SessionListResponse, error) {
	sessions, truncated, err := h.Manager.SessionList(haproxy.SessionFilter{Frontend: req.GetFrontend(), Backend: req.GetBackend(), Source: req.GetSource()}, listLimit(req.GetLimit()))
	if err != nil {
		return nil, haproxyError(err)
	}
	resp := &janusv1alpha1.SessionListResponse{Truncated: truncated}
	for _, s := range sessions {
		resp.Sessions = append(resp.Sessions, &janusv1alpha1.Session{
			Id: s.ID, Proto: s.Proto, Source: s.Source, Frontend: s.Frontend, Backend: s.Backend, Server: s.Server,
			Age: s.Age, AgeSeconds: s.AgeSeconds, Calls: s.Calls, Expires: s.Expires, Raw: s.Raw,
		})
	}
	return resp, nil
}

func (h *HAProxy) SessionShutdown(_ context.Context, req *janusv1alpha1.SessionShutdownRequest) (*emptypb.Empty, error) {
	if err := h.Manager.SessionShutdown(req.GetId()); err != nil {
		return nil, haproxyError(err)
	}
	events.Publish("haproxy.session.shutdown", map[string]string{"id": req.GetId()})
	return &emptypb.Empty{}, nil
}

func (h *HAProxy) ErrorsShow(_ context.Context, req *janusv1alpha1.ErrorsShowRequest) (*janusv1alpha1.ErrorsShowResponse, error) {
	total, captured, err := h.Manager.ErrorsShow(req.GetProxy(), req.GetDirection())
	if err != nil {
		return nil, haproxyError(err)
	}
	resp := &janusv1alpha1.ErrorsShowResponse{Total: total}
	for _, e := range captured {
		ce := &janusv1alpha1.CapturedError{
			When: e.When, WhenUnixMs: e.WhenUnixMS, ProxyKind: e.ProxyKind, Proxy: e.Proxy, Direction: e.Direction,
			Frontend: e.Frontend, Backend: e.Backend, Server: e.Server, Source: e.Source, Event: e.Event,
			Position: int32(e.Position), Details: e.Details, //nolint:gosec // G115: a buffer offset, below HAProxy's tune.bufsize
		}
		for _, l := range e.Lines {
			ce.Lines = append(ce.Lines, &janusv1alpha1.ErrorLine{Offset: int32(l.Offset), Continued: l.Continued, Text: l.Text, ErrorAt: int32(l.ErrorAt)}) //nolint:gosec // G115: offsets in a buffer of tune.bufsize bytes
		}
		resp.Errors = append(resp.Errors, ce)
	}
	return resp, nil
}
