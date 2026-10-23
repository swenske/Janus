package api

import (
	"context"
	"net/netip"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/nodeexporter"
)

func (s *System) NodeExporterConfigGet(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.NodeExporterConfigResponse, error) {
	if err := s.nodeExporterPresent(); err != nil {
		return nil, err
	}
	return s.nodeExporterResponse(), nil
}

func (s *System) NodeExporterConfigSet(_ context.Context, req *janusv1alpha1.NodeExporterConfig) (*janusv1alpha1.NodeExporterConfigResponse, error) {
	if err := s.nodeExporterPresent(); err != nil {
		return nil, err
	}
	cfg := nodeexporter.Config{Enabled: req.GetEnabled(), Address: req.GetAddress(), Port: req.GetPort(), Collectors: req.GetCollectors()}
	if err := cfg.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := nodeexporter.Apply(s.Extensions, cfg, s.mainAddresses()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.nodeExporterResponse(), nil
}

// mainAddresses is the main interface's addresses - none outside a node
// whose network janusd manages.
func (s *System) mainAddresses() []netip.Addr {
	if s.Main == nil {
		return nil
	}
	return s.Main()
}

func (s *System) nodeExporterPresent() error {
	if s.Extensions == nil || !s.Extensions.Has(nodeexporter.ServiceID) {
		return status.Error(codes.FailedPrecondition, "the prometheus-node-exporter extension isn't in this node's image")
	}
	return nil
}

func (s *System) nodeExporterResponse() *janusv1alpha1.NodeExporterConfigResponse {
	cfg, isDefault, err := nodeexporter.Load()
	resp := &janusv1alpha1.NodeExporterConfigResponse{
		Config:    &janusv1alpha1.NodeExporterConfig{Enabled: cfg.Enabled, Address: cfg.Address, Port: cfg.Port, Collectors: cfg.Collectors},
		IsDefault: isDefault,
	}
	for _, c := range nodeexporter.Collectors {
		resp.AvailableCollectors = append(resp.AvailableCollectors, &janusv1alpha1.NodeExporterCollector{Name: c.Name, Description: c.Description, Default: c.Default})
	}
	if st, stErr := s.Extensions.State(nodeexporter.ServiceID); stErr == nil {
		resp.State = st.State
		if st.State != "running" && st.State != "disabled" {
			resp.Error = st.LastError
		}
	}
	if cfg.Enabled {
		for _, h := range nodeexporter.Hosts(cfg, s.mainAddresses()) {
			if h != "" {
				resp.EffectiveAddresses = append(resp.EffectiveAddresses, h)
			}
		}
	}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}
