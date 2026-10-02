package api

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme"
)

func (h *HAProxy) acme() (*acme.Manager, error) {
	if h.ACME == nil || !h.ACME.Available() {
		return nil, status.Error(codes.FailedPrecondition, "the letsencrypt extension isn't in this node's image - optional modules are chosen when the image is built")
	}
	return h.ACME, nil
}

func (h *HAProxy) ACMEStatus(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.ACMEStatusResponse, error) {
	if h.ACME == nil {
		return &janusv1alpha1.ACMEStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED}, nil
	}
	return h.ACME.Status(), nil
}

func (h *HAProxy) ACMEGetConfig(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.ACMEGetConfigResponse, error) {
	m, err := h.acme()
	if err != nil {
		return nil, err
	}
	cfg := m.Config()
	if cfg == nil {
		return &janusv1alpha1.ACMEGetConfigResponse{Config: &janusv1alpha1.ACMEConfig{}, IsDefault: true}, nil
	}
	return &janusv1alpha1.ACMEGetConfigResponse{Config: cfg}, nil
}

func (h *HAProxy) ACMEApplyConfig(_ context.Context, req *janusv1alpha1.ACMEApplyConfigRequest) (*janusv1alpha1.ACMEApplyConfigResponse, error) {
	m, err := h.acme()
	if err != nil {
		return nil, err
	}
	cfg := req.GetConfig()
	if cfg == nil {
		cfg = &janusv1alpha1.ACMEConfig{}
	}
	errs, err := m.Apply(cfg, req.GetAccountKey(), req.GetValidateOnly())
	if err != nil {
		if len(errs) > 0 {
			return &janusv1alpha1.ACMEApplyConfigResponse{Errors: errs}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &janusv1alpha1.ACMEApplyConfigResponse{Accepted: true}, nil
}

func (h *HAProxy) ACMERenew(_ context.Context, req *janusv1alpha1.ACMERenewRequest) (*janusv1alpha1.ACMERenewResponse, error) {
	m, err := h.acme()
	if err != nil {
		return nil, err
	}
	names, err := m.Renew(req.GetNames())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &janusv1alpha1.ACMERenewResponse{Names: names}, nil
}
