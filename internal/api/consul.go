package api

import (
	"context"
	"log"
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
	managed, err := n.Consul.Managed()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &janusv1alpha1.ConsulStatusResponse{
		State: janusv1alpha1.ModuleState_MODULE_STATE_STOPPED, Configured: !isDefault, FilesDir: consul.FilesDir(),
		Managed: managed != nil,
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
	for _, s := range a.Services {
		svc := &janusv1alpha1.ConsulService{Id: s.ID, Name: s.Name, Address: s.Address,
			Port: uint32(max(s.Port, 0)), Tags: s.Tags, Meta: s.Meta, Status: s.Status, Managed: s.Managed()} //nolint:gosec // G115: a port, 0-65535
		for _, c := range s.Checks {
			svc.Checks = append(svc.Checks, &janusv1alpha1.ConsulCheck{Id: c.ID, Name: c.Name, Status: c.Status, Output: c.Output})
		}
		resp.Services = append(resp.Services, svc)
	}
	if a.ServicesError != "" && resp.Error == "" {
		resp.Error = a.ServicesError
	}
	return resp, nil
}

func managedToProto(m *consul.Managed) *janusv1alpha1.ConsulManaged {
	if m == nil {
		return &janusv1alpha1.ConsulManaged{}
	}
	r := m.Redacted()
	return &janusv1alpha1.ConsulManaged{
		ManagedBy: r.ManagedBy, Datacenter: r.Datacenter, NodeName: r.NodeName, RetryJoin: r.RetryJoin,
		AdvertiseAddr: r.AdvertiseAddr, HasEncrypt: m.Encrypt != "", HasAgentToken: m.AgentToken != "",
		CaPem: r.CAPEM, NodeMeta: r.NodeMeta,
		Services: &janusv1alpha1.ConsulManagedServices{Janus: r.Services.Janus, Exporters: r.Services.Exporters,
			Frontends: r.Services.Frontends, Urls: r.Services.URLs},
	}
}

func managedFromProto(p *janusv1alpha1.ConsulManaged) *consul.Managed {
	if p.GetManagedBy() == "" {
		return nil
	}
	return &consul.Managed{
		ManagedBy: p.GetManagedBy(), Datacenter: p.GetDatacenter(), NodeName: p.GetNodeName(), RetryJoin: p.GetRetryJoin(),
		AdvertiseAddr: p.GetAdvertiseAddr(), Encrypt: p.GetEncrypt(), AgentToken: p.GetAgentToken(), CAPEM: p.GetCaPem(),
		NodeMeta: p.GetNodeMeta(),
		Services: consul.Toggles{Janus: p.GetServices().GetJanus(), Exporters: p.GetServices().GetExporters(),
			Frontends: p.GetServices().GetFrontends(), URLs: p.GetServices().GetUrls()},
	}
}

func (n *Network) ConsulManagedGet(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.ConsulManagedGetResponse, error) {
	if n.Consul == nil || !n.Consul.Available() {
		return &janusv1alpha1.ConsulManagedGetResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED, Managed: &janusv1alpha1.ConsulManaged{}}, nil
	}
	m, err := n.Consul.Managed()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &janusv1alpha1.ConsulManagedGetResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_STOPPED, Managed: managedToProto(m)}
	if n.Services != nil {
		if st, err := n.Services.State(consul.ServiceID); err == nil && st.State == "running" {
			resp.State = janusv1alpha1.ModuleState_MODULE_STATE_RUNNING
		}
	}
	if m != nil {
		st := n.ConsulRegistrar.Status()
		if !st.At.IsZero() {
			resp.SyncedUnix = st.At.Unix()
		}
		resp.SyncError, resp.Registered, resp.Maintenance = st.Err, int32(min(st.Registered, 1<<30)), st.Maintenance //nolint:gosec // G115: bounded
	}
	return resp, nil
}

func (n *Network) ConsulManagedSet(ctx context.Context, req *janusv1alpha1.ConsulManaged) (*janusv1alpha1.ConsulManagedSetResponse, error) {
	m, err := n.consul()
	if err != nil {
		return nil, err
	}
	next := managedFromProto(req)
	if next == nil {
		// The layer goes: the services it registered first, while its
		// token is still the agent's.
		fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := n.ConsulRegistrar.Forget(fctx); err != nil {
			log.Printf("consul: the managed layer's services: %v - left to the agent", err)
		}
		cancel()
	}
	errs, restarted, err := m.SetManaged(next, req.GetHasEncrypt(), req.GetHasAgentToken())
	if err != nil {
		if len(errs) > 0 {
			return &janusv1alpha1.ConsulManagedSetResponse{Errors: errs}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	if next == nil {
		log.Printf("consul: the managed layer removed")
	} else if restarted {
		log.Printf("consul: managed layer %s applied - the agent restarts", next.ManagedBy)
	}
	n.ConsulRegistrar.Poke()
	return &janusv1alpha1.ConsulManagedSetResponse{Accepted: true, Restarted: restarted}, nil
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
