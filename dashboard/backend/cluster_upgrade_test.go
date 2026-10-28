package main

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/clusters"
)

// upgradeApp is a created cluster of lb-1..3 on Janus v1 (slot A), and
// the options of an update to v2 whose nodes reboot into it at once -
// what each one does on its Update said by how.
func upgradeApp(t *testing.T) (*app, *clusterFake, *clusters.Cluster, *clusters.UpgradeOptions, *[]string) {
	t.Helper()
	a, f, nodes := clusterApp(t, "lb-1", "lb-2", "lb-3")
	c := newCluster(t, a, nodes, "lb-1", "lb-2", "lb-3")
	if err := a.clusters.Create(context.Background(), c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	booted := time.Now().Add(-time.Hour)
	for _, fm := range f.nodes {
		fm.version, fm.slot, fm.boot = "v1", "A", booted
	}
	a.clusters.LearnTimeout = time.Second
	var updated []string
	opts := &clusters.UpgradeOptions{
		Release: "v2", Poll: time.Millisecond, BackTimeout: 200 * time.Millisecond,
		Update: func(_ context.Context, m clusters.Member) (string, error) {
			updated = append(updated, m.Name)
			f.mu.Lock()
			defer f.mu.Unlock()
			// It reboots into the new slot, keeps its drain, learns its
			// tables after a few checks.
			fm := f.nodes[m.Name]
			fm.version, fm.slot, fm.boot = "v2", "B", time.Now()
			fm.learning, fm.learnAfter = true, 3
			return "rebooting through the firmware", nil
		},
	}
	return a, f, c, opts, &updated
}

func upgradeJournal(a *app, id, words string) bool {
	return slices.ContainsFunc(a.clusters.Store.Entries(id, 500), func(e clusters.Entry) bool { return strings.Contains(e.Message, words) })
}

// Every member updated in turn, the VRRP master last: drained, updated,
// back on the new slot, its tables learnt, undrained - the balancer told
// it goes before its drain and comes back after its undrain.
func TestClusterUpgrade(t *testing.T) {
	a, f, c, opts, updated := upgradeApp(t)
	ctx := context.Background()
	f.nodes["lb-1"].masters = []string{"front"}
	prevRetries := clusters.BalancerRetries
	clusters.BalancerRetries = []time.Duration{time.Millisecond}
	t.Cleanup(func() { clusters.BalancerRetries = prevRetries })
	hook := &testBalancer{f: f}
	srv := httptest.NewTLSServer(hook)
	t.Cleanup(srv.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	if err := a.clusters.SetBalancer(ctx, c.ID, &clusters.Balancer{URL: srv.URL + "/hook", CACert: ca}, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	n, mark := hook.count(), f.mark()
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*updated, []string{"lb-2", "lb-3", "lb-1"}) {
		t.Errorf("the order: %v (the VRRP master last)", *updated)
	}
	for _, name := range []string{"lb-1", "lb-2", "lb-3"} {
		calls := f.callsOf(name, mark)
		if !inOrder(calls, "ClusterDrain", "BootTime", "ClusterStatus", "ClusterUndrain") {
			t.Errorf("%s: %v", name, calls)
		}
		if fm := f.nodes[name]; fm.drained || fm.version != "v2" || fm.slot != "B" || fm.learning {
			t.Errorf("%s after the update: %+v", name, fm)
		}
		if st := a.clusters.Store.State(c.ID).Members[name]; st.Drained || st.Stage != "" || st.Error != "" {
			t.Errorf("%s's state: %+v", name, st)
		}
	}
	// The balancer: out before each drain, in after each undrain.
	var events []string
	for _, call := range hook.since(n) {
		events = append(events, call.Event+" "+call.Member)
	}
	if !slices.Equal(events, []string{"out lb-2", "in lb-2", "out lb-3", "in lb-3", "out lb-1", "in lb-1"}) {
		t.Errorf("the balancer: %v", events)
	}
	if !upgradeJournal(a, c.ID, "lb-1 runs Janus v2 on slot B, in service") || !upgradeJournal(a, c.ID, "edge runs Janus v2: 3 member(s) updated, 0 already on it") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 10))
	}

	// Again: every member on the target, nothing touched.
	*updated = nil
	mark = f.mark()
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err != nil {
		t.Fatal(err)
	}
	if len(*updated) > 0 || slices.Contains(f.callsSince(mark), "lb-2 ClusterDrain") {
		t.Errorf("a cluster on the target updated again: %v", f.callsSince(mark))
	}
	if !upgradeJournal(a, c.ID, "lb-2 runs Janus v2 already: skipped") {
		t.Error("the skip isn't journaled")
	}
}

// The members given, in their order; a member already on the target
// skipped - an update stopped resumes.
func TestClusterUpgradeMembersAndResume(t *testing.T) {
	a, f, c, opts, updated := upgradeApp(t)
	ctx := context.Background()
	f.nodes["lb-3"].version, f.nodes["lb-3"].slot = "v2", "B"
	opts.Members = []string{"lb-3", "lb-1"}
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*updated, []string{"lb-1"}) || f.nodes["lb-2"].version != "v1" {
		t.Errorf("updated %v, lb-2 on %s", *updated, f.nodes["lb-2"].version)
	}
	opts.Members = []string{"lb-9"}
	if err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin"); err == nil || !strings.Contains(err.Error(), "lb-9 isn't a member") {
		t.Errorf("an unknown member: %v", err)
	}
}

// The cluster must be healthy first: a member drained, a peer down, a
// table not in sync - refused, naming the member, nothing touched.
func TestClusterUpgradeRefusesAnUnhealthyCluster(t *testing.T) {
	for name, spoil := range map[string]func(f *clusterFake){
		"lb-3 is drained":             func(f *clusterFake) { f.nodes["lb-3"].drained = true },
		"lb-3 doesn't reach its peer": func(f *clusterFake) { f.nodes["lb-3"].alone = true },
		"lb-3's HAProxy doesn't answer": func(f *clusterFake) {
			f.nodes["lb-3"].healthy = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			a, f, c, opts, updated := upgradeApp(t)
			spoil(f)
			opts.Members = []string{"lb-2"}
			mark := f.mark()
			err := a.clusters.Upgrade(context.Background(), c.ID, *opts, "admin")
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("refused: %v", err)
			}
			if len(*updated) > 0 || slices.Contains(f.callsOf("lb-2", mark), "ClusterDrain") {
				t.Errorf("touched: %v", f.callsOf("lb-2", mark))
			}
		})
	}
}

// The first failure stops the update: a node that refused its update is
// back in service, the members after it untouched.
func TestClusterUpgradeStopsAtTheFirstFailure(t *testing.T) {
	a, f, c, opts, updated := upgradeApp(t)
	ctx := context.Background()
	update := opts.Update
	opts.Update = func(ctx context.Context, m clusters.Member) (string, error) {
		if m.Name == "lb-2" {
			*updated = append(*updated, m.Name)
			return "", errors.New("the bundle's signature doesn't verify")
		}
		return update(ctx, m)
	}
	var done []string
	opts.Done = func(m clusters.Member, err error) {
		done = append(done, m.Name+" "+map[bool]string{true: "ok", false: "failed"}[err == nil])
	}
	err := a.clusters.Upgrade(ctx, c.ID, *opts, "admin")
	if err == nil || !strings.Contains(err.Error(), "lb-2's update failed: the bundle's signature doesn't verify - back in service on Janus v1") {
		t.Fatalf("the failure: %v", err)
	}
	if !slices.Equal(*updated, []string{"lb-1", "lb-2"}) || f.nodes["lb-2"].drained || f.nodes["lb-3"].version != "v1" {
		t.Errorf("updated %v, lb-2 drained %v, lb-3 on %s", *updated, f.nodes["lb-2"].drained, f.nodes["lb-3"].version)
	}
	if !slices.Equal(done, []string{"lb-1 ok", "lb-2 failed"}) {
		t.Errorf("told: %v", done)
	}
	if st := a.clusters.Store.State(c.ID).Members["lb-2"]; !strings.Contains(st.Error, "update failed") || st.Drained {
		t.Errorf("lb-2's state: %+v", st)
	}
	if !upgradeJournal(a, c.ID, "the update stops at lb-2") {
		t.Error("the stop isn't journaled")
	}
}

// A member whose new release doesn't confirm healthy reverts: back on
// its previous release and slot, in service, the update stopped.
func TestClusterUpgradeRevert(t *testing.T) {
	a, f, c, opts, _ := upgradeApp(t)
	opts.Members = []string{"lb-2", "lb-3"}
	opts.Update = func(_ context.Context, m clusters.Member) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		// Rebooted twice - not answering meanwhile -, back on slot A.
		f.nodes[m.Name].boot, f.nodes[m.Name].downFor = time.Now(), 2
		return "rebooting", nil
	}
	err := a.clusters.Upgrade(context.Background(), c.ID, *opts, "admin")
	var rev *clusters.RevertedError
	if !errors.As(err, &rev) || rev.Member != "lb-2" || rev.Running != "v1" {
		t.Fatalf("the revert: %v", err)
	}
	if f.nodes["lb-2"].drained || f.nodes["lb-3"].drained || slices.Contains(f.callsOf("lb-3", 0), "ClusterDrain") {
		t.Error("lb-2 left drained, or lb-3 touched")
	}
	if !upgradeJournal(a, c.ID, "lb-2 reverted to Janus v1 on slot A") {
		t.Errorf("the journal: %+v", a.clusters.Store.Entries(c.ID, 10))
	}
}

// A member not back in time stays drained, the update stopped.
func TestClusterUpgradeTimeout(t *testing.T) {
	a, f, c, opts, _ := upgradeApp(t)
	opts.Members = []string{"lb-2", "lb-3"}
	opts.Update = func(_ context.Context, m clusters.Member) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.nodes[m.Name].down = true
		return "rebooting", nil
	}
	err := a.clusters.Upgrade(context.Background(), c.ID, *opts, "admin")
	var nb *clusters.NotBackError
	if !errors.As(err, &nb) || nb.Member != "lb-2" || !strings.Contains(err.Error(), "it stays drained") {
		t.Fatalf("the timeout: %v", err)
	}
	if !f.nodes["lb-2"].drained || slices.Contains(f.callsOf("lb-3", 0), "ClusterDrain") {
		t.Error("lb-2 undrained, or lb-3 touched")
	}
	if st := a.clusters.Store.State(c.ID).Members["lb-2"]; !st.Drained || st.Error == "" {
		t.Errorf("lb-2's state: %+v", st)
	}
}

// A member back in service on its own - drained on a release that kept
// no drain across a reboot - is drained again before its tables are
// learnt, then undrained.
func TestClusterUpgradeDrainsAgain(t *testing.T) {
	a, f, c, opts, _ := upgradeApp(t)
	opts.Members = []string{"lb-2"}
	update := opts.Update
	opts.Update = func(ctx context.Context, m clusters.Member) (string, error) {
		said, err := update(ctx, m)
		f.mu.Lock()
		f.nodes[m.Name].drained = false
		f.mu.Unlock()
		return said, err
	}
	mark := f.mark()
	if err := a.clusters.Upgrade(context.Background(), c.ID, *opts, "admin"); err != nil {
		t.Fatal(err)
	}
	if calls := f.callsOf("lb-2", mark); !inOrder(calls, "ClusterDrain", "BootTime", "ClusterDrain", "ClusterStatus", "ClusterUndrain") {
		t.Errorf("lb-2: %v", calls)
	}
	if !upgradeJournal(a, c.ID, "lb-2 came back in service on its own") {
		t.Error("not journaled")
	}
}

// A BGP member back in service announces its routes again only once its
// sessions are re-established: the next member is drained only then -
// else the routers would have no member to send to (seen on real BGP
// members).
func TestClusterUpgradeWaitsForTheAnnouncement(t *testing.T) {
	a, f, c, opts, _ := upgradeApp(t)
	opts.Members = []string{"lb-1", "lb-2"}
	update := opts.Update
	opts.Update = func(ctx context.Context, m clusters.Member) (string, error) {
		said, err := update(ctx, m)
		f.mu.Lock()
		f.nodes[m.Name].announceAfter = 3
		f.mu.Unlock()
		return said, err
	}
	mark := f.mark()
	if err := a.clusters.Upgrade(context.Background(), c.ID, *opts, "admin"); err != nil {
		t.Fatal(err)
	}
	calls := f.callsSince(mark)
	last := -1
	for i, call := range calls {
		if call == "lb-1 Announcing" {
			last = i
		}
	}
	drain2 := slices.Index(calls, "lb-2 ClusterDrain")
	if last < 0 || drain2 < last || !inOrder(calls, "lb-1 ClusterUndrain", "lb-1 Announcing", "lb-1 Announcing", "lb-1 Announcing", "lb-1 Announcing", "lb-2 ClusterDrain") {
		t.Errorf("lb-2 drained before lb-1 announced again: %v", calls)
	}
	if !upgradeJournal(a, c.ID, "lb-1 announces its routes to its BGP peers again") {
		t.Error("not journaled")
	}
	// One that never announces again stops the update, the next left alone.
	prev := clusters.AnnounceTimeout
	clusters.AnnounceTimeout = 50 * time.Millisecond
	t.Cleanup(func() { clusters.AnnounceTimeout = prev })
	for _, fm := range f.nodes {
		fm.version, fm.slot, fm.boot = "v1", "A", time.Now().Add(-time.Hour)
	}
	opts.Update = func(ctx context.Context, m clusters.Member) (string, error) {
		said, err := update(ctx, m)
		f.mu.Lock()
		f.nodes[m.Name].announceAfter = 1 << 20
		f.mu.Unlock()
		return said, err
	}
	mark = f.mark()
	err := a.clusters.Upgrade(context.Background(), c.ID, *opts, "admin")
	if err == nil || !strings.Contains(err.Error(), "lb-1 is back in service but announces nothing to its BGP peers") {
		t.Fatalf("never announcing: %v", err)
	}
	if slices.Contains(f.callsSince(mark), "lb-2 ClusterDrain") {
		t.Error("lb-2 drained anyway")
	}
	// And a cluster whose other member announces nothing is refused.
	f.nodes["lb-1"].announceAfter = 1 << 20
	opts.Members = []string{"lb-2"}
	if err := a.clusters.Upgrade(context.Background(), c.ID, *opts, "admin"); err == nil || !strings.Contains(err.Error(), "lb-1 announces nothing to its BGP peers yet") {
		t.Errorf("refused: %v", err)
	}
}

// A boot time that moved without the node ever going away - its clock
// stepped - isn't a revert: the update waits for the reboot, and stops
// when it doesn't come.
func TestClusterUpgradeClockStepIsNoRevert(t *testing.T) {
	a, f, c, opts, _ := upgradeApp(t)
	opts.Members = []string{"lb-2"}
	opts.Update = func(_ context.Context, m clusters.Member) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.nodes[m.Name].boot = time.Now()
		return "rebooting", nil
	}
	err := a.clusters.Upgrade(context.Background(), c.ID, *opts, "admin")
	var nb *clusters.NotBackError
	if !errors.As(err, &nb) || !strings.Contains(err.Error(), "never seen rebooting") {
		t.Fatalf("a clock step: %v", err)
	}
}
