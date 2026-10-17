package api

import (
	"context"
	"net/netip"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/logship"
	"github.com/swenske/Janus/internal/netmgr"
)

// egressInterface says which interface a target's traffic leaves by
// (tests replace it).
var egressInterface = netmgr.EgressInterface

// LogForwardingGet returns the log shipper's configuration, without its
// secrets, and each target's state.
func (s *System) LogForwardingGet(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.LogForwardingResponse, error) {
	if s.LogShip == nil {
		return nil, status.Error(codes.Unavailable, "no log shipper on this node")
	}
	return s.logForwardingResponse(), nil
}

func (s *System) logForwardingResponse() *janusv1alpha1.LogForwardingResponse {
	cfg := s.LogShip.Config()
	resp := &janusv1alpha1.LogForwardingResponse{Config: logship.ToProto(cfg.Redacted())}
	secrets := map[string][]string{}
	for _, t := range cfg.Targets {
		for name := range t.HasSecret() {
			secrets[t.Name] = append(secrets[t.Name], name)
		}
		sort.Strings(secrets[t.Name])
	}
	for _, st := range s.LogShip.Status() {
		resp.Targets = append(resp.Targets, &janusv1alpha1.LogTargetStatus{
			Name: st.Name, Kind: st.Kind, State: st.State, LastError: st.LastError,
			LastSentUnixNs: unixNano(st.LastSent), FailingSinceUnixNs: unixNano(st.FailingSince),
			Sent: st.Sent, Dropped: st.Dropped, Errors: st.Errors, Queued: uint64(st.QueuedRecords), //nolint:gosec // G115: a count, never negative
			Secrets: secrets[st.Name], BytesSent: st.BytesSent, LocalAddress: st.Local, RemoteAddress: st.Remote,
			Interface: targetInterface(st.Local, st.Remote),
		})
	}
	return resp
}

// targetInterface is the interface the kernel routes a target's
// connection by now - "" when it never connected, or no route says.
func targetInterface(local, remote string) string {
	dst, err := netip.ParseAddrPort(remote)
	if err != nil {
		return ""
	}
	src, _ := netip.ParseAddrPort(local)
	name, err := egressInterface(dst.Addr(), src.Addr())
	if err != nil {
		return ""
	}
	return name
}

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// LogForwardingSet replaces the log shipper's configuration, keeping the
// secrets a target sent without them already had.
func (s *System) LogForwardingSet(ctx context.Context, req *janusv1alpha1.LogForwardingConfig) (*janusv1alpha1.LogForwardingResponse, error) {
	if s.LogShip == nil {
		return nil, status.Error(codes.Unavailable, "no log shipper on this node")
	}
	cfg := logship.FromProto(req)
	saved, err := logship.LoadConfig(s.LogShipDir)
	if err != nil {
		saved = s.LogShip.Config()
	}
	cfg.MergeSecrets(saved)
	if err := cfg.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := logship.SaveConfig(s.LogShipDir, cfg); err != nil {
		return nil, status.Errorf(codes.Internal, "save: %v", err)
	}
	s.LogShip.Apply(cfg)
	names := make([]string, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		names = append(names, t.Name)
	}
	caller, _ := CallerFrom(ctx)
	events.Publish("logship.configured", map[string]any{"targets": names, "managed_by": cfg.ManagedBy, "by": caller.String()})
	return s.logForwardingResponse(), nil
}

// LogForwardingTest sends one record to the target given.
func (s *System) LogForwardingTest(ctx context.Context, req *janusv1alpha1.LogTarget) (*janusv1alpha1.LogForwardingTestResponse, error) {
	if s.LogShip == nil {
		return nil, status.Error(codes.Unavailable, "no log shipper on this node")
	}
	one := logship.Config{Targets: []logship.Target{logship.TargetFromProto(req)}}
	saved, err := logship.LoadConfig(s.LogShipDir)
	if err != nil {
		saved = s.LogShip.Config()
	}
	one.MergeSecrets(saved)
	if err := one.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.LogShip.Test(ctx, one.Targets[0]); err != nil {
		return &janusv1alpha1.LogForwardingTestResponse{Detail: err.Error()}, nil
	}
	return &janusv1alpha1.LogForwardingTestResponse{Ok: true, Detail: "sent"}, nil
}
