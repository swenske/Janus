package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/clusters"
)

// journalSays reports whether cluster id's journal has an entry with s.
func journalSays(t *testing.T, a *app, id, s string) bool {
	t.Helper()
	return slices.ContainsFunc(a.clusters.Store.Entries(id, 500), func(e clusters.Entry) bool { return strings.Contains(e.Message, s) })
}

// The primitive: a member more is a new machine like the newest one's,
// placed; a member less the newest, out, its machine destroyed.
func TestClusterScale(t *testing.T) {
	x := newHVHarness(t)
	a := x.a
	x.fakes[x.h2.ID].hostID = "libvirt:bbbb"
	scale := func(id string, n int) *httpResult {
		rec := call(t, a.handleClusterScale, "POST", "/api/clusters/"+id+"/scale", "POST /api/clusters/{id}/scale", map[string]int{"members": n})
		return &httpResult{rec.Code, rec.Body.String()}
	}

	// Required, two hosts, two members: no host left for a third.
	c := x.build("edge", x.request(""))
	if r := scale(c.ID, 3); r.code != http.StatusAccepted {
		t.Fatalf("scale: %d %s", r.code, r.body)
	}
	if c = x.idle(c.ID); !strings.Contains(c.Error, "anti-affinity") || len(c.Members) != 2 {
		t.Errorf("a third on two hosts, required: %q, %d members", c.Error, len(c.Members))
	}

	// Preferred: the third shares a host; then back to two.
	c = x.build("edgep", x.request(clusters.AntiAffinityPreferred))
	c, _ = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		for i := range c.Members {
			c.Members[i].Vars = map[string]string{"priority": "100"}
		}
		return nil
	})
	if r := scale(c.ID, 3); r.code != http.StatusAccepted {
		t.Fatalf("scale: %d %s", r.code, r.body)
	}
	third := x.machine("edgep-3")
	x.register(third)
	c = x.idle(c.ID)
	// The journal names the hypervisor, not its record's id.
	if !journalSays(t, a, c.ID, "a member more: edgep-3, a new machine like") || !journalSays(t, a, c.ID, ", on kvm01") {
		t.Errorf("the scale-out's journal: %v", a.clusters.Store.Entries(c.ID, 50))
	}
	if c.Error != "" || len(c.Members) != 3 || c.Member("edgep-3") == nil || c.Member("edgep-3").Vars["priority"] != "100" {
		t.Fatalf("scaled out: %q, %+v", c.Error, c.Members)
	}
	if r := scale(c.ID, 2); r.code != http.StatusAccepted {
		t.Fatalf("scale: %d %s", r.code, r.body)
	}
	c = x.idle(c.ID)
	if c.Error != "" || len(c.Members) != 2 || c.Member("edgep-3") != nil {
		t.Fatalf("scaled in: %q, %+v", c.Error, c.Members)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, ok := a.machines.Get(third.ID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("edgep-3's machine never destroyed")
		}
	}

	// The policy: refused on a cluster made from nodes, or one with VRRP.
	put := func(id string, p clusters.Autoscale) *httpResult {
		rec := call(t, a.handleClusterAutoscale, "PUT", "/api/clusters/"+id+"/autoscale", "PUT /api/clusters/{id}/autoscale", p)
		return &httpResult{rec.Code, rec.Body.String()}
	}
	plain, err := a.clusters.Store.Create("plain", 0, []string{"haproxy", "files"})
	if err != nil {
		t.Fatal(err)
	}
	if r := put(plain.ID, clusters.Autoscale{Mode: clusters.ScaleObserve, MinMembers: 2, MaxMembers: 4}); r.code != http.StatusBadRequest || !strings.Contains(r.body, "hypervisors only") {
		t.Errorf("from nodes: %d %s", r.code, r.body)
	}
	if r := scale(plain.ID, 3); r.code != http.StatusBadRequest {
		t.Errorf("scale from nodes: %d %s", r.code, r.body)
	}
	_, _ = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error { c.Managed = append(c.Managed, "vrrp"); return nil })
	if r := put(c.ID, clusters.Autoscale{Mode: clusters.ScaleOn, MinMembers: 2, MaxMembers: 4}); r.code != http.StatusBadRequest || !strings.Contains(r.body, "VRRP") {
		t.Errorf("VRRP: %d %s", r.code, r.body)
	}
	_, _ = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error { c.Managed = []string{"haproxy", "files"}; return nil })
	if r := put(c.ID, clusters.Autoscale{Mode: clusters.ScaleOn, MinMembers: 2, MaxMembers: 4}); r.code != http.StatusOK {
		t.Fatalf("on: %d %s", r.code, r.body)
	}
	// Nor is VRRP brought in under auto-scaling (v2026.10.24-5's lab: it
	// went through, auto-scaling on over a VRRP cluster).
	apply := func(managed []string) *httpResult {
		rec := call(t, a.handleClusterApply, "POST", "/api/clusters/"+c.ID+"/apply", "POST /api/clusters/{id}/apply", map[string]any{"version": 1, "managed": managed})
		return &httpResult{rec.Code, rec.Body.String()}
	}
	if r := apply([]string{"haproxy", "files", "vrrp"}); r.code != http.StatusConflict || !strings.Contains(r.body, "set it off first") {
		t.Errorf("VRRP under auto-scaling: %d %s", r.code, r.body)
	}
	if cur, _ := a.clusters.Store.Get(c.ID); slices.Contains(cur.Managed, "vrrp") {
		t.Error("the refused apply changed what the cluster manages")
	}
	if r := apply([]string{"haproxy", "files"}); r.code != http.StatusAccepted {
		t.Errorf("an apply without VRRP: %d %s", r.code, r.body)
	}
	x.idle(c.ID)
	// Members set from outside while on: the engine pauses, and says so.
	if r := scale(c.ID, 2); r.code != http.StatusAccepted {
		t.Fatalf("scale: %d %s", r.code, r.body)
	}
	c = x.idle(c.ID)
	if c.Autoscale.PausedBy != "root" || !journalSays(t, a, c.ID, "auto-scaling paused: root set the members to 2") {
		t.Errorf("paused: %+v", c.Autoscale)
	}
	if r := put(c.ID, clusters.Autoscale{Mode: clusters.ScaleOn, MinMembers: 2, MaxMembers: 4}); r.code != http.StatusOK {
		t.Fatal(r.body)
	} else if c, _ = a.clusters.Store.Get(c.ID); c.Autoscale.PausedBy != "" {
		t.Error("setting the policy again doesn't resume it")
	}
}

type httpResult struct {
	code int
	body string
}

// busyNodes answer every node's load as busy: HAProxy 10 % idle, the
// CPUs 90 % busy.
type busyNodes struct {
	answeringNodes
	mu    sync.Mutex
	ticks map[string]uint64
}

func (b *busyNodes) Load(_ context.Context, node *store.Node) (*nodeproxy.Load, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ticks[node.ID] += 1000
	t := b.ticks[node.ID]
	return &nodeproxy.Load{CPUs: 2, CPUTotalTicks: t, CPUIdleTicks: t / 10, IdlePercent: 10, CurrConns: 10, MaxConn: 1000}, nil
}

// The engine: observing, it journals what it would do and simulates it;
// on, it acts - once, then cools down; paused, it says so and waits.
func TestClusterScaler(t *testing.T) {
	x := newHVHarness(t)
	a := x.a
	x.fakes[x.h2.ID].hostID = "libvirt:bbbb"
	c := x.build("edge", x.request(""))
	prev := nodes
	nodes = &busyNodes{ticks: map[string]uint64{}}
	t.Cleanup(func() { nodes = prev })
	s := newClusterScaler(a)
	a.clusterScaler = s
	var mu sync.Mutex
	var acted []string
	s.act = func(_ context.Context, _, action string) error {
		mu.Lock()
		defer mu.Unlock()
		acted = append(acted, action)
		return nil
	}
	set := func(mode string) {
		t.Helper()
		rec := call(t, a.handleClusterAutoscale, "PUT", "/api/clusters/"+c.ID+"/autoscale", "PUT /api/clusters/{id}/autoscale", clusters.Autoscale{Mode: mode, MinMembers: 2, MaxMembers: 4})
		if rec.Code != http.StatusOK {
			t.Fatalf("set %s: %d %s", mode, rec.Code, rec.Body)
		}
	}
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	run := func(n int) {
		for range n {
			now = now.Add(clusters.ScaleSampleEvery)
			s.tick(context.Background(), now)
			x.idle(c.ID)
		}
	}

	// No policy: the load is read all the same - the view's and the
	// Controller's metrics -, nothing judged, nothing journaled.
	entries := len(a.clusters.Store.Entries(c.ID, 1000))
	run(2)
	if cur, _ := a.clusters.Store.Get(c.ID); a.clusterView(cur).Load == nil || a.clusterView(cur).Load.Members != 2 {
		t.Errorf("no policy, the load: %+v", a.clusterView(cur).Load)
	}
	if len(a.clusters.Store.Entries(c.ID, 1000)) != entries || a.clusters.Store.State(c.ID).ScaleStatus != "" {
		t.Error("no policy, yet something was judged")
	}
	m := scrapeMetrics(t, a)
	for _, want := range []string{
		`janus_cluster_members{cluster_id="` + c.ID + `",cluster_name="edge"} 2`,
		`janus_cluster_load_members{cluster_id="` + c.ID + `",cluster_name="edge"} 2`,
		`janus_cluster_load_haproxy_idle_ratio{cluster_id="` + c.ID + `",cluster_name="edge"} 0.1`,
		`janus_cluster_load_cpu_busy_ratio{cluster_id="` + c.ID + `",cluster_name="edge"} 0.9`,
		`janus_cluster_load_connections_ratio{cluster_id="` + c.ID + `",cluster_name="edge"} 0.01`,
		`janus_cluster_operation_running{cluster_id="` + c.ID + `",cluster_name="edge"} 0`,
		`janus_controller_build_info{version="dev"} 1`,
	} {
		if !strings.Contains(m, want+"\n") {
			t.Errorf("metrics without %s:\n%s", want, m)
		}
	}
	if strings.Contains(m, "janus_cluster_autoscale_mode{") {
		t.Errorf("an auto-scaling mode without a policy:\n%s", m)
	}

	set(clusters.ScaleObserve)
	run(25)
	if m := scrapeMetrics(t, a); !strings.Contains(m, `janus_cluster_autoscale_mode{cluster_id="`+c.ID+`",cluster_name="edge",mode="observe"} 1`+"\n") ||
		!strings.Contains(m, `janus_cluster_autoscale_judged_members{cluster_id="`+c.ID+`",cluster_name="edge"} 3`+"\n") ||
		!strings.Contains(m, `janus_cluster_autoscale_paused{cluster_id="`+c.ID+`",cluster_name="edge"} 0`+"\n") {
		t.Errorf("observing, the metrics:\n%s", m)
	}
	if st := a.clusters.Store.State(c.ID); st.Scale.Members != 3 || !strings.HasPrefix(st.ScaleStatus, "observing") {
		t.Errorf("observing: %+v %q", st.Scale, st.ScaleStatus)
	}
	if !journalSays(t, a, c.ID, "would add a member (2 → 3)") || len(acted) != 0 {
		t.Errorf("observing: acted %v", acted)
	}
	cur, _ := a.clusters.Store.Get(c.ID)
	if v := a.clusterView(cur); v.Load == nil || v.Load.Members != 2 || v.Load.BusyPercent < 89 || v.Load.IdlePercent != 10 {
		t.Errorf("the load shown: %+v", v.Load)
	}

	set(clusters.ScaleOn)
	_, _ = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error { c.Autoscale.PausedBy = "root"; return nil }) // as POST .../scale does
	run(25)
	if len(acted) != 0 || !journalSays(t, a, c.ID, "paused since root set the members") {
		t.Errorf("paused: acted %v", acted)
	}

	set(clusters.ScaleOn)
	run(50)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(acted, []string{clusters.ScaleOut}) || !journalSays(t, a, c.ID, "adding a member (2 → 3)") {
		t.Errorf("on: acted %v", acted)
	}
	mu.Unlock()

	// A cluster managing VRRP with auto-scaling on (made before the
	// refusals): the engine says so and never steps.
	_, _ = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error { c.Managed = append(c.Managed, "vrrp"); return nil })
	s.forget(c.ID)
	_ = a.clusters.Store.UpdateState(c.ID, func(st *clusters.State) { st.Scale = clusters.ScaleState{} })
	run(60)
	mu.Lock()
	if len(acted) != 1 || !journalSays(t, a, c.ID, "refused: a VRRP cluster can't scale") {
		t.Errorf("VRRP: acted %v, status %q", acted, a.clusters.Store.State(c.ID).ScaleStatus)
	}
}

// scrapeMetrics is GET /api/metrics's body.
func scrapeMetrics(t *testing.T, a *app) string {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleMetrics(rec, httptest.NewRequest("GET", "/api/metrics", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("GET /api/metrics: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	return rec.Body.String()
}
