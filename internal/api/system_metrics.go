package api

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/exporter"
)

func (s *System) MetricsConfigGet(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.MetricsConfigResponse, error) {
	if s.Exporter == nil {
		return nil, status.Error(codes.Unavailable, "the exporter isn't running in this janusd")
	}
	return s.metricsConfigResponse(), nil
}

func (s *System) MetricsConfigSet(_ context.Context, req *janusv1alpha1.MetricsConfig) (*janusv1alpha1.MetricsConfigResponse, error) {
	if s.Exporter == nil {
		return nil, status.Error(codes.Unavailable, "the exporter isn't running in this janusd")
	}
	cfg := exporter.Config{Enabled: req.GetEnabled(), Port: req.GetPort(), Address: req.GetAddress()}
	if err := cfg.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.Exporter.Apply(cfg); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err := exporter.Save(cfg); err != nil {
		return nil, status.Errorf(codes.Internal, "the exporter switched, but its configuration couldn't be saved - it reverts at the next restart: %v", err)
	}
	return s.metricsConfigResponse(), nil
}

func (s *System) metricsConfigResponse() *janusv1alpha1.MetricsConfigResponse {
	cfg, listening, lastErr := s.Exporter.Status()
	_, isDefault, err := exporter.Load()
	if err != nil && lastErr == "" {
		lastErr = err.Error()
	}
	return &janusv1alpha1.MetricsConfigResponse{
		Config:    &janusv1alpha1.MetricsConfig{Enabled: cfg.Enabled, Port: cfg.Port, Address: cfg.Address},
		IsDefault: isDefault,
		Listening: listening,
		Error:     lastErr,
	}
}
