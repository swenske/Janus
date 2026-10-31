package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/swenske/Janus/dashboard/backend/internal/fleet"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/api"
	"github.com/swenske/Janus/internal/pki"
)

// realFleetNode is a node trusting a's fleet at its bundle now: its own
// CA and server certificate, its TLS as janusd builds it, AccessService
// behind janusd's own role check - reached by the Controller over real
// TLS with its fleet certificate.
func realFleetNode(t *testing.T, a *app, name string) (*store.Node, *pki.Fleet) {
	t.Helper()
	dir := t.TempDir()
	b, err := pki.LoadOrBootstrap(dir, "localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	sc := pki.NewServerCert(b.CA, dir, b.ServerCert)
	local := pki.NewLocal(b.CA)
	fl, err := pki.OpenFleet(dir)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM, bundle, version, _ := a.fleet.Trust()
	if err := fl.Set(rootPEM, bundle); err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(pki.NodeTLSConfig(local.CA, sc, fl, nil))), grpc.UnaryInterceptor(api.UnaryAuthInterceptor))
	janusv1alpha1.RegisterAccessServiceServer(srv, &api.Access{Fleet: fl, Local: local, ServerCert: sc})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	node := &store.Node{Name: name, Address: ln.Addr().String(), CACertPEM: b.CA.CertPEM, Fleet: true, FleetBundle: version}
	if err := a.store.Add(node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nodeproxy.Forget(node.ID) })
	return node, fl
}

// fleetApp is a Controller whose fleet is set up, its kit and
// passphrase, the Controller's certificates chosen per node as dashboardd
// does.
func fleetApp(t *testing.T) (*app, []byte, string) {
	t.Helper()
	a, _ := newTestApp(t)
	key, err := secrets.LoadOrCreate("", a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := fleet.Open(filepath.Join(a.dataDir, "fleet"), key, a.controllerID)
	if err != nil {
		t.Fatal(err)
	}
	pass, _ := f.Setup()
	kit, _ := f.RecoveryKit()
	if err := f.Confirm(kit, pass); err != nil {
		t.Fatal(err)
	}
	a.fleet, a.trust = f, newTrustTracker()
	prevID, prevRoot := nodeproxy.FleetIdentity, api.FleetRoot
	nodeproxy.FleetIdentity = func(n *store.Node) (*tls.Certificate, error) { return a.fleet.ClientCertificateFor(n.Bundle()) }
	root, _ := f.RootPEM()
	rootCert, _ := parseCertPEM(root)
	api.FleetRoot = func() *x509.Certificate { return rootCert }
	t.Cleanup(func() { nodeproxy.FleetIdentity, api.FleetRoot = prevID, prevRoot })
	return a, kit, pass
}

// TestRenewTheFleet: a renewal reaches every node over real TLS - each
// with the certificate its bundle lets in -, switches janusctl's
// certificates once they all have it; a node reset to the first bundle
// (its version recorded too high) is brought back with the CA before;
// one whose bundle lists no CA the Controller still has is reached with
// the kit's certificate, else waits for the kit.
func TestRenewTheFleet(t *testing.T) {
	a, kit, pass := fleetApp(t)
	ctx := context.Background()
	alpha, _ := realFleetNode(t, a, "alpha")
	beta, betaFleet := realFleetNode(t, a, "beta")
	rootPEM, bundle1, _, _ := a.fleet.Trust()
	a.trustAll(ctx)
	for _, n := range []*store.Node{alpha, beta} {
		if nt := a.trust.get(n.ID); nt.State != trustTrusted || nt.BundleVersion != 1 {
			t.Fatalf("%s before the renewal: %+v", n.Name, nt)
		}
	}

	if err := a.fleet.Renew(kit, pass, 0); err != nil {
		t.Fatal(err)
	}
	_, _, v2, _ := a.fleet.Trust()
	if behind := a.nodesBehind(); len(behind) != 2 {
		t.Fatalf("behind before the loop: %v", behind)
	}
	a.trustAll(ctx)
	for _, n := range []*store.Node{alpha, beta} {
		if nt := a.trust.get(n.ID); nt.State != trustTrusted || nt.BundleVersion != v2 || n.Bundle() != v2 {
			t.Fatalf("%s after the renewal: %+v, recorded %d", n.Name, nt, n.Bundle())
		}
	}
	st := a.fleet.Status()
	if st.Renewal == nil || st.Renewal.Switched.IsZero() {
		t.Fatalf("not switched with every node on the bundle: %+v", st.Renewal)
	}
	// The next pass reaches them with the new CA's certificate.
	a.trustAll(ctx)
	if nt := a.trust.get(alpha.ID); nt.State != trustTrusted {
		t.Fatalf("alpha with the new CA: %+v", nt)
	}

	// beta reset, given the first bundle again: the version recorded is
	// too high - the CA before brings it back.
	if err := betaFleet.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := betaFleet.Set(rootPEM, bundle1); err != nil {
		t.Fatal(err)
	}
	nodeproxy.Reconnect(beta.ID)
	a.trustAll(ctx)
	if nt := a.trust.get(beta.ID); nt.State != trustTrusted || beta.Bundle() != v2 {
		t.Fatalf("beta reset: %+v, recorded %d", nt, beta.Bundle())
	}

	// A second renewal drops the first CA. beta reset again to the first
	// bundle: reached with the kit's certificate the renewal left.
	if err := a.fleet.Renew(kit, pass, 0); err != nil {
		t.Fatal(err)
	}
	_, _, v3, _ := a.fleet.Trust()
	_ = betaFleet.Reset()
	_ = betaFleet.Set(rootPEM, bundle1)
	nodeproxy.Reconnect(beta.ID)
	_ = a.store.SetFleetBundle(beta.ID, v3)
	a.trustAll(ctx)
	if nt := a.trust.get(beta.ID); nt.State != trustTrusted || beta.Bundle() != v3 {
		t.Fatalf("beta on a bundle the Controller has no CA of, with the kit's certificate: %+v, recorded %d", nt, beta.Bundle())
	}

	// The same once the kit's hour is gone (a restart forgets it): it
	// waits for the kit - then the kit brings it back.
	key, _ := secrets.LoadOrCreate("", a.dataDir)
	reopened, err := fleet.Open(filepath.Join(a.dataDir, "fleet"), key, a.controllerID)
	if err != nil {
		t.Fatal(err)
	}
	a.fleet = reopened
	_ = betaFleet.Reset()
	_ = betaFleet.Set(rootPEM, bundle1)
	nodeproxy.Reconnect(beta.ID)
	a.trustAll(ctx)
	nt := a.trust.get(beta.ID)
	if nt.State != trustNeedsKit || !strings.Contains(nt.Error, "recovery kit") {
		t.Fatalf("beta with no certificate it lets in: %+v", nt)
	}
	if err := a.fleet.Rescue(kit, pass); err != nil {
		t.Fatal(err)
	}
	a.trustAll(ctx)
	if nt := a.trust.get(beta.ID); nt.State != trustTrusted || beta.Bundle() != v3 {
		t.Fatalf("beta after the kit: %+v", nt)
	}
}

// TestFleetRenewRoute: POST /api/fleet/renew takes the kit and its
// passphrase - a wrong one refused, nothing changed -, refuses a renewal
// while the last one waits for its nodes, or while a node still on the
// bundle before it would lose its CA; POST /api/fleet/rescue opens the
// kit for an hour.
func TestFleetRenewRoute(t *testing.T) {
	a, kit, pass := fleetApp(t)
	gamma := &store.Node{Name: "gamma", Address: "192.0.2.30:9505", CACertPEM: []byte("CA"), Fleet: true, FleetBundle: 1}
	if err := a.store.Add(gamma); err != nil {
		t.Fatal(err)
	}
	post := func(path, passphrase string) (int, string) {
		body, _ := json.Marshal(kitRequest{Kit: string(kit), Passphrase: passphrase})
		rec := httptest.NewRecorder()
		h := a.handleFleetRenew
		if path == "rescue" {
			h = a.handleFleetRescue
		}
		h(rec, httptest.NewRequest(http.MethodPost, "/api/fleet/"+path, bytes.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	if code, body := post("renew", "0000-0000-0000-0000-0000-0000"); code != http.StatusBadRequest || a.fleet.Status().Renewal != nil {
		t.Fatalf("a wrong passphrase: %d %s", code, body)
	}
	code, body := post("renew", pass)
	var out struct {
		Renewal     *fleet.Renewal `json:"renewal"`
		NodesBehind []string       `json:"nodes_behind"`
	}
	if _ = json.Unmarshal([]byte(body), &out); code != http.StatusOK || out.Renewal == nil || len(out.NodesBehind) != 1 || out.NodesBehind[0] != "gamma" {
		t.Fatalf("renew: %d %s", code, body)
	}
	if code, body := post("renew", pass); code != http.StatusConflict || !strings.Contains(body, "renewal") {
		t.Errorf("again while it waits for its nodes: %d %s", code, body)
	}
	if switched, _ := a.fleet.Switch(true); !switched {
		t.Fatal("not switched")
	}
	if code, body := post("renew", pass); code != http.StatusConflict || !strings.Contains(body, "gamma") {
		t.Errorf("with a node still on the bundle before: %d %s", code, body)
	}
	if code, body := post("rescue", pass); code != http.StatusOK || !strings.Contains(body, "rescue_until") {
		t.Errorf("rescue: %d %s", code, body)
	}
}
