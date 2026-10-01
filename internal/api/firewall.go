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
)

func (n *Network) firewall() (*firewall.Manager, error) {
	if !firewall.Available() {
		return nil, status.Error(codes.FailedPrecondition, firewall.ErrNotAvailable.Error())
	}
	if n.Firewall == nil {
		return nil, status.Error(codes.Unavailable, "the firewall isn't managed by this janusd")
	}
	return n.Firewall, nil
}

func (n *Network) FirewallList(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.FirewallListResponse, error) {
	if !firewall.Available() {
		return &janusv1alpha1.FirewallListResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED}, nil
	}
	fw, err := n.firewall()
	if err != nil {
		return nil, err
	}
	live, err := firewall.Live()
	if err != nil {
		return &janusv1alpha1.FirewallListResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_ERROR}, status.Error(codes.Internal, err.Error())
	}
	_, isDefault, err := firewall.Saved()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	pending, revertAt := fw.Trial()
	resp := &janusv1alpha1.FirewallListResponse{
		State:        janusv1alpha1.ModuleState_MODULE_STATE_STOPPED,
		Ruleset:      []byte(live),
		Configured:   !isDefault,
		TrialPending: pending,
	}
	if !isDefault || pending {
		resp.State = janusv1alpha1.ModuleState_MODULE_STATE_RUNNING
	}
	if pending {
		resp.TrialRevertAtUnix = revertAt.Unix()
	}
	return resp, nil
}

func (n *Network) FirewallGetRuleset(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.FirewallGetRulesetResponse, error) {
	if _, err := n.firewall(); err != nil {
		return nil, err
	}
	rs, isDefault, err := firewall.Saved()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &janusv1alpha1.FirewallGetRulesetResponse{Ruleset: []byte(rs), IsDefault: isDefault}, nil
}

func (n *Network) FirewallApplyRuleset(_ context.Context, req *janusv1alpha1.FirewallApplyRulesetRequest) (*janusv1alpha1.FirewallApplyRulesetResponse, error) {
	fw, err := n.firewall()
	if err != nil {
		return nil, err
	}
	rs := string(req.GetRuleset())
	if req.GetValidateOnly() {
		errs, err := firewall.Check(rs)
		return &janusv1alpha1.FirewallApplyRulesetResponse{Accepted: err == nil, Errors: errs}, nil
	}
	timeout := time.Duration(req.GetConfirmTimeoutSeconds()) * time.Second
	if timeout != 0 && (timeout < firewall.MinTrial || timeout > firewall.MaxTrial) {
		return nil, status.Errorf(codes.InvalidArgument, "confirm_timeout_seconds must be between %d and %d", int(firewall.MinTrial.Seconds()), int(firewall.MaxTrial.Seconds()))
	}
	revertAt, errs, err := fw.ApplyTrial(rs, timeout)
	if err != nil {
		if len(errs) > 0 {
			return &janusv1alpha1.FirewallApplyRulesetResponse{Accepted: false, Errors: errs}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &janusv1alpha1.FirewallApplyRulesetResponse{Accepted: true, RevertAtUnix: revertAt.Unix()}, nil
}

func (n *Network) FirewallConfirm(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.FirewallConfirmResponse, error) {
	fw, err := n.firewall()
	if err != nil {
		return nil, err
	}
	switch err := fw.Confirm(ConnOpened(ctx)); {
	case errors.Is(err, firewall.ErrNoTrial), errors.Is(err, firewall.ErrOldConnection):
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	case err != nil:
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &janusv1alpha1.FirewallConfirmResponse{}, nil
}

func (n *Network) FirewallSets(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.FirewallSetsResponse, error) {
	fw, err := n.firewall()
	if err != nil {
		return nil, err
	}
	sets, err := fw.Sets()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &janusv1alpha1.FirewallSetsResponse{}
	for _, s := range sets {
		resp.Sets = append(resp.Sets, firewallSetProto(s))
	}
	return resp, nil
}

func (n *Network) FirewallSetUpdate(_ context.Context, req *janusv1alpha1.FirewallSetUpdateRequest) (*janusv1alpha1.FirewallSetUpdateResponse, error) {
	fw, err := n.firewall()
	if err != nil {
		return nil, err
	}
	if err := firewall.ValidSetRef(req.GetFamily(), req.GetTable(), req.GetSet()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	var add []firewall.Element
	for _, e := range req.GetAdd() {
		if err := firewall.ValidElement(e.GetValue()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		add = append(add, firewall.Element{Value: e.GetValue(), Timeout: time.Duration(e.GetTimeoutSeconds()) * time.Second})
	}
	for _, v := range req.GetDelete() {
		if err := firewall.ValidElement(v); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	if err := fw.UpdateSet(req.GetFamily(), req.GetTable(), req.GetSet(), add, req.GetDelete()); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	sets, err := fw.Sets()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	for _, s := range sets {
		if s.Family == req.GetFamily() && s.Table == req.GetTable() && s.Name == req.GetSet() {
			return &janusv1alpha1.FirewallSetUpdateResponse{Set: firewallSetProto(s)}, nil
		}
	}
	return &janusv1alpha1.FirewallSetUpdateResponse{}, nil
}

func firewallSetProto(s firewall.Set) *janusv1alpha1.FirewallSet {
	out := &janusv1alpha1.FirewallSet{Family: s.Family, Table: s.Table, Name: s.Name, Type: s.Type, Flags: s.Flags}
	for _, e := range s.Elements {
		out.Elements = append(out.Elements, &janusv1alpha1.FirewallSetElement{
			Value:          e.Value,
			TimeoutSeconds: uint32(e.Timeout / time.Second),
			ExpiresSeconds: uint32(e.Expires / time.Second),
			Persistent:     e.Persistent,
		})
	}
	return out
}
