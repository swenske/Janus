package api

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/haproxy"
)

// HAProxy's own files (internal/haproxy/files.go).

func fileError(err error) error {
	switch {
	case errors.Is(err, haproxy.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, haproxy.ErrNoFile):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, haproxy.ErrSecretFile):
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

func (h *HAProxy) FileList(_ context.Context, _ *emptypb.Empty) (*janusv1alpha1.FileListResponse, error) {
	files, err := h.Manager.Files()
	if err != nil {
		return nil, fileError(err)
	}
	resp := &janusv1alpha1.FileListResponse{Dir: h.Manager.FilesDir}
	for _, f := range files {
		resp.Files = append(resp.Files, &janusv1alpha1.HAProxyFile{Name: f.Name, Path: f.Path, Size: uint64(f.Size),
			Sha256: f.SHA256, ModifiedUnix: f.Modified.Unix(), Secret: f.Secret})
	}
	return resp, nil
}

func (h *HAProxy) FileGet(_ context.Context, req *janusv1alpha1.FileGetRequest) (*janusv1alpha1.FileGetResponse, error) {
	data, err := h.Manager.FileRead(req.GetName())
	if err != nil {
		return nil, fileError(err)
	}
	return &janusv1alpha1.FileGetResponse{Content: data}, nil
}

func (h *HAProxy) FilePut(_ context.Context, req *janusv1alpha1.FilePutRequest) (*janusv1alpha1.FilePutResponse, error) {
	errs, err := h.Manager.FilePut(req.GetName(), req.GetContent())
	if err != nil {
		if len(errs) > 0 {
			return &janusv1alpha1.FilePutResponse{Errors: errs}, nil
		}
		return nil, fileError(err)
	}
	events.Publish("haproxy.file.put", map[string]any{"name": req.GetName(), "size": len(req.GetContent())})
	if req.GetReload() {
		if err := h.Manager.Reload(); err != nil {
			return nil, status.Errorf(codes.Internal, "the file is saved, but HAProxy didn't reload: %v", err)
		}
	}
	return &janusv1alpha1.FilePutResponse{Accepted: true}, nil
}

func (h *HAProxy) FileDelete(_ context.Context, req *janusv1alpha1.FileDeleteRequest) (*janusv1alpha1.FileDeleteResponse, error) {
	errs, err := h.Manager.FileDelete(req.GetName())
	if err != nil {
		if len(errs) > 0 {
			return &janusv1alpha1.FileDeleteResponse{Errors: errs}, nil
		}
		return nil, fileError(err)
	}
	events.Publish("haproxy.file.deleted", map[string]any{"name": req.GetName()})
	if req.GetReload() {
		if err := h.Manager.Reload(); err != nil {
			return nil, status.Errorf(codes.Internal, "the file is removed, but HAProxy didn't reload: %v", err)
		}
	}
	return &janusv1alpha1.FileDeleteResponse{Accepted: true}, nil
}
