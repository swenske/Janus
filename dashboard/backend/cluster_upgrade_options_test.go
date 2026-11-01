package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/clusters"
)

// confirmingHook is a webhook with confirm on cluster c, taking members
// in and out after lag check calls.
func confirmingHook(t *testing.T, a *app, f *clusterFake, c *clusters.Cluster, lag int) *testBalancer {
	t.Helper()
	prevRetries := clusters.BalancerRetries
	clusters.BalancerRetries = []time.Duration{time.Millisecond}
	t.Cleanup(func() { clusters.BalancerRetries = prevRetries })
	hook := &testBalancer{f: f, confirm: true, lag: lag, stuck: map[string]bool{}}
	srv := httptest.NewTLSServer(hook)
	t.Cleanup(srv.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	if err := a.clusters.SetBalancer(context.Background(), c.ID, &clusters.Balancer{URL: srv.URL + "/hook", CACert: ca, Confirm: true}, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	return hook
}

// An update that waits for the balancer: a member drained goes only once
// the balancer has it out, the next one only once the balancer has the
// one before back in service - asked with check calls.
func TestClusterUpgradeWaitsForTheBalancer(t *testing.T) {
	a, f, c, opts, _ := upgradeApp(t)
	ctx := context.Background()
	opts.Members = []string{"lb-1", "lb-2"}
	opts.WaitBalancer = true
	hook := confirmingHook(t, a, f, c, 2)
	n := hook.count()
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, call := range hook.since(n) {
		if len(events) == 0 || events[len(events)-1] != call.Event+" "+call.Member {
			events = append(events, call.Event+" "+call.Member)
		}
	}
	// lb-1 goes once lb-2 and lb-3 are in service; it is out before it
	// updates, back in before lb-2 goes.
	want := []string{"check lb-2", "check lb-3", "out lb-1", "check lb-1", "in lb-1", "check lb-1", "check lb-3", "out lb-2", "check lb-2", "in lb-2", "check lb-2"}
	if !slices.Equal(events, want) {
		t.Errorf("the balancer's calls:\n %v\nwant\n %v", events, want)
	}
	if !upgradeJournal(a, c.ID, "the balancer has lb-1 out of service after") || !upgradeJournal(a, c.ID, "the balancer has lb-1 in service after") ||
		!upgradeJournal(a, c.ID, "the balancer asked (the webhook's answers to check calls)") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 30))
	}

	// One the balancer never takes back stops the update there.
	prev := clusters.BalancerTimeout
	clusters.BalancerTimeout = 50 * time.Millisecond
	t.Cleanup(func() { clusters.BalancerTimeout = prev })
	for _, fm := range f.nodes {
		fm.version, fm.slot, fm.boot = "v1", "A", time.Now().Add(-time.Hour)
	}
	hook.mu.Lock()
	hook.stuck["lb-1"] = true
	hook.mu.Unlock()
	mark := f.mark()
	err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin")
	if err == nil || !strings.Contains(err.Error(), "lb-1 is back in service, but the balancer doesn't have lb-1 in service after 50ms") {
		t.Fatalf("never taken back: %v", err)
	}
	if slices.Contains(f.callsSince(mark), "lb-2 ClusterDrain") {
		t.Error("lb-2 drained anyway")
	}
	// lb-1 is out of the balancer now: lb-2 can't go.
	opts.Members = []string{"lb-2"}
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err == nil || !strings.Contains(err.Error(), "the balancer doesn't send lb-1 traffic") {
		t.Errorf("lb-2 with lb-1 out of the balancer: %v", err)
	}
}

// Waiting for a balancer that can't say is refused before anything is
// touched; a pause between members is journaled before each member
// updated after another.
func TestClusterUpgradeOptions(t *testing.T) {
	a, f, c, opts, updated := upgradeApp(t)
	ctx := context.Background()
	opts.WaitBalancer = true
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err == nil || !strings.Contains(err.Error(), "waiting for the balancer: the cluster has no balancer hook to ask") {
		t.Errorf("no hook: %v", err)
	}
	prevRetries := clusters.BalancerRetries
	clusters.BalancerRetries = []time.Duration{time.Millisecond}
	t.Cleanup(func() { clusters.BalancerRetries = prevRetries })
	srv := httptest.NewTLSServer(&testBalancer{f: f})
	t.Cleanup(srv.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	if err := a.clusters.SetBalancer(ctx, c.ID, &clusters.Balancer{URL: srv.URL + "/hook", CACert: ca}, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err == nil || !strings.Contains(err.Error(), "its webhook doesn't answer check calls") {
		t.Errorf("a webhook without confirm: %v", err)
	}
	if len(*updated) > 0 {
		t.Errorf("refused, but updated %v", *updated)
	}
	opts.WaitBalancer, opts.Settle = false, time.Hour
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err == nil || !strings.Contains(err.Error(), "the pause between members: 0 to 30m0s") {
		t.Errorf("an hour's pause: %v", err)
	}
	// The middle member already on the target: one pause, before the
	// last one.
	opts.Settle = 20 * time.Millisecond
	f.nodes["lb-3"].version, f.nodes["lb-3"].slot = "v2", "B"
	opts.Members = []string{"lb-1", "lb-3", "lb-2"}
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err != nil {
		t.Fatal(err)
	}
	pauses := 0
	for _, e := range a.clusters.Store.Entries(c.ID, 100) {
		if strings.Contains(e.Message, "a pause of 20ms before") {
			pauses++
			if e.Member != "lb-2" {
				t.Errorf("a pause before %s", e.Member)
			}
		}
	}
	if pauses != 1 || !slices.Equal(*updated, []string{"lb-1", "lb-2"}) {
		t.Errorf("%d pause(s), updated %v", pauses, *updated)
	}
}

// An update that hands connections over: before a member is drained the
// others copy its connections to it; drained, it serves them until they
// end (or its grace runs out), then updates; back, the others stop, and
// it copies theirs to them before it is undrained.
func TestClusterUpgradeHandsConnectionsOver(t *testing.T) {
	a, f, c, opts, _ := upgradeApp(t)
	ctx := context.Background()
	prevPoll := clusters.HandoffPoll
	clusters.HandoffPoll = time.Millisecond
	t.Cleanup(func() { clusters.HandoffPoll = prevPoll })
	anycast := netip.MustParseAddr("203.0.113.10")
	f.mu.Lock()
	for i, name := range []string{"lb-1", "lb-2", "lb-3"} {
		f.nodes[name].addrs = []netip.Addr{anycast, netip.MustParseAddr("fe80::" + string(rune('1'+i)))}
	}
	f.nodes["lb-1"].flows = 3
	f.mu.Unlock()
	opts.Members = []string{"lb-1", "lb-2"}
	opts.Handoff = &clusters.HandoffOptions{Grace: 5 * time.Second}
	mark := f.mark()
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err != nil {
		t.Fatal(err)
	}
	calls := f.callsSince(mark)
	// lb-1 refuses new connections, the others read its own; drained, it
	// serves them until none is left; back, the others stop, it holds
	// what it has no trace of and copies theirs, then is undrained.
	if !inOrder(calls, "lb-1 ClusterHandoff refuse", "lb-2 ClusterHandoff lb-1", "lb-3 ClusterHandoff lb-1", "lb-1 ClusterDrain", "lb-2 ClusterHandoff lb-1", "lb-2 ClusterHandoff lb-1",
		"lb-1 BootTime", "lb-2 ClusterHandoff lb-1 stop", "lb-3 ClusterHandoff lb-1 stop", "lb-1 ClusterHandoff hold lb-2", "lb-1 ClusterHandoff lb-3", "lb-1 ClusterUndrain",
		"lb-1 ClusterHandoff lb-2", "lb-2 ClusterHandoff refuse", "lb-3 ClusterHandoff lb-2", "lb-2 ClusterDrain") {
		t.Errorf("the calls: %v", calls)
	}
	// lb-1 went once it had none left - not after its grace.
	for _, want := range []string{"lb-2, lb-3 copy lb-1's connections to it while it is out - 3 now", "lb-1 has no connection left after",
		"lb-1 copies the others' connections to them for 1h0m0s: lb-2's 0, lb-3's 0", "connections on 203.0.113.10 handed over (a grace of 5s, copied back for 1h0m0s)"} {
		if !upgradeJournal(a, c.ID, want) {
			t.Errorf("not journaled: %q\n%+v", want, a.clusters.Store.Entries(c.ID, 60))
		}
	}
	f.mu.Lock()
	_, lb3copies := f.nodes["lb-3"].handoffs["lb-1"]
	_, lb2last := f.nodes["lb-2"].handoffs["lb-3"]
	f.mu.Unlock()
	if lb3copies || !lb2last {
		t.Errorf("after the update: lb-3 still copies lb-1's %v, lb-2 copies lb-3's %v", lb3copies, lb2last)
	}

	// Connections that never end: the member goes once its grace is over.
	for _, fm := range f.nodes {
		fm.version, fm.slot, fm.boot = "v1", "A", time.Now().Add(-time.Hour)
	}
	f.nodes["lb-1"].flows = 1 << 20
	opts.Members = []string{"lb-1"}
	opts.Handoff = &clusters.HandoffOptions{Grace: 30 * time.Millisecond}
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err != nil {
		t.Fatal(err)
	}
	if !upgradeJournal(a, c.ID, "connection(s) after its grace of 30ms: they end with its reboot") {
		t.Error("the grace's end isn't journaled")
	}

	// Refused before anything is touched: a member that can't, a cluster
	// whose members share no address, a grace out of bounds.
	for _, fm := range f.nodes {
		fm.version, fm.slot, fm.boot = "v1", "A", time.Now().Add(-time.Hour)
	}
	f.nodes["lb-2"].noHandoff = "the nftables extension isn't in this node's image"
	mark = f.mark()
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err == nil || !strings.Contains(err.Error(), "handing connections over: lb-2: the nftables extension isn't in this node's image") {
		t.Errorf("no nftables: %v", err)
	}
	f.nodes["lb-2"].noHandoff = ""
	f.nodes["lb-3"].addrs = nil
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err == nil || !strings.Contains(err.Error(), "the members share no address") {
		t.Errorf("no shared address: %v", err)
	}
	opts.Handoff = &clusters.HandoffOptions{Grace: 2 * time.Hour}
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err == nil || !strings.Contains(err.Error(), "the handoff's grace: 1 s to 1h0m0s") {
		t.Errorf("a grace of 2 h: %v", err)
	}
	if slices.ContainsFunc(f.callsSince(mark), func(s string) bool { return strings.Contains(s, "ClusterDrain") }) {
		t.Errorf("refused, but drained: %v", f.callsSince(mark))
	}
}

// GET .../upgrade-options says what an update may ask for, and why not;
// an update asking for the handoff goes once it is available.
func TestClusterUpgradeOptionsAPI(t *testing.T) {
	a, f, c, _, u := clusterUpgradeApp(t)
	get := func() upgradeOptions {
		t.Helper()
		rec := call(t, a.handleClusterUpgradeOptions, "GET", "/api/clusters/"+c.ID+"/upgrade-options", "GET /api/clusters/{id}/upgrade-options", nil)
		var o upgradeOptions
		if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		return o
	}
	o := get()
	if o.WaitBalancer.Available || o.WaitBalancer.Why != "the cluster has no balancer hook to ask" || o.Handoff.Available || !strings.Contains(o.Handoff.Why, "the members share no address") ||
		o.Handoff.GraceSeconds != 120 || o.Handoff.KeepSeconds != 3600 || o.MaxSettle != 1800 {
		t.Errorf("a cluster without hook nor anycast: %+v", o)
	}
	anycast := netip.MustParseAddr("203.0.113.10")
	f.mu.Lock()
	f.nodes["lb-1"].addrs, f.nodes["lb-2"].addrs = []netip.Addr{anycast}, []netip.Addr{anycast}
	f.nodes["lb-2"].noHandoff = "the nftables extension isn't in this node's image"
	f.mu.Unlock()
	if o := get(); o.Handoff.Available || o.Handoff.Why != "lb-2: the nftables extension isn't in this node's image" {
		t.Errorf("lb-2 without nftables: %+v", o.Handoff)
	}
	f.mu.Lock()
	f.nodes["lb-2"].noHandoff = ""
	f.mu.Unlock()
	if o := get(); !o.Handoff.Available || !slices.Equal(o.Handoff.Addresses, []string{"203.0.113.10"}) || !strings.Contains(o.Handoff.How, "on 203.0.113.10") {
		t.Errorf("an anycast cluster: %+v", o.Handoff)
	}
	code, body := postUpgrade(t, a, c, map[string]any{"version": upgradeTo, "handoff": map[string]any{"grace_seconds": 1}, "settle_seconds": 1})
	if code != http.StatusAccepted {
		t.Fatalf("POST: %d %s", code, body)
	}
	a.clusters.wait()
	a.runner.waitJobs()
	if len(u.upgraded) != 2 || !upgradeJournal(a, c.ID, "connections on 203.0.113.10 handed over (a grace of 1s") || !upgradeJournal(a, c.ID, "a pause of 1s before lb-2 goes") {
		t.Errorf("updated %v, journal %+v", u.upgraded, a.clusters.Store.Entries(c.ID, 60))
	}
}

// A webhook with confirm is refused on a RouterOS hook, whose routes are
// read.
func TestBalancerConfirmIsAWebhooks(t *testing.T) {
	b := clusters.Balancer{Kind: clusters.BalancerRouterOS, URL: "https://router.example.net", User: "janus", Prefixes: []string{"203.0.113.10/32"}, Confirm: true}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "balancer confirm: for a webhook") {
		t.Errorf("routeros with confirm: %v", err)
	}
	c := &clusters.Cluster{Balancer: &clusters.Balancer{Kind: clusters.BalancerRouterOS, Prefixes: []string{"203.0.113.10/32", "2001:db8::10/128"}}}
	if how, ok := c.CanConfirm(); !ok || how != "the router's routes to 203.0.113.10/32, 2001:db8::10/128" {
		t.Errorf("a router: %q %v", how, ok)
	}
}
