package api

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/vrrp"
)

func (n *Network) vrrp() (*vrrp.Manager, error) {
	if n.VRRP == nil || !n.VRRP.Available() {
		return nil, status.Error(codes.FailedPrecondition, "keepalived isn't in this node's image - optional modules are chosen when the image is built")
	}
	return n.VRRP, nil
}

func (n *Network) VRRPStatus(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.VRRPStatusResponse, error) {
	if n.VRRP == nil || !n.VRRP.Available() {
		return &janusv1alpha1.VRRPStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED}, nil
	}
	_, isDefault, err := n.VRRP.Saved()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &janusv1alpha1.VRRPStatusResponse{
		State:             janusv1alpha1.ModuleState_MODULE_STATE_STOPPED,
		Configured:        !isDefault,
		HaproxyHealthFile: vrrp.HealthFile(),
	}
	if n.HAProxyHealthy != nil {
		resp.HaproxyHealthy = n.HAProxyHealthy()
	}
	instances, err := n.VRRP.Status()
	switch {
	case err == nil:
		resp.State = janusv1alpha1.ModuleState_MODULE_STATE_RUNNING
	case errors.Is(err, extensions.ErrNotRunning):
		// stopped, or waiting for a configuration
	default:
		resp.State = janusv1alpha1.ModuleState_MODULE_STATE_ERROR
		resp.Error = err.Error()
	}
	for _, in := range instances {
		resp.Instances = append(resp.Instances, &janusv1alpha1.VRRPInstance{
			Name: in.Name, Role: in.State, Interface: in.Interface, VirtualRouterId: uint32(in.VRID),
			Priority: uint32(in.Priority), EffectivePriority: uint32(in.EffectivePriority),
			VirtualIps: in.VirtualIPs, LastTransitionUnix: in.LastTransition.Unix(), BecameMaster: in.BecameMaster,
		})
	}
	return resp, nil
}

func (n *Network) VRRPGetConfig(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.VRRPGetConfigResponse, error) {
	m, err := n.vrrp()
	if err != nil {
		return nil, err
	}
	cfg, isDefault, err := m.Saved()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &janusv1alpha1.VRRPGetConfigResponse{Config: []byte(cfg), IsDefault: isDefault}, nil
}

func (n *Network) VRRPApplyConfig(_ context.Context, req *janusv1alpha1.VRRPApplyConfigRequest) (*janusv1alpha1.VRRPApplyConfigResponse, error) {
	m, err := n.vrrp()
	if err != nil {
		return nil, err
	}
	cfg := string(req.GetConfig())
	if req.GetValidateOnly() {
		errs, err := m.Check(cfg)
		return &janusv1alpha1.VRRPApplyConfigResponse{Accepted: err == nil, Errors: errs}, nil
	}
	errs, err := m.Apply(cfg)
	if err != nil {
		if len(errs) > 0 {
			return &janusv1alpha1.VRRPApplyConfigResponse{Accepted: false, Errors: errs}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &janusv1alpha1.VRRPApplyConfigResponse{Accepted: true}, nil
}
