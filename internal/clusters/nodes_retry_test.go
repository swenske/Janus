package clusters

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// fakeMember serves what a member's ClusterService answers to a drain
// and an undrain, counting them.
type fakeMember struct {
	janusv1alpha1.UnimplementedClusterServiceServer
	mu               sync.Mutex
	drains, undrains int
}

func (f *fakeMember) ClusterUndrain(context.Context, *emptypb.Empty) (*janusv1alpha1.ClusterInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.undrains++
	return &janusv1alpha1.ClusterInfo{}, nil
}

func (f *fakeMember) ClusterDrain(_ *janusv1alpha1.ClusterDrainRequest, s grpc.ServerStreamingServer[janusv1alpha1.ClusterDrainProgress]) error {
	f.mu.Lock()
	f.drains++
	f.mu.Unlock()
	return s.Send(&janusv1alpha1.ClusterDrainProgress{Message: "drained", Done: true})
}

// rebootingProxy forwards TCP to upstream; reboot() is a member's reboot
// as the Controller's shared connection to it sees it - silent until it
// sends, then a reset - while a new connection reaches the member again
// (the same as the Controller's nodeproxy tests).
type rebootingProxy struct {
	ln       net.Listener
	upstream string
	mu       sync.Mutex
	gen      int
}

func newRebootingProxy(t *testing.T, upstream string) *rebootingProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &rebootingProxy{ln: ln, upstream: upstream}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			gen := p.gen
			p.mu.Unlock()
			go p.serve(c, gen)
		}
	}()
	return p
}

func (p *rebootingProxy) serve(c net.Conn, gen int) {
	u, err := net.Dial("tcp", p.upstream)
	if err != nil {
		c.Close()
		return
	}
	go func() { _, _ = io.Copy(c, u) }()
	buf := make([]byte, 32<<10)
	for {
		n, err := c.Read(buf)
		if err != nil {
			u.Close()
			return
		}
		p.mu.Lock()
		dead := gen != p.gen
		p.mu.Unlock()
		if dead {
			_ = c.(*net.TCPConn).SetLinger(0)
			c.Close()
			u.Close()
			return
		}
		if _, err := u.Write(buf[:n]); err != nil {
			c.Close()
			return
		}
	}
}

func (p *rebootingProxy) reboot() {
	p.mu.Lock()
	p.gen++
	p.mu.Unlock()
}

// The first call on the shared connection to a member that rebooted
// meets the reset: an undrain and a drain go through anyway, each sent
// once to the member (#438).
func TestCallsAfterAMemberReboot(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fake := &fakeMember{}
	janusv1alpha1.RegisterClusterServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	proxy := newRebootingProxy(t, lis.Addr().String())
	conn, err := grpc.NewClient(proxy.ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	g := GRPC{Conn: func(context.Context, Member) (grpc.ClientConnInterface, error) { return conn, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m := Member{Name: "edge-1"}

	if _, err := g.ClusterDrain(ctx, m, time.Second); err != nil {
		t.Fatalf("drain before the reboot: %v", err)
	}
	proxy.reboot()
	if err := g.ClusterUndrain(ctx, m); err != nil {
		t.Fatalf("undrain right after the reboot: %v", err)
	}
	proxy.reboot()
	if msg, err := g.ClusterDrain(ctx, m, time.Second); err != nil || msg != "drained" {
		t.Fatalf("drain right after another reboot: %q, %v", msg, err)
	}
	fake.mu.Lock()
	drains, undrains := fake.drains, fake.undrains
	fake.mu.Unlock()
	if drains != 2 || undrains != 1 {
		t.Fatalf("the member served %d drains and %d undrains, want 2 and 1 - none sent twice", drains, undrains)
	}

}
