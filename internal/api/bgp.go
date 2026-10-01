package api

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/bgp"
	"github.com/swenske/Janus/internal/extensions"
)

func (n *Network) bgp() (*bgp.Manager, error) {
	if n.BGP == nil || !n.BGP.Available() {
		return nil, status.Error(codes.FailedPrecondition, "bird isn't in this node's image - optional modules are chosen when the image is built")
	}
	return n.BGP, nil
}

func (n *Network) BGPStatus(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.BGPStatusResponse, error) {
	if n.BGP == nil || !n.BGP.Available() {
		return &janusv1alpha1.BGPStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED}, nil
	}
	_, isDefault, err := n.BGP.Saved()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &janusv1alpha1.BGPStatusResponse{
		State:          janusv1alpha1.ModuleState_MODULE_STATE_STOPPED,
		Configured:     !isDefault,
		HaproxyHealthy: n.BGP.HAProxyHealthy(),
	}
	st, err := n.BGP.Status()
	switch {
	case err == nil:
		resp.State = janusv1alpha1.ModuleState_MODULE_STATE_RUNNING
	case errors.Is(err, extensions.ErrNotRunning):
		return resp, nil // stopped, or waiting for a configuration
	default:
		resp.State = janusv1alpha1.ModuleState_MODULE_STATE_ERROR
		resp.Error = err.Error()
		return resp, nil
	}
	resp.Version, resp.RouterId = st.Version, st.RouterID
	for _, p := range st.Protocols {
		pp := &janusv1alpha1.BGPProtocol{
			Name: p.Name, Protocol: p.Proto, Table: p.Table, State: p.State, Since: p.Since, Info: p.Info,
			BgpState: p.BGPState, NeighborAddress: p.NeighborAddress, NeighborAs: p.NeighborAS, LocalAs: p.LocalAS,
			LastError: p.LastError, HeldDown: p.HeldDown,
		}
		for _, c := range p.Channels {
			pp.Channels = append(pp.Channels, &janusv1alpha1.BGPChannel{
				Name: c.Name, State: c.State, Imported: c.Imported, Exported: c.Exported, Preferred: c.Preferred,
			})
		}
		resp.Protocols = append(resp.Protocols, pp)
	}
	return resp, nil
}

func (n *Network) BGPGetConfig(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.BGPGetConfigResponse, error) {
	m, err := n.bgp()
	if err != nil {
		return nil, err
	}
	cfg, isDefault, err := m.Saved()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &janusv1alpha1.BGPGetConfigResponse{Config: []byte(cfg), IsDefault: isDefault}, nil
}

func (n *Network) BGPApplyConfig(_ context.Context, req *janusv1alpha1.BGPApplyConfigRequest) (*janusv1alpha1.BGPApplyConfigResponse, error) {
	m, err := n.bgp()
	if err != nil {
		return nil, err
	}
	cfg := string(req.GetConfig())
	if req.GetValidateOnly() {
		errs, err := m.Check(cfg)
		return &janusv1alpha1.BGPApplyConfigResponse{Accepted: err == nil, Errors: errs}, nil
	}
	errs, err := m.Apply(cfg)
	if err != nil {
		if len(errs) > 0 {
			return &janusv1alpha1.BGPApplyConfigResponse{Accepted: false, Errors: errs}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &janusv1alpha1.BGPApplyConfigResponse{Accepted: true}, nil
}
