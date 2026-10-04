package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// TestRequiredRolesCoversEveryRPC registers every service against a real
// *grpc.Server and cross-checks its actual method list against
// requiredRoles, in both directions - catches an RPC added to a .proto
// without a corresponding entry here (would silently fail closed to
// admin-only, which is safe but easy to miss), and a stale entry left
// behind after an RPC is renamed or removed.
func TestRequiredRolesCoversEveryRPC(t *testing.T) {
	srv := grpc.NewServer()
	janusv1alpha1.RegisterSystemServiceServer(srv, &System{})
	janusv1alpha1.RegisterLifecycleServiceServer(srv, &Lifecycle{})
	janusv1alpha1.RegisterHAProxyServiceServer(srv, &HAProxy{})
	janusv1alpha1.RegisterNetworkServiceServer(srv, &Network{})
	janusv1alpha1.RegisterAccessServiceServer(srv, &Access{})

	seen := map[string]bool{}
	for serviceName, info := range srv.GetServiceInfo() {
		for _, m := range info.Methods {
			full := "/" + serviceName + "/" + m.Name
			seen[full] = true
			if _, ok := requiredRoles[full]; !ok {
				t.Errorf("RPC %s has no entry in requiredRoles", full)
			}
		}
	}
	for full := range requiredRoles {
		if !seen[full] {
			t.Errorf("requiredRoles has a stale entry for %s (not a registered RPC)", full)
		}
	}
}

func TestCheckRole(t *testing.T) {
	requiredRoles["/test.Service/AdminOnly"] = adminOnly
	requiredRoles["/test.Service/AdminOrReader"] = readers
	t.Cleanup(func() {
		delete(requiredRoles, "/test.Service/AdminOnly")
		delete(requiredRoles, "/test.Service/AdminOrReader")
	})

	tests := []struct {
		name      string
		method    string
		peerRoles []string
		noPeer    bool
		wantCode  codes.Code
	}{
		{name: "admin can call admin-only", method: "/test.Service/AdminOnly", peerRoles: []string{"os:admin"}, wantCode: codes.OK},
		{name: "reader cannot call admin-only", method: "/test.Service/AdminOnly", peerRoles: []string{"os:reader"}, wantCode: codes.PermissionDenied},
		{name: "reader can call admin-or-reader", method: "/test.Service/AdminOrReader", peerRoles: []string{"os:reader"}, wantCode: codes.OK},
		{name: "unknown role is rejected everywhere", method: "/test.Service/AdminOrReader", peerRoles: []string{"os:mystery"}, wantCode: codes.PermissionDenied},
		{name: "unlisted method fails closed to admin-only", method: "/test.Service/NeverClassified", peerRoles: []string{"os:reader"}, wantCode: codes.PermissionDenied},
		{name: "unlisted method allows admin", method: "/test.Service/NeverClassified", peerRoles: []string{"os:admin"}, wantCode: codes.OK},
		{name: "no client certificate at all", method: "/test.Service/AdminOrReader", noPeer: true, wantCode: codes.Unauthenticated},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if !tc.noPeer {
				ctx = peer.NewContext(ctx, &peer.Peer{
					Addr: &net.TCPAddr{},
					AuthInfo: credentials.TLSInfo{
						State: tls.ConnectionState{
							PeerCertificates: []*x509.Certificate{
								{Subject: pkix.Name{Organization: tc.peerRoles}},
							},
						},
					},
				})
			}

			err := checkRole(ctx, tc.method)
			if tc.wantCode == codes.OK {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if status.Code(err) != tc.wantCode {
				t.Fatalf("expected code %v, got %v (%v)", tc.wantCode, status.Code(err), err)
			}
		})
	}
}

// TestOperatorRole: an operator runs HAProxy and services, reads logs -
// and doesn't touch how the node is set up, nor read its files.
func TestOperatorRole(t *testing.T) {
	ctx := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "op", Organization: []string{"os:operator"}}})
	for method, want := range map[string]codes.Code{
		"/janus.v1alpha1.HAProxyService/ApplyConfig":                codes.OK,
		"/janus.v1alpha1.HAProxyService/MapUpdate":                  codes.OK,
		"/janus.v1alpha1.HAProxyService/ShowInfo":                   codes.OK,
		"/janus.v1alpha1.SystemService/Reboot":                      codes.OK,
		"/janus.v1alpha1.SystemService/ServiceRestart":              codes.OK,
		"/janus.v1alpha1.SystemService/Logs":                        codes.OK,
		"/janus.v1alpha1.SystemService/Reset":                       codes.PermissionDenied,
		"/janus.v1alpha1.SystemService/Read":                        codes.PermissionDenied,
		"/janus.v1alpha1.SystemService/PacketCapture":               codes.PermissionDenied,
		"/janus.v1alpha1.SystemService/GenerateClientConfiguration": codes.PermissionDenied,
		"/janus.v1alpha1.LifecycleService/Upgrade":                  codes.PermissionDenied,
		"/janus.v1alpha1.NetworkService/NetworkConfigApply":         codes.PermissionDenied,
		"/janus.v1alpha1.NetworkService/FirewallApplyRuleset":       codes.PermissionDenied,
	} {
		if got := status.Code(checkRole(ctx, method)); got != want {
			t.Errorf("operator calling %s: %v, want %v", method, got, want)
		}
	}
	if got := status.Code(checkRole(peerWith(&x509.Certificate{Subject: pkix.Name{Organization: []string{"os:reader"}}}), "/janus.v1alpha1.SystemService/Logs")); got != codes.PermissionDenied {
		t.Errorf("reader reading logs: %v", got)
	}
}

func peerWith(leaf *x509.Certificate) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{
		Addr:     &net.TCPAddr{},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}},
	})
}

// TestControllerActsForAUser: the Controller's certificate has no right
// of its own - the call gets the roles of the user it names, and is
// refused when it names none.
func TestControllerActsForAUser(t *testing.T) {
	ctl := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "janus-controller", Organization: []string{"janus:controller"}}})
	as := func(user, roles string) context.Context {
		return metadata.NewIncomingContext(ctl, metadata.Pairs(pki.AsUserKey, user, pki.AsRolesKey, roles))
	}
	const apply, upgrade, show = "/janus.v1alpha1.HAProxyService/ApplyConfig", "/janus.v1alpha1.LifecycleService/Upgrade", "/janus.v1alpha1.HAProxyService/ShowInfo"
	for _, c := range []struct {
		name   string
		ctx    context.Context
		method string
		want   codes.Code
	}{
		{"for nobody", ctl, show, codes.PermissionDenied},
		{"for an operator, applying a config", as("alice", "os:operator"), apply, codes.OK},
		{"for an operator, upgrading", as("alice", "os:operator"), upgrade, codes.PermissionDenied},
		{"for an admin, upgrading", as("bob", "os:admin"), upgrade, codes.OK},
		{"for a reader, applying a config", as("carol", "os:reader"), apply, codes.PermissionDenied},
		{"with a role of its own", as("mallory", "janus:controller"), show, codes.PermissionDenied},
		{"with an unknown role", as("mallory", "os:root"), show, codes.PermissionDenied},
		{"for a malformed name", as("bad\nname", "os:admin"), show, codes.PermissionDenied},
	} {
		if got := status.Code(checkRole(c.ctx, c.method)); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}

	caller, err := authorize(as("alice", "os:operator,os:reader"), apply)
	if err != nil || caller.String() != "alice (os:operator, os:reader) via janus-controller" {
		t.Errorf("caller %q, %v", caller, err)
	}
	// Metadata from a certificate that isn't the Controller's is ignored.
	plain := metadata.NewIncomingContext(peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "r", Organization: []string{"os:reader"}}}), metadata.Pairs(pki.AsUserKey, "root", pki.AsRolesKey, "os:admin"))
	if got := status.Code(checkRole(plain, apply)); got != codes.PermissionDenied {
		t.Errorf("a reader naming an admin: %v", got)
	}
}
