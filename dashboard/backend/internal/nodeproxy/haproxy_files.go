package nodeproxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"path"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// HAProxy's own files (/etc/haproxy/files): listed, read (never a private
// key - the node refuses), written and removed - both refused by the node
// when haproxy.cfg wouldn't load with the change. Content travels as
// base64 in JSON; reading gives the raw bytes.

func registerHAProxyFileRoutes(mux *http.ServeMux, node *store.Node) {
	hap := func(fn func(context.Context, janusv1alpha1.HAProxyServiceClient) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
				return fn(ctx, janusv1alpha1.NewHAProxyServiceClient(c))
			})
		}
	}
	mux.HandleFunc("GET /api/haproxy/files", hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
		resp, err := c.FileList(ctx, &emptypb.Empty{})
		if err != nil {
			return nil, err
		}
		type file struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			Size     uint64 `json:"size"`
			SHA256   string `json:"sha256"`
			Modified int64  `json:"modified_unix"`
			Secret   bool   `json:"secret"`
		}
		files := []file{}
		for _, f := range resp.GetFiles() {
			files = append(files, file{f.GetName(), f.GetPath(), f.GetSize(), f.GetSha256(), f.GetModifiedUnix(), f.GetSecret()})
		}
		return map[string]any{"dir": resp.GetDir(), "files": files}, nil
	}))
	mux.HandleFunc("GET /api/haproxy/files/content", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		conn, err := dialNode(node)
		if err != nil {
			http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), unaryTimeout)
		defer cancel()
		resp, err := janusv1alpha1.NewHAProxyServiceClient(conn).FileGet(ctx, &janusv1alpha1.FileGetRequest{Name: name})
		if err != nil {
			http.Error(w, status.Convert(err).Message(), grpcHTTPStatus(err))
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(name)))
		_, _ = w.Write(resp.GetContent())
	})
	mux.HandleFunc("POST /api/haproxy/files", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name    string `json:"name"`
			Content string `json:"content_base64"`
			Reload  bool   `json:"reload"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		content, err := base64.StdEncoding.DecodeString(req.Content)
		if err != nil {
			http.Error(w, "content_base64: "+err.Error(), http.StatusBadRequest)
			return
		}
		hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
			resp, err := c.FilePut(ctx, &janusv1alpha1.FilePutRequest{Name: req.Name, Content: content, Reload: req.Reload})
			if err != nil {
				return nil, err
			}
			return moduleConfigResult{Accepted: resp.GetAccepted(), Errors: resp.GetErrors()}, nil
		})(w, r)
	})
	mux.HandleFunc("POST /api/haproxy/files/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name   string `json:"name"`
			Reload bool   `json:"reload"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (any, error) {
			resp, err := c.FileDelete(ctx, &janusv1alpha1.FileDeleteRequest{Name: req.Name, Reload: req.Reload})
			if err != nil {
				return nil, err
			}
			return moduleConfigResult{Accepted: resp.GetAccepted(), Errors: resp.GetErrors()}, nil
		})(w, r)
	})
}
