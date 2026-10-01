package api

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// TestConnOpened checks ConnStats over a real gRPC server: every RPC of
// one connection sees when that connection opened, and a connection
// opened later sees a later time - what FirewallConfirm relies on.
func TestConnOpened(t *testing.T) {
	var mu sync.Mutex
	var seen []time.Time
	srv := grpc.NewServer(grpc.StatsHandler(ConnStats{}), grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		mu.Lock()
		seen = append(seen, ConnOpened(ctx))
		mu.Unlock()
		return h(ctx, req)
	}))
	janusv1alpha1.RegisterSystemServiceServer(srv, &System{BuildVersion: "test"})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	call := func(conn *grpc.ClientConn) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := janusv1alpha1.NewSystemServiceClient(conn).Version(ctx, &emptypb.Empty{}); err != nil {
			t.Fatal(err)
		}
	}
	dial := func() *grpc.ClientConn {
		t.Helper()
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}

	first := dial()
	defer first.Close()
	call(first)
	time.Sleep(20 * time.Millisecond)
	call(first)
	second := dial()
	defer second.Close()
	call(second)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0].IsZero() {
		t.Fatalf("connection times %v", seen)
	}
	if !seen[1].Equal(seen[0]) {
		t.Fatalf("two calls on one connection: %v then %v", seen[0], seen[1])
	}
	if !seen[2].After(seen[1]) {
		t.Fatalf("a newer connection isn't newer: %v then %v", seen[1], seen[2])
	}
}
