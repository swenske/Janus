// The usage report (internal/telemetry, docs/guide/telemetry.md): off
// until an admin turns it on - at setup or on the Settings page - then
// once a day.
package main

import (
	"context"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/fleet"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/telemetry"
	"github.com/swenske/Janus/internal/clusters"
)

// telemetryReport is what would be sent now.
func (a *app) telemetryReport(ctx context.Context) *telemetry.Report {
	r := telemetry.NewReport(a.telemetry.Get().InstallID)
	r.Controller = telemetry.Controller{Version: nodeproxy.ControllerVersion, Arch: runtime.GOARCH}
	if rel, err := nodeproxy.LatestRelease(ctx); err == nil {
		r.Controller.Prerelease = rel.IsPrerelease(nodeproxy.ControllerVersion)
	}

	nodes := a.store.List()
	samples := make([]*telemetry.Node, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			if s, err := nodeproxy.TelemetryNode(nctx, n); err == nil {
				samples[i] = s
			}
		}()
	}
	wg.Wait()
	for _, s := range samples {
		r.AddNode(s)
	}

	kinds := map[string]string{}
	for _, h := range a.hypervisors.List() {
		kinds[h.ID] = string(h.Kind)
	}
	made := map[string]bool{}
	for _, m := range a.machines.List() {
		made[m.ID] = true
		if k := kinds[m.Spec.HypervisorID]; k != "" {
			r.Platforms.Machines[k]++
		}
		if as := m.Spec.Autoscale; as != nil && as.Mode == machines.AutoscaleOn {
			r.Features.Autoscaling = true
		}
	}
	for _, n := range nodes {
		if n.MachineID == "" || !made[n.MachineID] {
			r.Platforms.Added++
		}
	}
	if a.clusters != nil {
		for _, c := range a.clusters.Store.List() {
			r.AddCluster(len(c.Members))
			r.Features.ClusterBGP = r.Features.ClusterBGP || c.Manages("bgp")
			r.Features.ClusterVRRP = r.Features.ClusterVRRP || c.Manages("vrrp")
			if c.Autoscale != nil && c.Autoscale.Mode == clusters.ScaleOn {
				r.Features.Autoscaling = true
			}
		}
	}

	f := &r.Features
	if a.sso != nil {
		for _, p := range a.sso.store.List() {
			f.SSO = f.SSO || p.Enabled
		}
	}
	f.MFARequired = a.auth.Settings().MFARequired != "nobody"
	if a.logfwd != nil {
		st := a.logfwd.store.Settings()
		f.LogForwarding = len(st.Controller.Targets) > 0 || len(st.Profiles) > 0
	}
	f.LetsEncrypt = a.relay != nil && a.relay.Status().Config.Enabled || a.ordering != nil && len(a.ordering.store.Nodes()) > 0
	f.S3Backups = a.backups != nil && a.backups.Status().Settings.Enabled
	f.Fleet = a.fleet != nil && a.fleet.Status().State == fleet.StateReady
	f.Consul = a.consul != nil && a.consul.store.Settings().Enabled
	return r
}

// sendTelemetry builds and sends a report, and records how it went.
func (a *app) sendTelemetry(ctx context.Context) error {
	r := a.telemetryReport(ctx)
	err := telemetry.Send(ctx, a.telemetryURL, r)
	if rerr := a.telemetry.Recorded(time.Now(), err); rerr != nil {
		log.Printf("telemetry: %v", rerr)
	}
	if err != nil {
		log.Printf("telemetry: the report wasn't sent: %v", err)
		return err
	}
	log.Printf("telemetry: report sent (%d nodes, %d clusters)", r.Nodes.Total, r.Clusters.Count)
	return nil
}

// telemetryLoop sends the reports when they're due.
func (a *app) telemetryLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if a.telemetry.Due(time.Now()) {
			sctx, cancel := context.WithTimeout(ctx, time.Minute)
			_ = a.sendTelemetry(sctx)
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// --- /api/telemetry (admin) ---

func (a *app) registerTelemetryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/telemetry", a.sessionGate(auth.Admin, auth.Admin, a.handleTelemetryGet))
	mux.HandleFunc("PUT /api/telemetry", a.sessionGate(auth.Admin, auth.Admin, a.handleTelemetrySet))
	mux.HandleFunc("POST /api/telemetry/regenerate", a.sessionGate(auth.Admin, auth.Admin, a.handleTelemetryRegenerate))
	mux.HandleFunc("POST /api/telemetry/send", a.sessionGate(auth.Admin, auth.Admin, a.handleTelemetrySend))
}

// telemetryView is GET /api/telemetry: the state, where reports go, and
// the report that would be sent now - exactly.
type telemetryView struct {
	telemetry.State
	Locked bool              `json:"locked"`
	URL    string            `json:"url"`
	Next   *time.Time        `json:"next,omitempty"`
	Report *telemetry.Report `json:"report"`
}

func (a *app) telemetryView(ctx context.Context) telemetryView {
	v := telemetryView{State: a.telemetry.Get(), Locked: a.telemetry.Locked, URL: a.telemetryURL, Report: a.telemetryReport(ctx)}
	if next := a.telemetry.Next(); !next.IsZero() {
		v.Next = &next
	}
	return v
}

func (a *app) handleTelemetryGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.telemetryView(r.Context()))
}

func (a *app) handleTelemetrySet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if err := a.telemetry.SetEnabled(req.Enabled, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, a.telemetryView(r.Context()))
}

// handleTelemetryRegenerate gives the Controller a new ID, and asks the
// receiver to forget the old one's reports - best effort: a receiver
// that can't be reached keeps them, under an ID nothing sends again.
func (a *app) handleTelemetryRegenerate(w http.ResponseWriter, r *http.Request) {
	old, err := a.telemetry.Regenerate()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if old != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		if err := telemetry.Forget(ctx, a.telemetryURL, old); err != nil {
			log.Printf("telemetry: the receiver wasn't asked to forget the previous ID: %v", err)
		}
		cancel()
	}
	writeJSON(w, http.StatusOK, a.telemetryView(r.Context()))
}

func (a *app) handleTelemetrySend(w http.ResponseWriter, r *http.Request) {
	if !a.telemetry.Get().Enabled {
		http.Error(w, "telemetry is off", http.StatusConflict)
		return
	}
	if err := a.sendTelemetry(r.Context()); err != nil {
		http.Error(w, "the report wasn't sent: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, a.telemetryView(r.Context()))
}
