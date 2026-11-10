package api

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
)

// probeEvents is a Probe stream that keeps what it's sent.
type probeEvents struct {
	grpc.ServerStream
	ctx    context.Context
	events []*janusv1alpha1.ProbeEvent
}

func (p *probeEvents) Context() context.Context { return p.ctx }
func (p *probeEvents) Send(e *janusv1alpha1.ProbeEvent) error {
	p.events = append(p.events, e)
	return nil
}

// TestProbeLocalNeedsHAProxyRights: an operator of the network domain
// alone - through the Controller - may probe, but not the node itself
// nor link-local addresses; whoever may change HAProxy's configuration
// may (security review 2026-11-05 #28).
func TestProbeLocalNeedsHAProxyRights(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ctl := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "janus-controller", Organization: []string{pki.RoleController}}})
	as := func(domains string) context.Context {
		return metadata.NewIncomingContext(ctl, metadata.Pairs(pki.AsUserKey, "net-op", pki.AsRolesKey, pki.RoleOperator, pki.AsDomainsKey, domains))
	}
	connect := func(ctx context.Context) *janusv1alpha1.ProbeEvent {
		t.Helper()
		st := &probeEvents{ctx: ctx}
		if err := (&System{}).Probe(&janusv1alpha1.ProbeRequest{Kind: "tcp", Target: ln.Addr().String(), TimeoutMs: 1000}, st); err != nil {
			t.Fatal(err)
		}
		for _, e := range st.events {
			if e.GetStage() == "connect" {
				return e
			}
		}
		t.Fatalf("no connect step: %v", st.events)
		return nil
	}
	if e := connect(as("network")); e.GetOk() || !strings.Contains(e.GetMessage(), "link-local addresses are for whoever") {
		t.Errorf("an operator of the network domain alone reached the node itself: %v", e)
	}
	if e := connect(as("network,haproxy")); !e.GetOk() {
		t.Errorf("an operator who may change HAProxy's configuration refused: %v", e)
	}
}
