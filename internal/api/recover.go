package api

import (
	"context"
	"log"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UnaryRecoveryInterceptor and StreamRecoveryInterceptor turn a panic in
// an RPC handler into an Internal error instead of letting it take janusd
// down (security review 2026-11-05 #15, JANUS-2026-019): a crash would
// restart janusd and, with it, keepalived/BIRD/Consul - the VIP moves and
// the BGP routes are withdrawn. They are the outermost interceptors, so a
// panic anywhere below them is caught.

func UnaryRecoveryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("api: panic in %s: %v\n%s", info.FullMethod, r, debug.Stack())
			err = status.Errorf(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}

func StreamRecoveryInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("api: panic in %s: %v\n%s", info.FullMethod, r, debug.Stack())
			err = status.Errorf(codes.Internal, "internal error")
		}
	}()
	return handler(srv, ss)
}
