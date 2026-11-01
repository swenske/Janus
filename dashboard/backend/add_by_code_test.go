package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/api"
	"github.com/swenske/Janus/internal/pairing"
	"github.com/swenske/Janus/internal/pki"
)

// pairingWindow is a node's pairing window on a real TLS listener, as
// janusd opens it: its address, its code, the node's fleet and CA.
func pairingWindow(t *testing.T) (string, pairing.Code, *pki.Fleet, *pki.CA) {
	t.Helper()
	ca, err := pki.NewCA("node")
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := pki.OpenFleet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := pairing.NewLeaf(ca, "edge-1", []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	code, err := pairing.NewCode(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(pairing.ServerTLS(leaf))), grpc.UnaryInterceptor(api.PairingInterceptor))
	janusv1alpha1.RegisterPairingServiceServer(srv, &api.Pairing{Window: pairing.NewWindow(code.SecretBytes(), 0), Access: &api.Access{Fleet: fleet, Local: pki.NewLocal(ca)}})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), code, fleet, ca
}

// TestAddNodeByCode: a node is added by the code its console shows - only
// with a fleet set up; a code naming another certificate, a wrong one, are
// refused with the node's words; the right one leaves the node trusting
// the fleet and its record pinning its CA, no credential kept; the same
// node again is refused.
func TestAddNodeByCode(t *testing.T) {
	a, _ := newTestApp(t)
	addr, code, nodeFleet, ca := pairingWindow(t)
	add := func(c string) (int, string) {
		rec := call(t, a.handleNodes, "POST", "/api/nodes", "/api/nodes", map[string]any{
			"name": "edge-1", "address": "127.0.0.1:9505", "pairing_address": addr, "code": c,
			"labels": map[string]string{"site": "par"},
		})
		return rec.Code, rec.Body.String()
	}

	if got, body := add(code.String()); got != http.StatusConflict || !strings.Contains(body, "fleet isn't set up") {
		t.Fatalf("without a fleet: %d %s", got, body)
	}
	withFleet(t, a)

	if got, body := add("not-a-code"); got != http.StatusBadRequest {
		t.Errorf("not a code: %d %s", got, body)
	}
	other := pairing.Code{Prefix: code.Prefix ^ 1, Secret: code.Secret}
	if got, body := add(other.String()); got != http.StatusBadRequest || !strings.Contains(body, "isn't the one its code names") {
		t.Errorf("another certificate's code: %d %s", got, body)
	}
	wrong := pairing.Code{Prefix: code.Prefix, Secret: code.Secret ^ 1}
	if got, body := add(wrong.String()); got != http.StatusBadRequest || !strings.Contains(body, "wrong pairing code - 4 tries left") {
		t.Errorf("a wrong secret: %d %s", got, body)
	}
	if len(a.store.List()) != 0 {
		t.Fatal("a node recorded on a refused code")
	}

	if got, body := add(code.String()); got != http.StatusCreated {
		t.Fatalf("the right code: %d %s", got, body)
	}
	nodes := a.store.List()
	if len(nodes) != 1 || !nodes[0].TrustsFleet() || nodes[0].LabelSet()["site"] != "par" {
		t.Fatalf("added: %+v", nodes)
	}
	if cert, key := nodes[0].ServiceCredential(); cert != nil || key != nil {
		t.Error("a credential kept for a node paired by code")
	}
	b, _ := pem.Decode(nodes[0].CA())
	if got, err := x509.ParseCertificate(b.Bytes); err != nil || !got.Equal(ca.Cert) {
		t.Errorf("the record pins another CA: %v", err)
	}
	rootPEM, _, _, _ := a.fleet.Trust()
	if r := nodeFleet.Root(); r == nil || string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.Raw})) != string(rootPEM) {
		t.Error("the node doesn't trust this Controller's fleet")
	}

	// Its answer replayed (the window answers the same call again a
	// while): the same node isn't recorded twice.
	if got, body := add(code.String()); got != http.StatusConflict || !strings.Contains(body, "already here, as edge-1") {
		t.Errorf("the same node again: %d %s", got, body)
	}
	if len(a.store.List()) != 1 {
		t.Error("the same node recorded twice")
	}
}

// TestPairingAddress: the window is looked for on the API's host, port
// 9506, unless the request names it; no window there says so.
func TestPairingAddress(t *testing.T) {
	a, _ := newTestApp(t)
	withFleet(t, a)
	var dialed string
	prev := pairNode
	pairNode = func(_ context.Context, addr, _ string, _, _ []byte, _ ...grpc.DialOption) (*janusv1alpha1.PairResponse, *x509.Certificate, error) {
		dialed = addr
		return nil, nil, status.Error(codes.Unavailable, `connection error: desc = "transport: Error while dialing: dial tcp: connect: connection refused"`)
	}
	t.Cleanup(func() { pairNode = prev })
	code := pairing.Code{Prefix: 1, Secret: 2}.String()
	for _, c := range []struct{ address, pairing, want string }{
		{"10.0.0.5:9505", "", "10.0.0.5:9506"},
		{"[2001:db8::5]:9505", "", "[2001:db8::5]:9506"},
		{"lb1.example.net:19505", "", "lb1.example.net:9506"},
		{"10.0.0.5:9505", "192.0.2.7:19506", "192.0.2.7:19506"},
	} {
		rec := call(t, a.handleNodes, "POST", "/api/nodes", "/api/nodes", map[string]any{"name": "n", "address": c.address, "pairing_address": c.pairing, "code": code})
		if dialed != c.want || rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "no pairing window answers at "+c.want+" (connection refused)") {
			t.Errorf("%s/%q: dialed %s, %d %s", c.address, c.pairing, dialed, rec.Code, rec.Body)
		}
	}
	if rec := call(t, a.handleNodes, "POST", "/api/nodes", "/api/nodes", map[string]any{"name": "n", "address": "10.0.0.5", "code": code}); rec.Code != http.StatusBadRequest {
		t.Errorf("an address without a port: %d %s", rec.Code, rec.Body)
	}
}
