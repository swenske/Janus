package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/netip"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// TestClientList drives Clients over a real mTLS gRPC server: a
// Controller acting for two users - one of them with a stream open -,
// two direct clients; what each sees of the others, the bytes and calls
// counted, a closed connection gone.
func TestClientList(t *testing.T) {
	defer func(f func(dst, src netip.Addr) (string, error)) { egressInterface = f }(egressInterface)
	egressInterface = func(dst, src netip.Addr) (string, error) { return "lo", nil }

	ca, err := pki.NewCA("node")
	if err != nil {
		t.Fatal(err)
	}
	srvPEM, srvKey, err := ca.Issue(pki.IssueOptions{CommonName: "node", IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		t.Fatal(err)
	}
	srvCert, err := tls.X509KeyPair(srvPEM, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	clients := NewClients()
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(ca.ServerTLSConfig(srvCert))),
		grpc.StatsHandler(ConnStats{Clients: clients}),
		grpc.UnaryInterceptor(UnaryAuthInterceptor),
		grpc.StreamInterceptor(StreamAuthInterceptor),
	)
	janusv1alpha1.RegisterSystemServiceServer(srv, &System{BuildVersion: "test", Clients: clients})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(clients.Listener(lis)) }()
	defer srv.Stop()

	conns := map[string]*grpc.ClientConn{}
	dial := func(name string, roles ...string) janusv1alpha1.SystemServiceClient {
		t.Helper()
		certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{CommonName: name, Roles: roles, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := pki.ClientTLSConfig(ca.CertPEM, certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		conns[name] = conn
		return janusv1alpha1.NewSystemServiceClient(conn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	as := func(user, roles string) context.Context {
		return metadata.AppendToOutgoingContext(ctx, pki.AsUserKey, user, pki.AsRolesKey, roles)
	}

	ctl := dial("controller", pki.RoleController)
	for _, c := range []context.Context{as("bob", "os:admin"), as("carol", "os:reader"), as("bob", "os:admin")} {
		if _, err := ctl.Version(c, &emptypb.Empty{}); err != nil {
			t.Fatal(err)
		}
	}
	streamCtx, streamCancel := context.WithCancel(as("carol", "os:reader"))
	defer streamCancel()
	if _, err := ctl.Events(streamCtx, &janusv1alpha1.EventsRequest{}); err != nil {
		t.Fatal(err)
	}
	alice := dial("alice", pki.RoleReader)
	conns["alice"].Connect() // connected, no call yet
	root := dial("root", pki.RoleAdmin)
	gone := dial("gone", pki.RoleReader)
	if _, err := gone.Version(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Version(ctx, &emptypb.Empty{}); err != nil {
		t.Fatal(err)
	}

	// The stream reaches the server a moment after it's opened.
	waitFor := func(what string, ok func(*janusv1alpha1.ClientListResponse) bool, c janusv1alpha1.SystemServiceClient, cctx context.Context) *janusv1alpha1.ClientListResponse {
		t.Helper()
		for i := 0; ; i++ {
			resp, err := c.ClientList(cctx, &emptypb.Empty{})
			if err != nil {
				t.Fatal(err)
			}
			if ok(resp) {
				return resp
			}
			if i == 100 {
				t.Fatalf("%s: %v", what, resp)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	controllerOf := func(r *janusv1alpha1.ClientListResponse) *janusv1alpha1.ClientConnection {
		for _, c := range r.GetConnections() {
			if c.GetController() {
				return c
			}
		}
		return nil
	}
	resp := waitFor("the Controller's stream", func(r *janusv1alpha1.ClientListResponse) bool {
		c := controllerOf(r)
		return c != nil && c.GetOpenStreams() == 1 && len(r.GetConnections()) == 4
	}, root, ctx)

	if n := len(resp.GetConnections()); n != 4 {
		t.Fatalf("%d connections, want 4: %v", n, resp)
	}
	byName := map[string]*janusv1alpha1.ClientConnection{}
	for _, c := range resp.GetConnections() {
		byName[c.GetName()] = c
		if c.GetInterface() != "lo" || c.GetLocalAddress() != lis.Addr().String() || c.GetRemoteAddress() == "" || c.GetOpenedUnixNs() == 0 {
			t.Errorf("%s: %v", c.GetName(), c)
		}
		if c.GetBytesReceived() == 0 || c.GetBytesSent() == 0 {
			t.Errorf("%s: no bytes counted: %v", c.GetName(), c)
		}
	}
	c := byName["controller"]
	if c == nil || !c.GetController() || c.GetCalls() != 4 || c.GetUserCount() != 2 || len(c.GetUsers()) != 2 || c.GetSelf() {
		t.Fatalf("the Controller: %v", c)
	}
	carol, bob := c.GetUsers()[0], c.GetUsers()[1]
	if carol.GetName() != "carol" || carol.GetCalls() != 2 || carol.GetOpenStreams() != 1 || carol.GetRoles()[0] != "os:reader" {
		t.Errorf("the latest first, carol with her stream: %v", carol)
	}
	if bob.GetName() != "bob" || bob.GetCalls() != 2 || bob.GetOpenStreams() != 0 || bob.GetLastCallUnixNs() == 0 {
		t.Errorf("bob: %v", bob)
	}
	if r := byName["root"]; r == nil || !r.GetSelf() || r.GetController() || r.GetCalls() != 2 || r.GetRoles()[0] != pki.RoleAdmin {
		t.Errorf("the caller's own: %v", r)
	}
	if a := byName["alice"]; a == nil || a.GetCalls() != 0 || a.GetSelf() {
		t.Errorf("alice, connected, no call yet: %v", a)
	}

	// A reader: the Controller's users counted, not named; the other
	// clients nameless.
	resp, err = alice.ClientList(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.GetConnections() {
		switch {
		case c.GetController():
			if c.GetName() != "controller" || c.GetUserCount() != 2 || len(c.GetUsers()) != 0 {
				t.Errorf("a reader's view of the Controller: %v", c)
			}
		case c.GetSelf():
			if c.GetName() != "alice" || c.GetRemoteAddress() == "" {
				t.Errorf("a reader's own: %v", c)
			}
		default:
			if c.GetName() != "" || c.GetRemoteAddress() != "" || len(c.GetRoles()) != 0 {
				t.Errorf("a reader sees another client: %v", c)
			}
		}
	}
	// Through the Controller, carol finds herself among its users - alone.
	resp, err = ctl.ClientList(as("carol", "os:reader"), &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if c := controllerOf(resp); c == nil || !c.GetSelf() || len(c.GetUsers()) != 1 || !c.GetUsers()[0].GetSelf() || c.GetUsers()[0].GetName() != "carol" || c.GetUserCount() != 2 {
		t.Errorf("carol through the Controller: %v", c)
	}

	// The stream ended, then a connection closed: counted, gone.
	streamCancel()
	waitFor("carol's stream ended", func(r *janusv1alpha1.ClientListResponse) bool {
		c := controllerOf(r)
		return c != nil && c.GetOpenStreams() == 0 && c.GetUsers()[0].GetOpenStreams() == 0
	}, root, ctx)
	conns["gone"].Close()
	waitFor("a connection closed", func(r *janusv1alpha1.ClientListResponse) bool {
		for _, c := range r.GetConnections() {
			if c.GetName() == "gone" {
				return false
			}
		}
		return len(r.GetConnections()) == 3
	}, root, ctx)
}
