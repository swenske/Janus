package nodeproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// HAProxy's runtime diagnostics: map/ACL lookups, stick tables, streams,
// captured errors. Reads are GETs - a reader may make them -, changes
// POSTs. Answers are protojson with every field (a stream's error_at of
// 0 is a position, not "unset"): 64-bit numbers come as strings.

func registerHAProxyRuntimeRoutes(mux *http.ServeMux, node *store.Node) {
	hap := func(fn func(context.Context, janusv1alpha1.HAProxyServiceClient) (proto.Message, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			unary(w, r, node, unaryTimeout, func(ctx context.Context, c *grpc.ClientConn) (any, error) {
				m, err := fn(ctx, janusv1alpha1.NewHAProxyServiceClient(c))
				if err != nil {
					return nil, err
				}
				b, err := protoOut.Marshal(m)
				return json.RawMessage(b), err
			})
		}
	}
	limit := func(r *http.Request) uint32 {
		n, _ := strconv.ParseUint(r.URL.Query().Get("limit"), 10, 32)
		return uint32(n)
	}

	// ?kind=map|acl&name=<as MapList names it>&value=
	mux.HandleFunc("GET /api/haproxy/lookup", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		req := &janusv1alpha1.PatternLookupRequest{Name: q.Get("name"), Value: q.Get("value")}
		kind := q.Get("kind")
		if kind != "map" && kind != "acl" {
			http.Error(w, "kind: map or acl", http.StatusBadRequest)
			return
		}
		hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (proto.Message, error) {
			if kind == "map" {
				return c.MapLookup(ctx, req)
			}
			return c.ACLLookup(ctx, req)
		})(w, r)
	})
	mux.HandleFunc("GET /api/haproxy/tables", hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (proto.Message, error) {
		return c.TableList(ctx, &emptypb.Empty{})
	}))
	mux.HandleFunc("GET /api/haproxy/tables/{name}", func(w http.ResponseWriter, r *http.Request) {
		filters, err := queryFilters(r.URL.Query()["filter"])
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req := &janusv1alpha1.TableShowRequest{Table: r.PathValue("name"), Key: r.URL.Query().Get("key"), Filters: filters, Limit: limit(r)}
		hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (proto.Message, error) {
			return c.TableShow(ctx, req)
		})(w, r)
	})
	mux.HandleFunc("POST /api/haproxy/tables/{name}/clear", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key     string `json:"key"`
			Filters []struct {
				Data  string `json:"data"`
				Op    string `json:"op"`
				Value int64  `json:"value"`
			} `json:"filters"`
			All bool `json:"all"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		req := &janusv1alpha1.TableClearRequest{Table: r.PathValue("name"), Key: body.Key, All: body.All}
		for _, f := range body.Filters {
			req.Filters = append(req.Filters, &janusv1alpha1.TableFilter{Data: f.Data, Op: f.Op, Value: f.Value})
		}
		hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (proto.Message, error) {
			return c.TableClear(ctx, req)
		})(w, r)
	})
	mux.HandleFunc("GET /api/haproxy/sessions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		req := &janusv1alpha1.SessionListRequest{Frontend: q.Get("frontend"), Backend: q.Get("backend"), Source: q.Get("source"), Limit: limit(r)}
		hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (proto.Message, error) {
			return c.SessionList(ctx, req)
		})(w, r)
	})
	mux.HandleFunc("POST /api/haproxy/sessions/{id}/shutdown", func(w http.ResponseWriter, r *http.Request) {
		req := &janusv1alpha1.SessionShutdownRequest{Id: r.PathValue("id")}
		hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (proto.Message, error) {
			return c.SessionShutdown(ctx, req)
		})(w, r)
	})
	mux.HandleFunc("GET /api/haproxy/errors", func(w http.ResponseWriter, r *http.Request) {
		req := &janusv1alpha1.ErrorsShowRequest{Proxy: r.URL.Query().Get("proxy"), Direction: r.URL.Query().Get("direction")}
		hap(func(ctx context.Context, c janusv1alpha1.HAProxyServiceClient) (proto.Message, error) {
			return c.ErrorsShow(ctx, req)
		})(w, r)
	})
}

// queryFilters reads ?filter=http_req_rate:gt:100 (repeated); the node
// checks the data type and the operator.
func queryFilters(in []string) ([]*janusv1alpha1.TableFilter, error) {
	var out []*janusv1alpha1.TableFilter
	for _, f := range in {
		parts := strings.Split(f, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("filter %q: data:op:value", f)
		}
		v, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("filter %q: the value is a whole number", f)
		}
		out = append(out, &janusv1alpha1.TableFilter{Data: parts[0], Op: parts[1], Value: v})
	}
	return out, nil
}
