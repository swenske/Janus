package api

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/pairing"
	"github.com/swenske/Janus/internal/pki"
)

// pairingNode is a node's pairing window on a real TLS listener: its
// address, its code, its window, its fleet and its CA.
func pairingNode(t *testing.T) (string, pairing.Code, *pairing.Window, *pki.Fleet, *pki.CA) {
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
	w := pairing.NewWindow(code.SecretBytes(), 0)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(pairing.ServerTLS(leaf))), grpc.UnaryInterceptor(PairingInterceptor))
	janusv1alpha1.RegisterPairingServiceServer(srv, &Pairing{Window: w, Access: &Access{Fleet: fleet, Local: pki.NewLocal(ca)}})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), code, w, fleet, ca
}

func testFleet(t *testing.T) (rootPEM, bundle []byte) {
	t.Helper()
	root, err := pki.NewCAFor("root", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	issuing, err := root.IssueCA("issuing", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := pki.SignBundle(root, pki.Bundle{Version: 1, Issued: time.Now().UTC(), IssuingCAs: []string{string(issuing.CertPEM)}})
	if err != nil {
		t.Fatal(err)
	}
	return root.CertPEM, signed
}

// TestPairing: a client checks the window's certificate against the
// code before anything is sent; a wrong secret counts against the
// window; a refused fleet leaves it open; the right one pairs the node -
// its fleet set, its CA answered and verified, the window closed; the
// same call again gets the same answer, another is refused; every call
// is audited.
func TestPairing(t *testing.T) {
	saved := AuditLog
	AuditLog = events.NewLog("audit", AuditCapacity)
	t.Cleanup(func() { AuditLog = saved })
	addr, code, w, fleet, ca := pairingNode(t)
	rootPEM, bundle := testFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Another certificate's code: refused by the client, nothing sent.
	other := pairing.Code{Prefix: code.Prefix ^ 1, Secret: code.Secret}
	if _, _, err := pairing.Pair(ctx, addr, other.String(), rootPEM, bundle); err == nil || !strings.Contains(err.Error(), "isn't the one its code names") {
		t.Fatalf("another certificate's code: %v", err)
	}
	if evs, _ := AuditLog.Since(0); len(evs) != 0 {
		t.Errorf("a call reached the node: %d audited", len(evs))
	}

	// A wrong secret: counted.
	wrong := pairing.Code{Prefix: code.Prefix, Secret: code.Secret ^ 1}
	if _, _, err := pairing.Pair(ctx, addr, wrong.String(), rootPEM, bundle); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "4 tries left") {
		t.Fatalf("a wrong secret: %v", err)
	}

	// The right secret, a bundle that isn't: the window stays open.
	if _, _, err := pairing.Pair(ctx, addr, code.String(), rootPEM, []byte(`{"payload":"x"}`)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a bad bundle: %v", err)
	}
	if w.Closed() != "" || fleet.Root() != nil {
		t.Fatal("a refused fleet closed the window or was taken")
	}

	resp, gotCA, err := pairing.Pair(ctx, addr, code.String(), rootPEM, bundle)
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	if !gotCA.Equal(ca.Cert) || resp.GetTrust().GetBundleVersion() != 1 || fleet.Root() == nil {
		t.Errorf("paired: CA %v, bundle %d, root %v", gotCA.Subject, resp.GetTrust().GetBundleVersion(), fleet.Root())
	}
	if w.Closed() != "paired" {
		t.Errorf("the window: %q", w.Closed())
	}
	// Its answer lost, the same call comes again: the same answer.
	if again, _, err := pairing.Pair(ctx, addr, code.String(), rootPEM, bundle); err != nil || again.GetTrust().GetBundleVersion() != 1 {
		t.Errorf("the same call again: %v", err)
	}
	// Another one: the window is closed.
	otherRoot, otherBundle := testFleet(t)
	if _, _, err := pairing.Pair(ctx, addr, code.String(), otherRoot, otherBundle); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "closed") {
		t.Errorf("after pairing: %v", err)
	}

	evs, _ := AuditLog.Since(0)
	var codesSeen []string
	for _, e := range evs {
		var a AuditEntry
		if err := json.Unmarshal(e.Payload, &a); err != nil {
			t.Fatal(err)
		}
		if e.Type != "PairingService/Pair" || a.Caller != "pairing code" || a.Peer == "" {
			t.Errorf("audited %s %+v", e.Type, a)
		}
		codesSeen = append(codesSeen, a.Code)
	}
	if strings.Join(codesSeen, ",") != "PermissionDenied,InvalidArgument,OK,OK,FailedPrecondition" {
		t.Errorf("audited %v", codesSeen)
	}
}

// TestPairingTries: five wrong secrets close the window - the right one
// is refused then.
func TestPairingTries(t *testing.T) {
	addr, code, w, fleet, _ := pairingNode(t)
	rootPEM, bundle := testFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range pairing.MaxFailures {
		wrong := pairing.Code{Prefix: code.Prefix, Secret: code.Secret ^ uint64(i+1)}
		if _, _, err := pairing.Pair(ctx, addr, wrong.String(), rootPEM, bundle); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("wrong secret %d: %v", i, err)
		}
	}
	if _, _, err := pairing.Pair(ctx, addr, code.String(), rootPEM, bundle); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("the right one after %d wrong: %v", pairing.MaxFailures, err)
	}
	if fleet.Root() != nil || w.Closed() == "" {
		t.Error("the window isn't closed, or a fleet was taken")
	}
}
