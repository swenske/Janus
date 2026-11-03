package nodeproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

type fakeAccess struct {
	janusv1alpha1.UnimplementedAccessServiceServer
	calls int
	mu    sync.Mutex
}

func (f *fakeAccess) TrustGet(context.Context, *emptypb.Empty) (*janusv1alpha1.TrustState, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return &janusv1alpha1.TrustState{BundleVersion: 7}, nil
}

// rebootingProxy forwards TCP to upstream. reboot() is what a node's
// reboot looks like from a client holding a connection to it: the
// upstream side is gone, the client's side stays open and silent, and
// the next bytes the client sends get a reset - while a new connection
// reaches the upstream again (the node is back).
type rebootingProxy struct {
	ln       net.Listener
	upstream string
	mu       sync.Mutex
	gen      int // connections from an older generation are dead
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
			// The node rebooted since: what arrives now gets a reset.
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

// reboot leaves every connection made so far dead (silent until the
// client sends, then reset); new ones work.
func (p *rebootingProxy) reboot() {
	p.mu.Lock()
	p.gen++
	p.mu.Unlock()
}

// TestOnceMoreAfterANodeReboot: the first call on the shared connection
// to a node that rebooted fails with Unavailable (its bytes come back as
// a reset); onceMore makes the call succeed on the connection grpc opened
// again, without the caller seeing the reset.
func TestOnceMoreAfterANodeReboot(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fake := &fakeAccess{}
	janusv1alpha1.RegisterAccessServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	proxy := newRebootingProxy(t, lis.Addr().String())
	conn, err := grpc.NewClient(proxy.ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := janusv1alpha1.NewAccessServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call := func(opts ...grpc.CallOption) (*janusv1alpha1.TrustState, error) {
		return client.TrustGet(ctx, &emptypb.Empty{}, opts...)
	}

	if _, err := onceMore(call); err != nil {
		t.Fatalf("before the reboot: %v", err)
	}

	// The node reboots: the connection's upstream half is gone, the
	// client holds a connection that looks fine - and a plain call sees
	// the reset.
	proxy.reboot()
	// Usually the reset: grpc may also have noticed the dead connection
	// by itself and opened a new one, on which the call just works (a
	// loaded CI runner) - anything else is wrong.
	plainCalls := 0
	if _, err := call(); err == nil {
		plainCalls = 1
	} else if status.Code(err) != codes.Unavailable {
		t.Fatalf("the first call on the dead connection: %v, want Unavailable", err)
	}
	// Once more, after another reboot: onceMore hides it.
	for _, err := onceMore(call); err != nil; _, err = onceMore(call) {
		t.Fatalf("recovering after the first reset: %v", err)
	}
	proxy.reboot()
	st, err := onceMore(call)
	if err != nil {
		t.Fatalf("onceMore after the reboot: %v", err)
	}
	if st.GetBundleVersion() != 7 {
		t.Fatalf("answer: %v", st)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.calls != 3+plainCalls {
		t.Fatalf("the node served %d calls, want 3 (before, after the first reboot, after the second)", fake.calls)
	}
}

// resettingTLSNode is a node that refuses the client's certificate the
// way a TLS 1.3 server does when its alert is lost: the handshake looks
// complete to the client (it sends its certificate in its last flight,
// checked after), then the connection is reset - the client's HTTP/2
// preface was already in the node's buffer, so closing sends a reset,
// and the alert in flight is dropped with it (#483).
func resettingTLSNode(t *testing.T) string {
	t.Helper()
	cert := selfSignedCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequestClientCert, NextProtos: []string{"h2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				tc := c.(*tls.Conn)
				if tc.Handshake() != nil {
					_ = c.Close()
					return
				}
				// Wait for the client's preface, then reset.
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 1)
				_, _ = tc.Read(buf)
				if raw, ok := tc.NetConn().(*net.TCPConn); ok {
					_ = raw.SetLinger(0)
				}
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// TestRefusedWhenTheAlertIsLost: a node resetting the connection right
// after the TLS handshake - before its HTTP/2 preface - refused the
// certificate: the call says so at once, rather than waiting for the
// connection to come up until the caller's deadline (#483).
func TestRefusedWhenTheAlertIsLost(t *testing.T) {
	addr := resettingTLSNode(t)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, // #nosec G402 -- a test's throwaway server
		Certificates: []tls.Certificate{selfSignedCert(t)},
	})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err = onceMore(func(opts ...grpc.CallOption) (*janusv1alpha1.TrustState, error) {
		return janusv1alpha1.NewAccessServiceClient(conn).TrustGet(ctx, &emptypb.Empty{}, opts...)
	})
	t.Logf("after %v: %v", time.Since(start).Round(time.Millisecond), err)
	if !RefusedCertificate(err) {
		t.Errorf("a reset right after the handshake isn't taken for a refused certificate: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v: waited for the connection instead of answering", time.Since(start))
	}
}

// selfSignedCert is a throwaway ECDSA certificate.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
