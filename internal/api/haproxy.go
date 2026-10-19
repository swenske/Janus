package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/haproxy"
	"github.com/swenske/Janus/internal/haproxystat"
)

// HAProxy implements janusv1alpha1.HAProxyServiceServer, backed by a
// single supervised haproxy process (internal/haproxy.Manager).
// BackendList isn't implemented yet - falls through to
// UnimplementedHAProxyServiceServer.
type HAProxy struct {
	janusv1alpha1.UnimplementedHAProxyServiceServer

	Manager *haproxy.Manager
	// ACME manages the letsencrypt extension (acme.go).
	ACME *acme.Manager
	// History keeps every configuration ApplyConfig accepted
	// (ConfigHistory); nil: none kept.
	History *haproxy.History
}

// GetConfig gives the configuration whole only to who may apply one: a
// reader gets it with its secrets masked (JANUS-2026-006), and the hash
// of what it got - the real file's would let it try passwords offline.
func (h *HAProxy) GetConfig(ctx context.Context, _ *emptypb.Empty) (*janusv1alpha1.GetConfigResponse, error) {
	cfg, err := readFile(h.Manager.ConfigPath)
	if err != nil {
		return nil, err
	}
	redacted := false
	if checkRole(ctx, janusv1alpha1.HAProxyService_ApplyConfig_FullMethodName) != nil {
		masked := haproxy.RedactConfig(cfg)
		redacted = !bytes.Equal(masked, cfg)
		cfg = masked
	}
	return &janusv1alpha1.GetConfigResponse{Config: cfg, Sha256: sha256Hex(cfg), Redacted: redacted}, nil
}

// readableSHA256 names a configuration in events - which readers read,
// and log targets receive - by the hash of what a reader's GetConfig
// shows: the same as the file's when it holds no secret.
func readableSHA256(cfg []byte) string {
	return sha256Hex(haproxy.RedactConfig(cfg))
}

func (h *HAProxy) ApplyConfig(req *janusv1alpha1.ApplyConfigRequest, stream janusv1alpha1.HAProxyService_ApplyConfigServer) error {
	cfg, from := req.GetConfig(), req.GetVersion()
	if from != 0 {
		if len(cfg) > 0 {
			return status.Error(codes.InvalidArgument, "a configuration or a version, not both")
		}
		if h.History == nil {
			return status.Error(codes.FailedPrecondition, "this node keeps no configuration history")
		}
		_, stored, err := h.History.Get(from)
		if errors.Is(err, haproxy.ErrNoVersion) {
			return status.Error(codes.NotFound, err.Error())
		}
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		cfg = stored
	}
	if err := stream.Send(&janusv1alpha1.ApplyConfigResponse{Stage: "validating"}); err != nil {
		return err
	}

	// What it replaces: the history begins with it when it's empty.
	prev, _ := readFile(h.Manager.ConfigPath)
	errs, err := h.Manager.Apply(cfg)
	if err != nil {
		if len(errs) == 0 {
			errs = []string{err.Error()}
		}
		events.Publish("haproxy.config.rejected", map[string]any{"sha256": readableSHA256(cfg), "errors": errs})
		return stream.Send(&janusv1alpha1.ApplyConfigResponse{
			Stage:    "rejected",
			Message:  strings.Join(errs, "; "),
			Accepted: false,
		})
	}

	applied := map[string]any{"sha256": readableSHA256(cfg)}
	if h.History != nil {
		caller, _ := CallerFrom(stream.Context())
		v, err := h.History.Record(prev, cfg, haproxy.Author{Name: caller.Name, Roles: caller.Roles, Via: caller.Via}, from)
		if err != nil {
			// Applied all the same: the history is a record, not a gate.
			log.Printf("haproxy: the configuration was applied but not kept in the history: %v", err)
		} else {
			applied["version"] = v.ID
		}
		if from != 0 {
			applied["reverted_from"] = from
		}
	}
	events.Publish("haproxy.config.applied", applied)
	if err := stream.Send(&janusv1alpha1.ApplyConfigResponse{Stage: "reloading"}); err != nil {
		return err
	}
	if h.ACME != nil {
		h.ACME.HAProxyConfigChanged()
	}
	return stream.Send(&janusv1alpha1.ApplyConfigResponse{Stage: "done", Accepted: true})
}

// ConfigHistory lists the history's versions. Whoever may not apply a
// configuration gets each version's figures from its masked text - its
// SHA-256, its size, the lines it changed - as GetConfig shows it.
func (h *HAProxy) ConfigHistory(ctx context.Context, req *janusv1alpha1.ConfigHistoryRequest) (*janusv1alpha1.ConfigHistoryResponse, error) {
	if h.History == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node keeps no configuration history")
	}
	versions, err := h.History.List()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	mask := checkRole(ctx, janusv1alpha1.HAProxyService_ApplyConfig_FullMethodName) != nil
	running, _ := readFile(h.Manager.ConfigPath)
	runningSHA := sha256Hex(running)
	texts := make([][]byte, len(versions))
	for i, v := range versions {
		if _, cfg, err := h.History.Get(v.ID); err == nil {
			texts[i] = cfg
		}
	}
	out := &janusv1alpha1.ConfigHistoryResponse{Redacted: mask, MaxVersions: uint32(h.History.MaxVersionsOrDefault()), MaxBytes: uint64(h.History.MaxBytesOrDefault())} //nolint:gosec // G115: small positive bounds
	for i, v := range versions {
		info := configVersionInfo(v, texts[i], mask)
		info.Running = v.SHA256 == runningSHA
		// Newest first: the version before this one is the next.
		var before []byte
		if i+1 < len(versions) {
			before = texts[i+1]
			if mask {
				before = haproxy.RedactConfig(before)
			}
		}
		text := texts[i]
		if mask {
			text = haproxy.RedactConfig(text)
		}
		added, removed := haproxy.LineChanges(before, text)
		info.LinesAdded, info.LinesRemoved = uint32(added), uint32(removed) //nolint:gosec // G115: line counts
		out.Versions = append(out.Versions, info)
		if req.GetLimit() > 0 && len(out.Versions) == int(req.GetLimit()) {
			break
		}
	}
	return out, nil
}

// ConfigVersion returns one version, masked like GetConfig.
func (h *HAProxy) ConfigVersion(ctx context.Context, req *janusv1alpha1.ConfigVersionRequest) (*janusv1alpha1.ConfigVersionResponse, error) {
	if h.History == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node keeps no configuration history")
	}
	v, cfg, err := h.History.Get(req.GetId())
	if errors.Is(err, haproxy.ErrNoVersion) {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	mask := checkRole(ctx, janusv1alpha1.HAProxyService_ApplyConfig_FullMethodName) != nil
	info := configVersionInfo(v, cfg, mask)
	running, _ := readFile(h.Manager.ConfigPath)
	info.Running = v.SHA256 == sha256Hex(running)
	redacted := false
	if mask {
		masked := haproxy.RedactConfig(cfg)
		redacted = !bytes.Equal(masked, cfg)
		cfg = masked
	}
	return &janusv1alpha1.ConfigVersionResponse{Version: info, Config: cfg, Redacted: redacted}, nil
}

// configVersionInfo is v as the API gives it; with mask, its SHA-256 and
// size are its masked text's (the real hash would let a reader try
// passwords offline).
func configVersionInfo(v haproxy.Version, cfg []byte, mask bool) *janusv1alpha1.ConfigVersionInfo {
	info := &janusv1alpha1.ConfigVersionInfo{
		Id: v.ID, Author: &janusv1alpha1.ConfigAuthor{Name: v.Author.Name, Roles: v.Author.Roles, Via: v.Author.Via},
		Sha256: v.SHA256, Size: uint64(v.Size), //nolint:gosec // G115: a file's size
		RevertedFrom: v.RevertedFrom, BeforeHistory: v.BeforeHistory,
	}
	if !v.Time.IsZero() {
		info.AppliedUnix = v.Time.Unix()
	}
	if mask && cfg != nil {
		masked := haproxy.RedactConfig(cfg)
		info.Sha256, info.Size = sha256Hex(masked), uint64(len(masked))
	}
	return info
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
		DroppedLogs:           info.DroppedLogs,
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
// reaching the stats socket into codes.InvalidArgument, and what HAProxy
// says it doesn't have into codes.NotFound; anything else is returned
// unchanged.
func haproxyError(err error) error {
	switch {
	case errors.Is(err, haproxy.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, haproxy.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
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
		// By the type column: under `option socket-stats` a frontend's
		// listeners have rows of their own, named after their bind.
		switch haproxystat.KindOf(field(f, "type"), sv) {
		case haproxystat.KindBackend:
			backend(px)
		case haproxystat.KindServer:
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
