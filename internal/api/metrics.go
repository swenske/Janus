package api

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// APIRequests counts the API's calls by method and status code, for the
// node's exporter - refused ones included, so it's installed ahead of
// the auth interceptors.
var APIRequests = &RequestCounts{}

// RequestCounts is a set of per-(method, code) counters.
type RequestCounts struct {
	m sync.Map // "method code" -> *atomic.Uint64
}

// RequestCount is one counter.
type RequestCount struct {
	Method string // "SystemService/Version"
	Code   string // "OK", "PermissionDenied"...
	N      uint64
}

func (c *RequestCounts) add(fullMethod string, code codes.Code) {
	method := strings.TrimPrefix(fullMethod, "/janus.v1alpha1.")
	key := method + " " + code.String()
	v, ok := c.m.Load(key)
	if !ok {
		v, _ = c.m.LoadOrStore(key, new(atomic.Uint64))
	}
	v.(*atomic.Uint64).Add(1)
}

// Snapshot returns every counter, sorted.
func (c *RequestCounts) Snapshot() []RequestCount {
	var out []RequestCount
	c.m.Range(func(k, v any) bool {
		method, code, _ := strings.Cut(k.(string), " ")
		out = append(out, RequestCount{Method: method, Code: code, N: v.(*atomic.Uint64).Load()})
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// UnaryMetricsInterceptor counts unary calls.
func UnaryMetricsInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	APIRequests.add(info.FullMethod, status.Code(err))
	return resp, err
}

// StreamMetricsInterceptor counts streaming calls, once each, when they end.
func StreamMetricsInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	err := handler(srv, ss)
	APIRequests.add(info.FullMethod, status.Code(err))
	return err
}
