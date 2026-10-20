package api

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/consolelimit"
	"github.com/swenske/Janus/internal/events"
)

// ConsoleBudgetGet reports the services' share of the console.
func (s *System) ConsoleBudgetGet(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.ConsoleBudgetResponse, error) {
	if s.Console == nil {
		return nil, status.Error(codes.Unavailable, "this janusd doesn't manage the node's console")
	}
	return s.consoleBudgetResponse(), nil
}

// ConsoleBudgetSet changes it, at once and for good - or back to the
// default.
func (s *System) ConsoleBudgetSet(ctx context.Context, req *janusv1alpha1.ConsoleBudget) (*janusv1alpha1.ConsoleBudgetResponse, error) {
	if s.Console == nil {
		return nil, status.Error(codes.Unavailable, "this janusd doesn't manage the node's console")
	}
	cfg := consolelimit.DefaultConfig()
	if !req.GetUseDefault() {
		cfg = consolelimit.Config{RateBytesPerSecond: int(req.GetRateBytesPerSecond()), BurstBytes: int(req.GetBurstBytes())}
		if err := cfg.Validate(); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	var err error
	if req.GetUseDefault() {
		err = consolelimit.Reset()
	} else {
		err = consolelimit.Save(cfg)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "the console budget couldn't be saved: %v", err)
	}
	s.Console.Set(cfg.RateBytesPerSecond, cfg.BurstBytes)
	events.Publish("console.budget_changed", map[string]any{"rate_bytes_per_second": cfg.RateBytesPerSecond, "burst_bytes": cfg.BurstBytes, "default": req.GetUseDefault(), "by": callerName(ctx)})
	return s.consoleBudgetResponse(), nil
}

func (s *System) consoleBudgetResponse() *janusv1alpha1.ConsoleBudgetResponse {
	rate, burst := s.Console.Settings()
	_, isDefault, _ := consolelimit.Load()
	def := consolelimit.DefaultConfig()
	var leftOut uint64
	for _, n := range s.Console.LeftOut() {
		leftOut += n
	}
	return &janusv1alpha1.ConsoleBudgetResponse{
		Config:            &janusv1alpha1.ConsoleBudget{RateBytesPerSecond: uint32(rate), BurstBytes: uint32(burst)},
		IsDefault:         isDefault,
		Default:           &janusv1alpha1.ConsoleBudget{RateBytesPerSecond: uint32(def.RateBytesPerSecond), BurstBytes: uint32(def.BurstBytes)},
		LinesLeftOutTotal: leftOut,
	}
}

// callerName names who called, for an event.
func callerName(ctx context.Context) string {
	c, _ := CallerFrom(ctx)
	if c.Via != "" {
		return c.Name + " via " + c.Via
	}
	return c.Name
}
