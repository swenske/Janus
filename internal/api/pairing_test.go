package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"slices"
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
	return pairingServer(t, nil)
}

// pairingServer is pairingNode, its Pairing changed by set first.
func pairingServer(t *testing.T, set func(*Pairing)) (string, pairing.Code, *pairing.Window, *pki.Fleet, *pki.CA) {
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
	p := &Pairing{Window: w, Access: &Access{Fleet: fleet, Local: pki.NewLocal(ca)}}
	if set != nil {
		set(p)
	}
	janusv1alpha1.RegisterPairingServiceServer(srv, p)
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
		want := codes.PermissionDenied
		if i == pairing.MaxFailures-1 {
			want = codes.ResourceExhausted // the last wrong one pauses the source
		}
		if _, _, err := pairing.Pair(ctx, addr, wrong.String(), rootPEM, bundle); status.Code(err) != want {
			t.Fatalf("wrong secret %d: %v", i, err)
		}
	}
	// The source is paused: even the right code is turned away, and the
	// window stays open for other sources (security review 2026-11-05 #17).
	if _, _, err := pairing.Pair(ctx, addr, code.String(), rootPEM, bundle); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("the right one from a paused source: %v", err)
	}
	if fleet.Root() != nil {
		t.Error("a fleet was taken")
	}
	if w.Closed() != "" {
		t.Error("a source's failures closed the window for everyone")
	}
}

// TestPairingAuditNotFlooded: once a source is paused, its further wrong
// calls aren't audited, so a flood can't evict the audit (security review
// 2026-11-05 #17).
func TestPairingAuditNotFlooded(t *testing.T) {
	saved := AuditLog
	AuditLog = events.NewLog("audit", AuditCapacity)
	t.Cleanup(func() { AuditLog = saved })
	addr, code, _, _, _ := pairingNode(t)
	rootPEM, bundle := testFleet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < pairing.MaxFailures+5; i++ {
		wrong := pairing.Code{Prefix: code.Prefix, Secret: code.Secret ^ uint64(i+1)}
		_, _, _ = pairing.Pair(ctx, addr, wrong.String(), rootPEM, bundle)
	}
	if evs, _ := AuditLog.Since(0); len(evs) > pairing.MaxFailures {
		t.Errorf("a paused source was audited %d times, want at most %d", len(evs), pairing.MaxFailures)
	}
}

func rescueKey(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// TestRescue: only a rescue volume's window gives a certificate; a
// wrong secret counts; a bad name or key leaves the window open; the
// right call gets an hour's os:admin certificate of the node's CA,
// named rescue:<name>, even on a node trusting a fleet - the window
// closes, OnRescue is called, the same call again gets the same answer,
// another is refused.
func TestRescue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pub := rescueKey(t)

	addr, code, _, _, _ := pairingNode(t)
	if _, _, err := pairing.Rescue(ctx, addr, code.String(), pub, "bob"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "no rescue volume") {
		t.Fatalf("a pairing window: %v", err)
	}

	rescued := 0
	addr, code, w, fleet, ca := pairingServer(t, func(p *Pairing) {
		p.RescueVolume = true
		p.OnRescue = func() { rescued++ }
	})
	rootPEM, bundle := testFleet(t)
	if err := fleet.Set(rootPEM, bundle); err != nil {
		t.Fatal(err)
	}
	wrong := pairing.Code{Prefix: code.Prefix, Secret: code.Secret ^ 1}
	if _, _, err := pairing.Rescue(ctx, addr, wrong.String(), pub, "bob"); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "4 tries left") {
		t.Fatalf("a wrong secret: %v", err)
	}
	for _, c := range []struct {
		pub  []byte
		name string
	}{{pub, ""}, {pub, "bob smith"}, {pub, "-bob"}, {[]byte("not a key"), "bob"}} {
		if _, _, err := pairing.Rescue(ctx, addr, code.String(), c.pub, c.name); status.Code(err) != codes.InvalidArgument {
			t.Errorf("name %q: %v", c.name, err)
		}
	}
	if w.Closed() != "" || rescued != 0 {
		t.Fatal("a refused call closed the window")
	}

	resp, gotCA, err := pairing.Rescue(ctx, addr, code.String(), pub, "bob")
	if err != nil {
		t.Fatalf("rescue: %v", err)
	}
	if !gotCA.Equal(ca.Cert) {
		t.Error("another CA")
	}
	b, _ := pem.Decode(resp.GetCert())
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !pki.IsConsole(cert) {
		t.Error("the rescue certificate lacks the console mark: it couldn't make the node forget its fleet")
	}
	if cert.Subject.CommonName != "rescue:bob" || !slices.Equal(cert.Subject.Organization, []string{pki.RoleAdmin}) || cert.CheckSignatureFrom(ca.Cert) != nil {
		t.Errorf("the certificate: %s %v", cert.Subject.CommonName, cert.Subject.Organization)
	}
	if left := time.Until(cert.NotAfter); left > pairing.RescueValidity || left < pairing.RescueValidity-time.Minute {
		t.Errorf("valid %s more", left)
	}
	if w.Closed() != "rescued" || rescued != 1 {
		t.Errorf("after: window %q, OnRescue %d", w.Closed(), rescued)
	}
	if again, _, err := pairing.Rescue(ctx, addr, code.String(), pub, "bob"); err != nil || string(again.GetCert()) != string(resp.GetCert()) {
		t.Errorf("the same call again: %v", err)
	}
	if _, _, err := pairing.Rescue(ctx, addr, code.String(), rescueKey(t), "eve"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("another call after: %v", err)
	}
	if rescued != 1 {
		t.Errorf("OnRescue called %d times", rescued)
	}
}
