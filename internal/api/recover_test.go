package api

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRecoveryInterceptors: a panicking handler becomes an Internal error
// instead of crashing janusd (security review 2026-11-05 #15).
func TestRecoveryInterceptors(t *testing.T) {
	_, err := UnaryRecoveryInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x"},
		func(context.Context, any) (any, error) { panic("boom") })
	if status.Code(err) != codes.Internal {
		t.Errorf("unary: got %v, want Internal", err)
	}
	err = StreamRecoveryInterceptor(nil, nil, &grpc.StreamServerInfo{FullMethod: "/x"},
		func(any, grpc.ServerStream) error { panic("boom") })
	if status.Code(err) != codes.Internal {
		t.Errorf("stream: got %v, want Internal", err)
	}
	// No panic: the handler's result passes through.
	if _, err := UnaryRecoveryInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x"},
		func(context.Context, any) (any, error) { return 1, nil }); err != nil {
		t.Errorf("no panic: %v", err)
	}
}
