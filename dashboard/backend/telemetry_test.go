package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/dashboard/backend/internal/telemetry"
)

func withTelemetry(t *testing.T, a *authApp) {
	t.Helper()
	dir := t.TempDir()
	var err error
	if a.hypervisors, err = hypervisor.Open(dir); err != nil {
		t.Fatal(err)
	}
	if a.telemetry, err = telemetry.Open(dir, false); err != nil {
		t.Fatal(err)
	}
	a.telemetryURL = "http://127.0.0.1:1/v1/report"
}

// The report counts nodes and machines, and names none: no name, no
// address, no label, no machine's name - whatever the Controller holds.
func TestTelemetryReportNamesNothing(t *testing.T) {
	a := newAuthApp(t)
	withTelemetry(t, a)
	if err := a.telemetry.SetEnabled(true, time.Now()); err != nil {
		t.Fatal(err)
	}
	node := &store.Node{Name: "edge-paris-7", Address: "198.51.100.77:9505", Labels: map[string]string{"site": "dc-secret"}}
	if err := a.store.Add(node); err != nil {
		t.Fatal(err)
	}
	if err := a.machines.Add(&machines.Machine{Spec: machines.Spec{Name: "edge-lyon-3", HypervisorID: "hv-1"}}); err != nil {
		t.Fatal(err)
	}
	r := a.telemetryReport(context.Background())
	if r.Nodes.Total != 1 || r.Nodes.Unreachable != 1 || r.Platforms.Added != 1 || r.InstallID == "" || r.Schema != telemetry.Schema {
		t.Errorf("report %+v", r)
	}
	data, _ := json.Marshal(r)
	for _, secret := range []string{"edge-paris-7", "198.51.100.77", "dc-secret", "site", "edge-lyon-3", "hv-1", node.ID} {
		if strings.Contains(string(data), secret) {
			t.Errorf("the report holds %q: %s", secret, data)
		}
	}
	if r.Features.MFARequired {
		t.Error("mfa_required with newAuthApp's policy, \"nobody\"")
	}
}

// Off until turned on - at setup, or by an admin - and the environment
// can lock it off.
func TestTelemetrySettings(t *testing.T) {
	a := newAuthApp(t)
	withTelemetry(t, a)
	root := a.login(t, "root", "root-password")
	var v telemetryView
	if code, body := a.req(t, "GET", "/api/telemetry", root, nil); code != http.StatusOK || json.Unmarshal([]byte(body), &v) != nil || v.Enabled || v.Report == nil {
		t.Fatalf("get: %d %s", code, body)
	}
	if code, body := a.req(t, "PUT", "/api/telemetry", root, map[string]bool{"enabled": true}); code != http.StatusOK || json.Unmarshal([]byte(body), &v) != nil || !v.Enabled || v.InstallID == "" || v.Next == nil {
		t.Fatalf("on: %d %s", code, body)
	}
	id := v.InstallID
	if code, body := a.req(t, "POST", "/api/telemetry/regenerate", root, nil); code != http.StatusOK || json.Unmarshal([]byte(body), &v) != nil || v.InstallID == id || v.InstallID == "" {
		t.Fatalf("regenerate: %d %s", code, body)
	}
	// Nothing listens at the receiver: said, and recorded.
	if code, _ := a.req(t, "POST", "/api/telemetry/send", root, nil); code != http.StatusBadGateway {
		t.Errorf("send to nowhere: %d", code)
	}
	if st := a.telemetry.Get(); st.LastError == "" || st.Failures != 1 {
		t.Errorf("state after a failure: %+v", st)
	}

	a.telemetry.Locked = true
	if code, _ := a.req(t, "PUT", "/api/telemetry", root, map[string]bool{"enabled": true}); code != http.StatusConflict {
		t.Errorf("turned on while locked: %d", code)
	}
	if a.telemetry.Get().Enabled || a.telemetry.Due(time.Now().Add(48*time.Hour)) {
		t.Error("locked, yet on")
	}
}

// Setup can turn it on, and only when asked.
func TestSetupTurnsTelemetryOn(t *testing.T) {
	for _, on := range []bool{false, true} {
		dir := t.TempDir()
		authStore, err := auth.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		ts, err := telemetry.Open(dir, false)
		if err != nil {
			t.Fatal(err)
		}
		a := &app{auth: authStore, telemetry: ts}
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"name":"root","password":"a-long-password-1","mfa_required":"nobody","telemetry":%v}`, on)
		a.handleAuthSetup(rec, httptest.NewRequest("POST", "/api/auth/setup", strings.NewReader(body)))
		if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
			t.Fatalf("setup: %d %s", rec.Code, rec.Body)
		}
		if got := ts.Get().Enabled; got != on {
			t.Errorf("setup with telemetry %v: on = %v", on, got)
		}
	}
}
