package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/haproxy"
)

// HAProxy implements janusv1alpha1.HAProxyServiceServer, backed by a
// single supervised haproxy process (internal/haproxy.Manager).
// BackendList isn't implemented yet - falls through to
// UnimplementedHAProxyServiceServer.
type HAProxy struct {
	janusv1alpha1.UnimplementedHAProxyServiceServer

	Manager *haproxy.Manager
}

func (h *HAProxy) GetConfig(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.GetConfigResponse, error) {
	cfg, err := readFile(h.Manager.ConfigPath)
	if err != nil {
		return nil, err
	}
	return &janusv1alpha1.GetConfigResponse{Config: cfg, Sha256: sha256Hex(cfg)}, nil
}

func (h *HAProxy) ApplyConfig(req *janusv1alpha1.ApplyConfigRequest, stream janusv1alpha1.HAProxyService_ApplyConfigServer) error {
	if err := stream.Send(&janusv1alpha1.ApplyConfigResponse{Stage: "validating"}); err != nil {
		return err
	}

	errs, err := h.Manager.Apply(req.GetConfig())
	if err != nil {
		events.Publish("haproxy.config.rejected", map[string]any{"sha256": sha256Hex(req.GetConfig()), "errors": errs})
		return stream.Send(&janusv1alpha1.ApplyConfigResponse{
			Stage:    "rejected",
			Message:  strings.Join(errs, "; "),
			Accepted: false,
		})
	}

	events.Publish("haproxy.config.applied", map[string]string{"sha256": sha256Hex(req.GetConfig())})
	if err := stream.Send(&janusv1alpha1.ApplyConfigResponse{Stage: "reloading"}); err != nil {
		return err
	}
	return stream.Send(&janusv1alpha1.ApplyConfigResponse{Stage: "done", Accepted: true})
}

func (h *HAProxy) ValidateConfig(_ context.Context, req *janusv1alpha1.ValidateConfigRequest) (*janusv1alpha1.ValidateConfigResponse, error) {
	ok, errs := h.Manager.Validate(req.GetConfig())
	return &janusv1alpha1.ValidateConfigResponse{Valid: ok, Errors: errs}, nil
}

func (h *HAProxy) Reload(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.ReloadResponse, error) {
	if err := h.Manager.Reload(); err != nil {
		return &janusv1alpha1.ReloadResponse{Success: false, Message: err.Error()}, nil
	}
	events.Publish("haproxy.reloaded", nil)
	return &janusv1alpha1.ReloadResponse{Success: true}, nil
}

func (h *HAProxy) Stats(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.HAProxyStatsResponse, error) {
	raw, err := h.Manager.ShowStat()
	if err != nil {
		return nil, err
	}
	return &janusv1alpha1.HAProxyStatsResponse{RawCsv: raw}, nil
}

func (h *HAProxy) ShowInfo(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.ShowInfoResponse, error) {
	info, err := h.Manager.ShowInfo()
	if err != nil {
		return nil, err
	}
	return &janusv1alpha1.ShowInfoResponse{
		Version:               info.Version,
		UptimeSeconds:         info.UptimeSeconds,
		CurrentConnections:    info.CurrentConnections,
		MaxConnections:        info.MaxConnections,
		CumulativeConnections: info.CumulativeConnections,
		CumulativeRequests:    info.CumulativeRequests,
		ConnectionRate:        info.ConnectionRate,
		SessionRate:           info.SessionRate,
		IdlePercent:           info.IdlePercent,
	}, nil
}

func (h *HAProxy) ServerSetState(_ context.Context, req *janusv1alpha1.ServerSetStateRequest) (*emptypb.Empty, error) {
	state := map[janusv1alpha1.ServerSetStateRequest_State]string{
		janusv1alpha1.ServerSetStateRequest_STATE_READY: "ready",
		janusv1alpha1.ServerSetStateRequest_STATE_DRAIN: "drain",
		janusv1alpha1.ServerSetStateRequest_STATE_MAINT: "maint",
	}[req.GetState()]
	if state == "" {
		return nil, status.Errorf(codes.InvalidArgument, "state must be READY, DRAIN or MAINT, got %s", req.GetState())
	}

	if err := h.Manager.SetServerState(req.GetBackend(), req.GetServer(), state); err != nil {
		return nil, haproxyError(err)
	}
	events.Publish("haproxy.server.state", map[string]string{"backend": req.GetBackend(), "server": req.GetServer(), "state": state})
	return &emptypb.Empty{}, nil
}

func (h *HAProxy) MapList(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.MapListResponse, error) {
	maps, err := h.Manager.MapList()
	if err != nil {
		return nil, err
	}
	return &janusv1alpha1.MapListResponse{Maps: maps}, nil
}

func (h *HAProxy) MapGet(_ context.Context, req *janusv1alpha1.MapGetRequest) (*janusv1alpha1.MapGetResponse, error) {
	entries, err := h.Manager.MapGet(req.GetMap())
	if err != nil {
		return nil, haproxyError(err)
	}
	return &janusv1alpha1.MapGetResponse{Entries: entries}, nil
}

func (h *HAProxy) MapUpdate(_ context.Context, req *janusv1alpha1.MapUpdateRequest) (*emptypb.Empty, error) {
	if err := h.Manager.MapUpdate(req.GetMap(), req.GetKey(), req.GetValue(), req.GetDelete()); err != nil {
		return nil, haproxyError(err)
	}
	events.Publish("haproxy.map.updated", map[string]any{"map": req.GetMap(), "key": req.GetKey(), "delete": req.GetDelete()})
	return &emptypb.Empty{}, nil
}

func (h *HAProxy) ACLUpdate(_ context.Context, req *janusv1alpha1.ACLUpdateRequest) (*emptypb.Empty, error) {
	if err := h.Manager.ACLUpdate(req.GetAcl(), req.GetValue(), req.GetDelete()); err != nil {
		return nil, haproxyError(err)
	}
	events.Publish("haproxy.acl.updated", map[string]any{"acl": req.GetAcl(), "value": req.GetValue(), "delete": req.GetDelete()})
	return &emptypb.Empty{}, nil
}

func (h *HAProxy) CertificateList(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.CertificateListResponse, error) {
	certs, err := h.Manager.CertificateList()
	if err != nil {
		return nil, err
	}
	resp := &janusv1alpha1.CertificateListResponse{}
	for _, c := range certs {
		resp.Certificates = append(resp.Certificates, &janusv1alpha1.CertificateInfo{Name: c.Name, NotAfter: c.NotAfter, Status: c.Status})
	}
	return resp, nil
}

func (h *HAProxy) CertificateUpload(_ context.Context, req *janusv1alpha1.CertificateUploadRequest) (*emptypb.Empty, error) {
	if err := h.Manager.CertificateUpload(req.GetName(), req.GetPemBundle(), req.GetCrtList(), req.GetSni()); err != nil {
		return nil, haproxyError(err)
	}
	events.Publish("haproxy.certificate.uploaded", map[string]any{"name": req.GetName(), "crt_list": req.GetCrtList(), "sni": req.GetSni()})
	return &emptypb.Empty{}, nil
}

func (h *HAProxy) CertificateDelete(_ context.Context, req *janusv1alpha1.CertificateDeleteRequest) (*emptypb.Empty, error) {
	if err := h.Manager.CertificateDelete(req.GetName(), req.GetCrtList()); err != nil {
		return nil, haproxyError(err)
	}
	events.Publish("haproxy.certificate.deleted", map[string]string{"name": req.GetName(), "crt_list": req.GetCrtList()})
	return &emptypb.Empty{}, nil
}

// haproxyError turns an argument internal/haproxy refused before
// reaching the stats socket into codes.InvalidArgument; anything else is
// returned unchanged.
func haproxyError(err error) error {
	if errors.Is(err, haproxy.ErrInvalidArgument) {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return err
}

// BackendList reads the stats socket's "show stat": every backend, with
// each of its servers' address and state (HAProxy's own status,
// lowercased - "up", "down", "maint", "drain", "no check", ...).
func (h *HAProxy) BackendList(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.BackendListResponse, error) {
	csv, err := h.Manager.ShowStat()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "show stat: %v", err)
	}
	backends, err := parseBackends(string(csv))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "parse show stat: %v", err)
	}
	return &janusv1alpha1.BackendListResponse{Backends: backends}, nil
}

// parseBackends reads "show stat" CSV by header name rather than column
// position, since HAProxy versions have added and moved columns.
func parseBackends(csv string) ([]*janusv1alpha1.Backend, error) {
	lines := strings.Split(strings.TrimSpace(csv), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "# ") {
		return nil, fmt.Errorf("missing header line")
	}
	col := map[string]int{}
	for i, name := range strings.Split(strings.TrimPrefix(lines[0], "# "), ",") {
		col[name] = i
	}
	for _, need := range []string{"pxname", "svname", "status"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("no %q column", need)
		}
	}
	field := func(f []string, name string) string {
		if i, ok := col[name]; ok && i < len(f) {
			return f[i]
		}
		return ""
	}

	var out []*janusv1alpha1.Backend
	byName := map[string]*janusv1alpha1.Backend{}
	backend := func(name string) *janusv1alpha1.Backend {
		if b, ok := byName[name]; ok {
			return b
		}
		b := &janusv1alpha1.Backend{Name: name}
		byName[name] = b
		out = append(out, b)
		return b
	}
	for _, line := range lines[1:] {
		f := strings.Split(line, ",")
		px, sv := field(f, "pxname"), field(f, "svname")
		switch sv {
		case "FRONTEND", "":
		case "BACKEND":
			backend(px)
		default:
			b := backend(px)
			b.Servers = append(b.Servers, &janusv1alpha1.BackendServer{
				Name:    sv,
				Address: field(f, "addr"),
				State:   strings.ToLower(field(f, "status")),
			})
		}
	}
	return out, nil
}
