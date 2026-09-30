package main

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/api"
)

// freezingProxy forwards TCP until freeze(), then keeps both connections
// open but silently drops every byte - what a client that vanished
// without closing its connection looks like from the server.
type freezingProxy struct {
	ln     net.Listener
	frozen atomic.Bool
	mu     sync.Mutex
	conns  []net.Conn
}

func newFreezingProxy(t *testing.T, target string) *freezingProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezingProxy{ln: ln}
	t.Cleanup(func() {
		ln.Close()
		p.mu.Lock()
		for _, c := range p.conns {
			c.Close()
		}
		p.mu.Unlock()
	})
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				client.Close()
				return
			}
			p.mu.Lock()
			p.conns = append(p.conns, client, server)
			p.mu.Unlock()
			go p.pipe(server, client)
			go p.pipe(client, server)
		}
	}()
	return p
}

func (p *freezingProxy) pipe(dst io.Writer, src io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if !p.frozen.Load() {
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
	}
}

// followedStreamEnds opens a followed Events stream through a proxy,
// freezes the proxy, and reports whether the server-side handler returned
// within wait.
func followedStreamEnds(t *testing.T, opts []grpc.ServerOption, wait time.Duration) bool {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, ended := make(chan struct{}), make(chan struct{})
	srv := grpc.NewServer(append(opts, grpc.StreamInterceptor(
		func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			close(started)
			err := h(srv, ss)
			close(ended)
			return err
		}))...)
	janusv1alpha1.RegisterSystemServiceServer(srv, &api.System{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	proxy := newFreezingProxy(t, lis.Addr().String())
	conn, err := grpc.NewClient(proxy.ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := janusv1alpha1.NewSystemServiceClient(conn).Events(context.Background(), &janusv1alpha1.EventsRequest{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never reached the server")
	}

	proxy.frozen.Store(true)
	select {
	case <-ended:
		return true
	case <-time.After(wait):
		return false
	}
}

// TestKeepaliveEndsStreamsOfVanishedClients: with janusd's connection
// options (shortened timings), a followed stream whose client went
// silent is cancelled; without them, it stays open.
func TestKeepaliveEndsStreamsOfVanishedClients(t *testing.T) {
	if testing.Short() {
		t.Skip("waits several seconds")
	}
	// gRPC's own floor for the ping interval is 1s.
	if !followedStreamEnds(t, connectionOptions(time.Second, time.Second), 10*time.Second) {
		t.Fatal("the stream of a vanished client was still open 10s later")
	}
	if followedStreamEnds(t, nil, 4*time.Second) {
		t.Fatal("control: without keepalive the stream should have stayed open - the test proves nothing")
	}
}
