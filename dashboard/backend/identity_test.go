package main

import (
	"context"
	"encoding/pem"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/acmerelay"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// fakeACMENode is a node's Let's Encrypt as the relay's settings see it.
type fakeACMENode struct {
	janusv1alpha1.UnimplementedHAProxyServiceServer
	mu      sync.Mutex
	cfg     *janusv1alpha1.ACMEConfig
	applied []*janusv1alpha1.ACMEConfig
}

func (f *fakeACMENode) ACMEStatus(context.Context, *emptypb.Empty) (*janusv1alpha1.ACMEStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &janusv1alpha1.ACMEStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_RUNNING, PathMode: f.cfg.GetPath().GetMode()}, nil
}

func (f *fakeACMENode) ACMEGetConfig(context.Context, *emptypb.Empty) (*janusv1alpha1.ACMEGetConfigResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &janusv1alpha1.ACMEGetConfigResponse{Config: f.cfg}, nil
}

func (f *fakeACMENode) ACMEApplyConfig(_ context.Context, req *janusv1alpha1.ACMEApplyConfigRequest) (*janusv1alpha1.ACMEApplyConfigResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, req.GetConfig())
	f.cfg = req.GetConfig()
	return &janusv1alpha1.ACMEApplyConfigResponse{Accepted: true}, nil
}

func acmeNode(t *testing.T, id string, f *fakeACMENode) *store.Node {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	janusv1alpha1.RegisterHAProxyServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	node := &store.Node{ID: id, Name: id}
	nodeproxy.SetConnForTests(node.ID, conn)
	t.Cleanup(func() { nodeproxy.Forget(node.ID) })
	return node
}

// TestRelayTrust: a node reaching Let's Encrypt through the relay with
// the Controller's certificate alone - as it was given before the
// Controller kept its CA - is given the CA, keeping the rest of its
// settings; one given the CA already, or not through the
// relay, is left alone. (The node is called as lets-encrypt - the
// connection's interceptor says so, which this fake one has not.)
func TestRelayTrust(t *testing.T) {
	a, _ := newTestApp(t)
	leafPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.identity.Certificate().Certificate[0]}))
	relay := &fakeACMENode{cfg: &janusv1alpha1.ACMEConfig{
		Account:      &janusv1alpha1.ACMEAccount{Email: "ops@example.com"},
		Certificates: []*janusv1alpha1.ACMECertificate{{Name: "www", Domains: []string{"www.example.com"}}},
		Path:         &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_CONTROLLER_RELAY, ProxyUrl: "https://192.0.2.1:8444", ProxyCa: leafPEM},
	}}
	direct := &fakeACMENode{cfg: &janusv1alpha1.ACMEConfig{Path: &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_DIRECT}}}
	ctx := context.Background()

	if err := a.syncNodeRelayTrust(ctx, acmeNode(t, "relay-node", relay)); err != nil {
		t.Fatal(err)
	}
	if len(relay.applied) != 1 {
		t.Fatalf("applied %d times", len(relay.applied))
	}
	got := relay.applied[0]
	if got.GetPath().GetProxyCa() != string(a.identity.TrustPEM()) || !a.identity.HoldsCA([]byte(got.GetPath().GetProxyCa())) {
		t.Errorf("proxy_ca %q", got.GetPath().GetProxyCa())
	}
	if got.GetPath().GetProxyUrl() != "https://192.0.2.1:8444" || got.GetAccount().GetEmail() != "ops@example.com" || len(got.GetCertificates()) != 1 {
		t.Errorf("the rest of the settings changed: %v", got)
	}
	// Given the CA, it's left alone.
	if err := a.syncNodeRelayTrust(ctx, acmeNode(t, "relay-node-2", relay)); err != nil || len(relay.applied) != 1 {
		t.Errorf("applied again: %d, %v", len(relay.applied), err)
	}
	if err := a.syncNodeRelayTrust(ctx, acmeNode(t, "direct-node", direct)); err != nil || len(direct.applied) != 0 {
		t.Errorf("a node reaching the CA itself: %d, %v", len(direct.applied), err)
	}
}

// TestRelayURLFollows: a node still set to a relay URL this Controller
// gave - before its relay's address changed, or before it was restored on
// another host - is given the one now, the rest of its settings kept; a
// node set to an address an admin typed on it is left alone.
func TestRelayURLFollows(t *testing.T) {
	a, _ := newTestApp(t)
	a.relay = &acmerelay.Server{Identity: a.identity.Certificate}
	a.relay.Boot(acmerelay.Config{Address: "198.51.100.9:8444"})
	t.Cleanup(a.relay.Close)
	givenFile := filepath.Join(t.TempDir(), relayGivenFile)
	given, err := acmerelay.LoadGiven(givenFile)
	if err != nil {
		t.Fatal(err)
	}
	a.relayGiven = given
	const old, now, typed = "https://192.0.2.1:8444", "https://198.51.100.9:8444", "https://203.0.113.5:8444"
	if err := given.Note(old); err != nil { // what it gave on its old host
		t.Fatal(err)
	}
	trust := string(a.identity.TrustPEM())
	path := func(url string) *janusv1alpha1.ACMEPath {
		return &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_CONTROLLER_RELAY, ProxyUrl: url, ProxyCa: trust}
	}
	ours := &fakeACMENode{cfg: &janusv1alpha1.ACMEConfig{Account: &janusv1alpha1.ACMEAccount{Email: "ops@example.com"}, Path: path(old)}}
	own := &fakeACMENode{cfg: &janusv1alpha1.ACMEConfig{Path: path(typed)}}
	ctx := context.Background()

	if err := a.syncNodeRelayTrust(ctx, acmeNode(t, "ours", ours)); err != nil {
		t.Fatal(err)
	}
	if len(ours.applied) != 1 {
		t.Fatalf("applied %d times", len(ours.applied))
	}
	got := ours.applied[0]
	if got.GetPath().GetProxyUrl() != now || got.GetPath().GetProxyCa() != trust || got.GetAccount().GetEmail() != "ops@example.com" {
		t.Errorf("settings applied: %v", got)
	}
	if err := a.syncNodeRelayTrust(ctx, acmeNode(t, "ours-again", ours)); err != nil || len(ours.applied) != 1 {
		t.Errorf("applied again: %d, %v", len(ours.applied), err)
	}
	if err := a.syncNodeRelayTrust(ctx, acmeNode(t, "own", own)); err != nil || len(own.applied) != 0 {
		t.Errorf("an address typed on the node was replaced: %d, %v", len(own.applied), err)
	}

	// The URLs given are kept: a restored data directory brings them.
	again, err := acmerelay.LoadGiven(givenFile)
	if err != nil || !again.Has(old) || !again.Has(now) || again.Has(typed) {
		t.Errorf("kept: old %v, now %v, typed %v (%v)", again.Has(old), again.Has(now), again.Has(typed), err)
	}
}
