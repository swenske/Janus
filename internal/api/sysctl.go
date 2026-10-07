package api

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/firewall"
	"github.com/swenske/Janus/internal/sysctl"
)

// The kernel parameters (internal/sysctl): the whitelist an operator may
// change on trial, and the CIS benchmark's, read-only.

func (s *System) sysctl() (*sysctl.Manager, error) {
	if s.Sysctl == nil {
		return nil, status.Error(codes.Unavailable, "kernel parameters aren't managed by this janusd")
	}
	return s.Sysctl, nil
}

func sysctlActor(ctx context.Context) sysctl.Actor {
	c, _ := CallerFrom(ctx)
	return sysctl.Actor{Name: c.Name, Roles: c.Roles, Via: c.Via}
}

func (s *System) SysctlList(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.SysctlListResponse, error) {
	m, err := s.sysctl()
	if err != nil {
		return nil, err
	}
	snap := m.Snapshot()
	resp := &janusv1alpha1.SysctlListResponse{Managed: snap.Managed, Cis: sysctlCISProto(snap.CIS), Trial: sysctlTrialProto(snap.Trial), Observation: sysctlObservationProto(snap.Observation)}
	for _, ps := range snap.Params {
		resp.Parameters = append(resp.Parameters, sysctlParamProto(ps))
	}
	return resp, nil
}

func sysctlParamProto(ps sysctl.ParamState) *janusv1alpha1.SysctlParameter {
	p := ps.Param
	out := &janusv1alpha1.SysctlParameter{
		Name:           p.Name,
		Class:          janusv1alpha1.SysctlClass(p.Class),
		Kind:           janusv1alpha1.SysctlKind(p.Kind),
		Unit:           p.Unit,
		Value:          ps.Value,
		Missing:        ps.Missing,
		DefaultValue:   ps.Default,
		DefaultDynamic: p.Dynamic,
		Saved:          ps.HasSaved,
		SavedValue:     ps.Saved,
		OnTrial:        ps.OnTrial,
		Allowed:        p.Allowed,
		MaxItems:       uint32(p.MaxItems),
		Applies:        janusv1alpha1.SysctlApplies(p.Applies),
		Requires:       p.Requires,
		Summary:        p.Summary,
		Effect:         p.Effect,
		Risk:           p.Risk,
		Why:            p.Why,
		KernelDefault:  p.KernelDefault,
		HaproxyValue:   p.HAProxyValue,
		Warnings:       ps.Warnings,
	}
	for _, b := range p.Bounds {
		out.Bounds = append(out.Bounds, &janusv1alpha1.SysctlBound{Min: b.Min, Max: b.Max})
	}
	out.Sources = sysctlSources(p.Sources)
	if p.Requires == "nftables" && !firewall.Available() {
		out.Warnings = append(out.Warnings, "This node's image has no nftables extension: the parameter has no effect here.")
	}
	if r := ps.Recommendation; r != nil {
		rec := &janusv1alpha1.SysctlRecommendation{Value: r.Value, RuleId: r.RuleID, Rule: r.Rule, Sources: sysctlSources(r.Sources)}
		for _, m := range r.Measured {
			rec.Measured = append(rec.Measured, &janusv1alpha1.SysctlMeasurement{Name: m.Name, Value: m.Value, Window: m.Window})
		}
		out.Recommendation = rec
	}
	return out
}

func sysctlObservationProto(o *sysctl.Observation) *janusv1alpha1.SysctlObservation {
	if o == nil {
		return nil
	}
	out := &janusv1alpha1.SysctlObservation{SinceUnix: o.Since.Unix(), WindowHours: uint32(sysctl.Window.Hours()), MinHours: sysctl.MinHours}
	for _, def := range sysctl.Signals {
		st := o.Signals[def.ID]
		sig := &janusv1alpha1.SysctlSignal{Id: def.ID, Title: def.Title, Measure: def.Measure, Seen: def.Seen, Hours: uint32(st.Hours), Peak: sysctl.PeakText(def.ID, st)}
		if def.Kind == sysctl.Counter {
			sig.Total = uint64(st.Total)
		}
		if !st.LastSeen.IsZero() {
			sig.LastSeenUnix = st.LastSeen.Unix()
		}
		out.Signals = append(out.Signals, sig)
	}
	return out
}

func sysctlSources(in []sysctl.Source) []*janusv1alpha1.SysctlSource {
	var out []*janusv1alpha1.SysctlSource
	for _, s := range in {
		out = append(out, &janusv1alpha1.SysctlSource{Title: s.Title, Url: s.URL})
	}
	return out
}

func sysctlCISProto(a sysctl.Audit) *janusv1alpha1.SysctlCIS {
	out := &janusv1alpha1.SysctlCIS{Benchmark: sysctl.Benchmark, Profile: sysctl.Profile, Compliant: uint32(a.Compliant())}
	for _, r := range a.Results {
		c := r.Control
		out.Controls = append(out.Controls, &janusv1alpha1.SysctlCISControl{
			Ids: c.IDs, Level: uint32(c.Level), Key: c.Key, Value: r.Value, Want: c.Want,
			Compliant: r.Compliant, Problems: r.Problems, Interfaces: c.Interfaces,
		})
	}
	return out
}

func sysctlActorProto(a sysctl.Actor) *janusv1alpha1.SysctlActor {
	return &janusv1alpha1.SysctlActor{Name: a.Name, Roles: a.Roles, Via: a.Via}
}

func sysctlChangesProto(in []sysctl.ChangeRecord) []*janusv1alpha1.SysctlChange {
	var out []*janusv1alpha1.SysctlChange
	for _, c := range in {
		out = append(out, &janusv1alpha1.SysctlChange{Name: c.Name, OldValue: c.Old, NewValue: c.New, ToDefault: c.Reset})
	}
	return out
}

func sysctlTrialProto(t *sysctl.Trial) *janusv1alpha1.SysctlTrial {
	if t == nil {
		return nil
	}
	return &janusv1alpha1.SysctlTrial{
		StartedUnix:     t.Started.Unix(),
		RevertAtUnix:    t.RevertAt.Unix(),
		Changes:         sysctlChangesProto(t.Changes),
		Actor:           sysctlActorProto(t.Actor),
		HaproxyReloaded: t.Reloaded,
	}
}

func (s *System) SysctlApply(ctx context.Context, req *janusv1alpha1.SysctlApplyRequest) (*janusv1alpha1.SysctlApplyResponse, error) {
	m, err := s.sysctl()
	if err != nil {
		return nil, err
	}
	r := sysctl.Request{
		ResetAll:      req.GetResetAll(),
		Timeout:       time.Duration(req.GetConfirmTimeoutSeconds()) * time.Second,
		ReloadHAProxy: req.GetReloadHaproxy(),
		Actor:         sysctlActor(ctx),
	}
	for _, c := range req.GetChanges() {
		r.Changes = append(r.Changes, sysctl.Change{Name: c.GetName(), Value: c.GetValue(), Reset: c.GetToDefault()})
	}
	if req.GetValidateOnly() {
		changes, err := m.Validate(r)
		if resp, ok := sysctlRefused(err); ok {
			return resp, nil
		}
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		return &janusv1alpha1.SysctlApplyResponse{Accepted: true, Changes: sysctlChangesProto(changes)}, nil
	}
	t, err := m.ApplyTrial(r)
	if resp, ok := sysctlRefused(err); ok {
		return resp, nil
	}
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &janusv1alpha1.SysctlApplyResponse{Accepted: true, Changes: sysctlChangesProto(t.Changes), Trial: sysctlTrialProto(t)}, nil
}

// sysctlRefused turns refused changes into a response that lists them.
func sysctlRefused(err error) (*janusv1alpha1.SysctlApplyResponse, bool) {
	var inv *sysctl.InvalidError
	if !errors.As(err, &inv) {
		return nil, false
	}
	resp := &janusv1alpha1.SysctlApplyResponse{}
	for _, f := range inv.Fields {
		resp.Errors = append(resp.Errors, &janusv1alpha1.SysctlFieldError{Name: f.Name, Message: f.Message})
	}
	return resp, true
}

func (s *System) SysctlConfirm(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.SysctlTrialResponse, error) {
	m, err := s.sysctl()
	if err != nil {
		return nil, err
	}
	t, err := m.Confirm(ConnOpened(ctx), sysctlActor(ctx))
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &janusv1alpha1.SysctlTrialResponse{Trial: sysctlTrialProto(t)}, nil
}

func (s *System) SysctlCancel(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.SysctlTrialResponse, error) {
	m, err := s.sysctl()
	if err != nil {
		return nil, err
	}
	t, err := m.Cancel(sysctlActor(ctx))
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &janusv1alpha1.SysctlTrialResponse{Trial: sysctlTrialProto(t)}, nil
}

func (s *System) SysctlHistory(_ context.Context, req *janusv1alpha1.SysctlHistoryRequest) (*janusv1alpha1.SysctlHistoryResponse, error) {
	if _, err := s.sysctl(); err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit == 0 {
		limit = 100
	}
	entries, err := sysctl.History(min(limit, 1000))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &janusv1alpha1.SysctlHistoryResponse{}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &janusv1alpha1.SysctlHistoryEntry{
			TimeUnix: e.Time.Unix(), Actor: sysctlActorProto(e.Actor), Action: e.Action,
			Changes: sysctlChangesProto(e.Changes), Detail: e.Detail,
		})
	}
	return resp, nil
}
