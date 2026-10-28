package api

import (
	"context"
	"net/netip"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/birdexporter"
)

// bird_exporter's settings (the prometheus-bird-exporter extension,
// docs/bgp.md "bird_exporter"), on node_exporter's pattern.

func (n *Network) BGPExporterConfigGet(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.BGPExporterConfigResponse, error) {
	if err := n.birdExporterPresent(); err != nil {
		return nil, err
	}
	return n.birdExporterResponse(), nil
}

func (n *Network) BGPExporterConfigSet(_ context.Context, req *janusv1alpha1.BGPExporterConfig) (*janusv1alpha1.BGPExporterConfigResponse, error) {
	if err := n.birdExporterPresent(); err != nil {
		return nil, err
	}
	cfg := birdexporter.Config{Enabled: req.GetEnabled(), Address: req.GetAddress(), Port: req.GetPort()}
	if err := cfg.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := birdexporter.Apply(n.Exporters, cfg, n.mainAddresses()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return n.birdExporterResponse(), nil
}

func (n *Network) mainAddresses() []netip.Addr {
	if n.Main == nil {
		return nil
	}
	return n.Main()
}

func (n *Network) birdExporterPresent() error {
	if n.Exporters == nil || !n.Exporters.Has(birdexporter.ServiceID) {
		return status.Error(codes.FailedPrecondition, "the prometheus-bird-exporter extension isn't in this node's image")
	}
	return nil
}

func (n *Network) birdExporterResponse() *janusv1alpha1.BGPExporterConfigResponse {
	cfg, isDefault, err := birdexporter.Load()
	resp := &janusv1alpha1.BGPExporterConfigResponse{
		Config:    &janusv1alpha1.BGPExporterConfig{Enabled: cfg.Enabled, Address: cfg.Address, Port: cfg.Port},
		IsDefault: isDefault,
	}
	if st, stErr := n.Exporters.State(birdexporter.ServiceID); stErr == nil {
		resp.State = st.State
		if st.State != "running" && st.State != "disabled" {
			resp.Error = st.LastError
		}
	}
	if cfg.Enabled {
		resp.EffectiveAddress = birdexporter.Host(cfg, n.mainAddresses())
	}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}
