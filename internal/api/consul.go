package api

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/consul"
)

func (n *Network) consul() (*consul.Manager, error) {
	if n.Consul == nil || !n.Consul.Available() {
		return nil, status.Error(codes.FailedPrecondition, "consul isn't in this node's image - optional modules are chosen when the image is built")
	}
	return n.Consul, nil
}

func (n *Network) ConsulStatus(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.ConsulStatusResponse, error) {
	if n.Consul == nil || !n.Consul.Available() {
		return &janusv1alpha1.ConsulStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED}, nil
	}
	_, isDefault, _, err := n.Consul.Saved()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &janusv1alpha1.ConsulStatusResponse{
		State: janusv1alpha1.ModuleState_MODULE_STATE_STOPPED, Configured: !isDefault, FilesDir: consul.FilesDir(),
	}
	if n.Services != nil {
		if st, err := n.Services.State(consul.ServiceID); err == nil {
			resp.ServiceState, resp.ServiceError = st.State, st.LastError
		}
	}
	if resp.ServiceState != "running" {
		return resp, nil
	}
	resp.State = janusv1alpha1.ModuleState_MODULE_STATE_RUNNING
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a, err := n.Consul.Agent(ctx)
	if err != nil {
		resp.Error = err.Error()
		return resp, nil
	}
	resp.NodeName, resp.NodeId, resp.Datacenter, resp.Server, resp.Version, resp.Leader =
		a.NodeName, a.NodeID, a.Datacenter, a.Server, a.Version, a.Leader
	for _, m := range a.Members {
		resp.Members = append(resp.Members, &janusv1alpha1.ConsulMember{
			Name: m.Name, Address: m.Address, Status: m.Status, Role: m.Role, Datacenter: m.Datacenter})
	}
	return resp, nil
}

func (n *Network) ConsulGetConfig(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.ConsulGetConfigResponse, error) {
	m, err := n.consul()
	if err != nil {
		return nil, err
	}
	cfg, isDefault, files, err := m.Saved()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &janusv1alpha1.ConsulGetConfigResponse{Config: []byte(cfg), IsDefault: isDefault, Files: files}, nil
}

func (n *Network) ConsulApplyConfig(_ context.Context, req *janusv1alpha1.ConsulApplyConfigRequest) (*janusv1alpha1.ConsulApplyConfigResponse, error) {
	m, err := n.consul()
	if err != nil {
		return nil, err
	}
	cfg := string(req.GetConfig())
	if req.GetValidateOnly() {
		errs, err := m.Check(cfg, req.GetFiles())
		return &janusv1alpha1.ConsulApplyConfigResponse{Accepted: err == nil, Errors: errs}, nil
	}
	errs, err := m.Apply(cfg, req.GetFiles())
	if err != nil {
		if len(errs) > 0 {
			return &janusv1alpha1.ConsulApplyConfigResponse{Errors: errs}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &janusv1alpha1.ConsulApplyConfigResponse{Accepted: true}, nil
}
