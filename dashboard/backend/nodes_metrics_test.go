package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/nodemetrics"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
)

// TestNodesMetrics: the node list's curves - each caller gets the nodes
// it sees, a node outside the fleet only an admin.
func TestNodesMetrics(t *testing.T) {
	a := newAuthApp(t)
	root, rita := a.login(t, "root", "root-password"), a.login(t, "rita", "rita-password")
	code, body := a.req(t, "POST", "/api/users", root, map[string]any{"name": "wes", "role": "none",
		"grants": []map[string]any{{"role": "operator", "selector": map[string]string{"team": "web"}, "domains": []string{"haproxy"}}}})
	if code != http.StatusCreated {
		t.Fatalf("wes: %d %s", code, body)
	}
	var made struct {
		Password string `json:"password"`
	}
	_ = json.Unmarshal([]byte(body), &made)
	if err := a.auth.ChangePassword("wes", made.Password, "wes-password"); err != nil {
		t.Fatal(err)
	}
	wes := a.login(t, "wes", "wes-password")

	nodeCA, _ := pki.NewCA("node")
	ids := map[string]string{}
	for _, n := range []*store.Node{
		{Name: "web-1", Fleet: true, Labels: map[string]string{"team": "web"}},
		{Name: "db-1", Fleet: true, Labels: map[string]string{"team": "db"}},
		{Name: "new-1"}, // not trusting the fleet yet
	} {
		n.Address, n.CACertPEM = "127.0.0.1:1", nodeCA.CertPEM
		if err := a.store.Add(n); err != nil {
			t.Fatal(err)
		}
		ids[n.Name] = n.ID
	}
	var mu sync.Mutex
	ticks := map[string]uint64{}
	a.nodeMetrics = nodemetrics.New(nodemetrics.Options{Interval: 5 * time.Millisecond, Lease: time.Hour, Read: func(_ context.Context, id string) (*nodemetrics.Counters, error) {
		mu.Lock()
		defer mu.Unlock()
		ticks[id] += 100
		return &nodemetrics.Counters{TotalTicks: ticks[id], IdleTicks: ticks[id] / 2, HAProxy: true}, nil
	}})
	t.Cleanup(a.nodeMetrics.Close)

	get := func(cred string) nodesMetrics {
		code, body := a.req(t, "GET", "/api/nodes/metrics", cred, nil)
		if code != http.StatusOK {
			t.Fatalf("metrics: %d %s", code, body)
		}
		var m nodesMetrics
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	names := func(m nodesMetrics) []string {
		var out []string
		for name, id := range ids {
			if _, ok := m.Nodes[id]; ok {
				out = append(out, name)
			}
		}
		slices.Sort(out)
		return out
	}
	// root's request starts the sampling of all three.
	deadline := time.Now().Add(5 * time.Second)
	for {
		m := get(root)
		if len(m.Nodes) == 3 && len(m.Nodes[ids["web-1"]].T) >= 2 {
			if m.IntervalMs != 5 || m.NowMs == 0 {
				t.Errorf("interval %d, now %d", m.IntervalMs, m.NowMs)
			}
			if cpu := m.Nodes[ids["web-1"]].CPU; cpu[len(cpu)-1] == nil || *cpu[len(cpu)-1] != 50 {
				t.Errorf("web-1's CPU: %v", cpu)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("root: %v", names(m))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := names(get(wes)); !slices.Equal(got, []string{"web-1"}) {
		t.Errorf("wes gets %v", got)
	}
	if got := names(get(rita)); !slices.Equal(got, []string{"db-1", "web-1"}) {
		t.Errorf("a reader gets %v", got)
	}
	// since: only what's newer.
	last := get(root).Nodes[ids["web-1"]].T
	newest := last[len(last)-1]
	code, body = a.req(t, "GET", "/api/nodes/metrics?since="+strconv.FormatInt(newest, 10), root, nil)
	var newer nodesMetrics
	if err := json.Unmarshal([]byte(body), &newer); code != http.StatusOK || err != nil {
		t.Fatalf("since: %d %s", code, body)
	}
	for _, ts := range newer.Nodes[ids["web-1"]].T {
		if ts <= newest {
			t.Errorf("since %d gave %d", newest, ts)
		}
	}
}
