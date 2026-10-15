package api

import (
	"context"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/logship"
	"github.com/swenske/Janus/internal/tlstrust"
)

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
	resp := &janusv1alpha1.LogForwardingResponse{Config: logConfigToProto(cfg.Redacted())}
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
			Secrets: secrets[st.Name],
		})
	}
	return resp
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
	cfg := logConfigFromProto(req)
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
	one := logship.Config{Targets: []logship.Target{logTargetFromProto(req)}}
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

func logConfigFromProto(p *janusv1alpha1.LogForwardingConfig) logship.Config {
	c := logship.Config{Labels: p.GetLabels(), AccessLogsOnDisk: p.GetAccessLogsOnDisk(), ManagedBy: p.GetManagedBy()}
	for _, t := range p.GetTargets() {
		c.Targets = append(c.Targets, logTargetFromProto(t))
	}
	return c
}

func logTargetFromProto(t *janusv1alpha1.LogTarget) logship.Target {
	out := logship.Target{Name: t.GetName(), Sources: t.GetSources(), Disabled: t.GetDisabled()}
	if sl := t.GetSyslog(); sl != nil {
		out.Syslog = &logship.SyslogTarget{Address: sl.GetAddress(), Transport: sl.GetTransport(), Format: sl.GetFormat(), Framing: sl.GetFraming(), Facility: sl.GetFacility()}
	}
	if es := t.GetElasticsearch(); es != nil {
		out.Elasticsearch = &logship.ElasticsearchTarget{URL: es.GetUrl(), Namespace: es.GetNamespace(), APIKey: es.GetApiKey(), Username: es.GetUsername(), Password: es.GetPassword(), Gzip: es.GetGzip()}
	}
	if tr := t.GetTls(); tr != nil {
		out.TLS = tlstrust.Trust{CAPEM: tr.GetCaPem(), Fingerprint: tr.GetFingerprint(), ServerName: tr.GetServerName(), ClientCertPEM: tr.GetClientCertPem(), ClientKeyPEM: tr.GetClientKeyPem()}
	}
	return out
}

func logConfigToProto(c logship.Config) *janusv1alpha1.LogForwardingConfig {
	p := &janusv1alpha1.LogForwardingConfig{Labels: c.Labels, AccessLogsOnDisk: c.AccessLogsOnDisk, ManagedBy: c.ManagedBy}
	for _, t := range c.Targets {
		pt := &janusv1alpha1.LogTarget{Name: t.Name, Sources: t.Sources, Disabled: t.Disabled}
		if sl := t.Syslog; sl != nil {
			pt.Syslog = &janusv1alpha1.SyslogTarget{Address: sl.Address, Transport: sl.Transport, Format: sl.Format, Framing: sl.Framing, Facility: sl.Facility}
		}
		if es := t.Elasticsearch; es != nil {
			pt.Elasticsearch = &janusv1alpha1.ElasticsearchTarget{Url: es.URL, Namespace: es.Namespace, ApiKey: es.APIKey, Username: es.Username, Password: es.Password, Gzip: es.Gzip}
		}
		if t.TLS != (tlstrust.Trust{}) {
			pt.Tls = &janusv1alpha1.TLSTrust{CaPem: t.TLS.CAPEM, Fingerprint: t.TLS.Fingerprint, ServerName: t.TLS.ServerName, ClientCertPem: t.TLS.ClientCertPEM, ClientKeyPem: t.TLS.ClientKeyPEM}
		}
		p.Targets = append(p.Targets, pt)
	}
	return p
}
