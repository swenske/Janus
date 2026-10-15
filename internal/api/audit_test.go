package api

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/pki"
)

// The audit keeps each call that changes something once it's done, with
// how it ended, and each call refused - not the reads.
func TestAuditKeepsChangesAndRefusals(t *testing.T) {
	saved := AuditLog
	AuditLog = events.NewLog("audit", AuditCapacity)
	t.Cleanup(func() { AuditLog = saved })

	admin := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "alice", Organization: []string{"os:admin"}}})
	reader := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "rita", Organization: []string{"os:reader"}}})
	ctl := peerWith(&x509.Certificate{Subject: pkix.Name{CommonName: "janus-controller", Organization: []string{pki.RoleController}}})
	viaCtl := metadata.NewIncomingContext(ctl, metadata.Pairs(pki.AsUserKey, "carol", pki.AsRolesKey, "os:reader"))
	call := func(ctx context.Context, method string, result error) error {
		_, err := UnaryAuthInterceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/janus.v1alpha1." + method}, func(context.Context, any) (any, error) {
			return nil, result
		})
		return err
	}
	_ = call(admin, "SystemService/MetricsConfigSet", nil)
	_ = call(admin, "HAProxyService/ApplyConfig", status.Error(codes.InvalidArgument, "line 3: unknown keyword"))
	_ = call(admin, "HAProxyService/ShowInfo", nil) // a read: not kept
	if err := call(reader, "SystemService/Reboot", nil); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a reader rebooting: %v", err)
	}
	_ = call(viaCtl, "SystemService/Reset", nil)

	evs, _ := AuditLog.Since(0)
	if len(evs) != 4 {
		t.Fatalf("%d entries, want 4: %+v", len(evs), evs)
	}
	var got []AuditEntry
	for _, e := range evs {
		var a AuditEntry
		if err := json.Unmarshal(e.Payload, &a); err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
	}
	if evs[0].Type != "SystemService/MetricsConfigSet" || got[0].Caller != "alice" || got[0].Code != "OK" || got[0].Denied || got[0].Peer == "" {
		t.Errorf("a change: %s %+v", evs[0].Type, got[0])
	}
	if evs[1].Type != "HAProxyService/ApplyConfig" || got[1].Code != "InvalidArgument" || got[1].Error != "line 3: unknown keyword" {
		t.Errorf("a change refused by the handler: %+v", got[1])
	}
	if evs[2].Type != "SystemService/Reboot" || !got[2].Denied || got[2].Caller != "rita" || got[2].Code != "PermissionDenied" || len(got[2].Roles) != 1 {
		t.Errorf("a refusal: %+v", got[2])
	}
	if evs[3].Type != "SystemService/Reset" || !got[3].Denied || got[3].Caller != "carol" || got[3].Via != "janus-controller" {
		t.Errorf("a refusal through the Controller: %+v", got[3])
	}
}
