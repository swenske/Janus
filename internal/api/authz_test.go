package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"strings"
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
	requiredRoles["/test.Service/AdminOnly"] = []string{pki.RoleAdmin}
	requiredRoles["/test.Service/AdminOrReader"] = []string{pki.RoleAdmin, pki.RoleOperator, pki.RoleReader}
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

// TestFleetCaller: a certificate chained to the fleet's root is told
// apart from the node's own CA's - the same name in both.
func TestFleetCaller(t *testing.T) {
	root := &x509.Certificate{Raw: []byte("fleet root"), Subject: pkix.Name{CommonName: "root"}}
	local := &x509.Certificate{Raw: []byte("node CA"), Subject: pkix.Name{CommonName: "node"}}
	prev := FleetRoot
	FleetRoot = func() *x509.Certificate { return root }
	t.Cleanup(func() { FleetRoot = prev })
	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: "admin", Organization: []string{"os:admin"}}}
	with := func(chainRoot *x509.Certificate) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{}, AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{leaf},
			VerifiedChains:   [][]*x509.Certificate{{leaf, chainRoot}},
		}}})
	}
	const show = "/janus.v1alpha1.HAProxyService/ShowInfo"
	if c, err := authorize(with(root), show); err != nil || c.String() != "admin (os:admin, fleet)" {
		t.Errorf("the fleet's: %q %v", c, err)
	}
	if c, err := authorize(with(local), show); err != nil || c.String() != "admin (os:admin)" {
		t.Errorf("the node's own: %q %v", c, err)
	}
}

// TestControllerNarrowsDomains: the Controller can narrow the user it
// acts for to some domains - the node holds it to them.
func TestControllerNarrowsDomains(t *testing.T) {
	ctl := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "janus-controller", Organization: []string{"janus:controller"}}})
	as := func(domains string) context.Context {
		return metadata.NewIncomingContext(ctl, metadata.Pairs(pki.AsUserKey, "tf", pki.AsRolesKey, "os:operator", pki.AsDomainsKey, domains))
	}
	for _, c := range []struct {
		domains, method string
		want            codes.Code
	}{
		{"haproxy", "/janus.v1alpha1.HAProxyService/ApplyConfig", codes.OK},
		{"haproxy", "/janus.v1alpha1.SystemService/Stats", codes.OK},
		{"haproxy", "/janus.v1alpha1.SystemService/Reboot", codes.PermissionDenied},
		{"haproxy,services", "/janus.v1alpha1.SystemService/Reboot", codes.OK},
		{"haproxy", "/janus.v1alpha1.NetworkService/NetworkConfigApply", codes.PermissionDenied},
		{"root", "/janus.v1alpha1.SystemService/Stats", codes.PermissionDenied},
		{"observe", "/janus.v1alpha1.SystemService/Stats", codes.OK},
		{"observe", "/janus.v1alpha1.HAProxyService/ApplyConfig", codes.PermissionDenied},
	} {
		if got := status.Code(checkRole(as(c.domains), c.method)); got != c.want {
			t.Errorf("%s, %s: %v, want %v", c.domains, c.method, got, c.want)
		}
	}
	caller, err := authorize(as("haproxy"), "/janus.v1alpha1.HAProxyService/ApplyConfig")
	if err != nil || caller.String() != "tf (os:operator; haproxy) via janus-controller" {
		t.Errorf("caller %q, %v", caller, err)
	}
}

// TestScopedCertificate: a scoped certificate opens this node only as its
// scope names it - by the node's CA's key -, with the role and domains it
// gives here.
func TestScopedCertificate(t *testing.T) {
	local, err := pki.NewCA("this node")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := pki.NewCA("another node")
	issuing, _ := pki.NewCA("fleet issuing CA")
	prev := LocalCA
	LocalCA = func() *x509.Certificate { return local.Cert }
	t.Cleanup(func() { LocalCA = prev })
	leafOf := func(s *pki.Scope) context.Context {
		t.Helper()
		certPEM, _, err := issuing.Issue(pki.IssueOptions{CommonName: "web-dev", Scope: s, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(certPEM)
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return peerWith(leaf)
	}
	here := [][]byte{pki.CAKey(local.Cert)}
	there := [][]byte{pki.CAKey(other.Cert)}
	webHere := leafOf(&pki.Scope{Nodes: []pki.ScopeNodes{{CAKeys: here, Perms: []pki.ScopePerm{{Role: pki.RoleOperator, Domains: []string{"haproxy"}}}}}})
	webThere := leafOf(&pki.Scope{Nodes: []pki.ScopeNodes{{CAKeys: there, Perms: []pki.ScopePerm{{Role: pki.RoleAdmin}}}}})
	readerEverywhere := leafOf(&pki.Scope{Any: []pki.ScopePerm{{Role: pki.RoleReader}}, Nodes: []pki.ScopeNodes{{CAKeys: there, Perms: []pki.ScopePerm{{Role: pki.RoleAdmin}}}}})
	unknownDomain := leafOf(&pki.Scope{Any: []pki.ScopePerm{{Role: pki.RoleOperator, Domains: []string{"root"}}}})
	const apply, restart, stats, upgrade = "/janus.v1alpha1.HAProxyService/ApplyConfig", "/janus.v1alpha1.SystemService/ServiceRestart", "/janus.v1alpha1.SystemService/Stats", "/janus.v1alpha1.LifecycleService/Upgrade"
	for _, c := range []struct {
		name   string
		ctx    context.Context
		method string
		want   codes.Code
		msg    string
	}{
		{"operator on HAProxy here, applying", webHere, apply, codes.OK, ""},
		{"operator on HAProxy here, observing", webHere, stats, codes.OK, ""},
		{"operator on HAProxy here, restarting a service", webHere, restart, codes.PermissionDenied, "in the services domain: web-dev may only haproxy on this node"},
		{"operator on HAProxy here, upgrading", webHere, upgrade, codes.PermissionDenied, "requires role [os:admin], web-dev has [os:operator] on this node"},
		{"admin elsewhere", webThere, stats, codes.PermissionDenied, "doesn't open this node"},
		{"reader everywhere, admin elsewhere, reading", readerEverywhere, stats, codes.OK, ""},
		{"reader everywhere, admin elsewhere, applying", readerEverywhere, apply, codes.PermissionDenied, "has [os:reader] on this node"},
		{"an unknown domain", unknownDomain, stats, codes.PermissionDenied, "unknown domain"},
	} {
		err := checkRole(c.ctx, c.method)
		if got := status.Code(err); got != c.want || c.msg != "" && !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v, want %v (%q)", c.name, err, c.want, c.msg)
		}
	}
	caller, err := authorize(webHere, apply)
	if err != nil || caller.String() != "web-dev (os:operator, scoped; haproxy)" {
		t.Errorf("caller %q, %v", caller, err)
	}
	// The marker beside a role of its own, or without its scope: refused.
	for name, o := range map[string][]string{"with a role": {pki.RoleScoped, pki.RoleAdmin}, "without its scope": {pki.RoleScoped}} {
		ctx := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "x", Organization: o}})
		if got := status.Code(checkRole(ctx, stats)); got != codes.PermissionDenied {
			t.Errorf("%s: %v", name, got)
		}
	}
}
