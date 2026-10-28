package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/clusters"
	"github.com/swenske/Janus/internal/schematic"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// upgradeNodes is fakeNodes whose updates the cluster's members feel: the
// node named by store ID reboots into the bundle's release on slot B.
type upgradeNodes struct {
	*fakeNodes
	f          *clusterFake
	byNode     map[string]string // store node ID -> member name
	resolveErr error
	mu         sync.Mutex
	upgraded   []string // member names, in order
}

func (u *upgradeNodes) ResolveBundle(ctx context.Context, version string, sc *schematic.Schematic) (*nodeproxy.Bundle, string, error) {
	if u.resolveErr != nil {
		return nil, "missing", u.resolveErr
	}
	return u.fakeNodes.ResolveBundle(ctx, version, sc)
}

func (u *upgradeNodes) Upgrade(ctx context.Context, node *store.Node, src *janusv1alpha1.ImageSource, mode janusv1alpha1.RebootMode) (string, error) {
	how, err := u.fakeNodes.Upgrade(ctx, node, src, mode)
	if err != nil {
		return "", err
	}
	u.fakeNodes.mu.Lock()
	version, sc := u.version, u.schematic
	u.fakeNodes.mu.Unlock()
	name := u.byNode[node.ID]
	u.mu.Lock()
	u.upgraded = append(u.upgraded, name)
	u.mu.Unlock()
	u.f.mu.Lock()
	defer u.f.mu.Unlock()
	fm := u.f.nodes[name]
	fm.version, fm.schematic, fm.slot, fm.boot = version, sc, "B", time.Now()
	return how, nil
}

const upgradeFrom, upgradeTo = "v2026.10.02-4", "v2026.10.29"

// clusterUpgradeApp is cluster edge of lb-1 - one of the Controller's
// machines - and lb-2, a node, both on upgradeFrom.
func clusterUpgradeApp(t *testing.T) (*app, *clusterFake, *clusters.Cluster, *machines.Machine, *upgradeNodes) {
	t.Helper()
	a, f, byName := clusterApp(t, "lb-1", "lb-2")
	c := newCluster(t, a, byName, "lb-1", "lb-2")
	if err := a.clusters.Create(context.Background(), c.ID, "lb-1", "admin"); err != nil {
		t.Fatal(err)
	}
	c, _ = a.clusters.Store.Get(c.ID)
	hv := addTrustedHypervisor(t, a)
	m := &machines.Machine{
		Spec:  machines.Spec{Name: "lb-1", HypervisorID: hv.ID, VCPUs: 2, MemoryMiB: 1024, Version: upgradeFrom, NICs: []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", MAC: mgmtMAC, Mode: "dhcp"}}},
		Phase: machines.PhaseReady, NodeID: byName["lb-1"].ID, Version: upgradeFrom, Schematic: schematic.DefaultID(),
		Ref: &hypervisor.MachineRef{MachineID: "m", UUID: "uuid-m", Name: "janus-lb-1"},
	}
	if err := a.machines.Add(m); err != nil {
		t.Fatal(err)
	}
	booted := time.Now().Add(-time.Hour)
	for _, fm := range f.nodes {
		fm.version, fm.slot, fm.boot, fm.schematic = upgradeFrom, "A", booted, schematic.DefaultID()
	}
	u := &upgradeNodes{fakeNodes: &fakeNodes{version: upgradeFrom, schematic: schematic.DefaultID(), cfg: &janusv1alpha1.NetworkConfig{Hostname: "lb-1"}}, f: f,
		byNode: map[string]string{byName["lb-1"].ID: "lb-1", byName["lb-2"].ID: "lb-2"}}
	prevNodes, prevPoll, prevBack := nodes, clusterUpgradePoll, clusterUpgradeBack
	nodes, clusterUpgradePoll, clusterUpgradeBack = u, time.Millisecond, 2*time.Second
	a.clusters.LearnTimeout = time.Second
	t.Cleanup(func() { nodes, clusterUpgradePoll, clusterUpgradeBack = prevNodes, prevPoll, prevBack })
	t.Cleanup(a.runner.waitJobs)
	return a, f, c, m, u
}

func postUpgrade(t *testing.T, a *app, c *clusters.Cluster, body any) (int, string) {
	t.Helper()
	rec := call(t, a.handleClusterUpgrade, "POST", "/api/clusters/"+c.ID+"/upgrade", "POST /api/clusters/{id}/upgrade", body)
	return rec.Code, rec.Body.String()
}

// What can be checked first is refused at once (400), nothing touched.
func TestClusterUpgradeRefusals(t *testing.T) {
	a, f, c, m, u := clusterUpgradeApp(t)
	for name, tc := range map[string]struct {
		body  map[string]any
		spoil func()
		want  string
	}{
		"unknown member":                  {map[string]any{"version": upgradeTo, "members": []string{"lb-9"}}, nil, "lb-9 isn't a member"},
		"not a release":                   {map[string]any{"version": "latest"}, nil, "isn't a Janus release"},
		"reboot mode":                     {map[string]any{"version": upgradeTo, "reboot_mode": "warp"}, nil, "firmware or kexec"},
		"another image without new_image": {map[string]any{"version": upgradeTo, "extensions": []string{"keepalived"}}, nil, "set new_image"},
		"image URL": {map[string]any{"version": upgradeTo}, func() {
			_, _ = a.machines.Update(m.ID, func(m *machines.Machine) error {
				m.Spec.Image = &machines.ImageSource{URL: "https://images/janus.qcow2", SHA256: strings.Repeat("b", 64)}
				return nil
			})
		}, "created from an image URL"},
		"no bundle": {map[string]any{"version": upgradeTo}, func() { u.resolveErr = errors.New("no such release") }, "no update to Janus " + upgradeTo},
		"unhealthy": {map[string]any{"version": upgradeTo}, func() { f.nodes["lb-2"].drained = true }, "the cluster isn't healthy: lb-2 is drained"},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.spoil != nil {
				tc.spoil()
				t.Cleanup(func() {
					u.resolveErr, f.nodes["lb-2"].drained = nil, false
					_, _ = a.machines.Update(m.ID, func(m *machines.Machine) error { m.Spec.Image = nil; return nil })
				})
			}
			code, body := postUpgrade(t, a, c, tc.body)
			if code != http.StatusBadRequest || !strings.Contains(body, tc.want) {
				t.Errorf("%d %s", code, body)
			}
			if len(u.upgraded) > 0 || a.clusters.isBusy(c.ID) {
				t.Error("touched")
			}
		})
	}
}

// Both members updated in turn through the handler: the machine through
// its own update path - its record updating, then its spec on the new
// release -, the node on its own.
func TestClusterUpgradeThroughTheAPI(t *testing.T) {
	a, f, c, m, u := clusterUpgradeApp(t)
	f.nodes["lb-1"].masters = []string{"front"}
	code, body := postUpgrade(t, a, c, map[string]any{"version": upgradeTo, "reboot_mode": "kexec"})
	if code != http.StatusAccepted || !strings.Contains(body, `"phase":"upgrading"`) {
		t.Fatalf("POST: %d %s", code, body)
	}
	if code, _ := postUpgrade(t, a, c, map[string]any{"version": upgradeTo}); code != http.StatusConflict {
		t.Errorf("a second update while one runs: %d", code)
	}
	a.clusters.wait()
	cur, _ := a.clusters.Store.Get(c.ID)
	if cur.Error != "" || cur.Phase != "" {
		t.Fatalf("the update: %q %q", cur.Phase, cur.Error)
	}
	if !slices.Equal(u.upgraded, []string{"lb-2", "lb-1"}) {
		t.Errorf("updated %v: the VRRP master last", u.upgraded)
	}
	for _, mode := range u.modes {
		if mode != janusv1alpha1.RebootMode_REBOOT_MODE_KEXEC {
			t.Errorf("reboot modes %v", u.modes)
		}
	}
	for _, name := range []string{"lb-1", "lb-2"} {
		if fm := f.nodes[name]; fm.version != upgradeTo || fm.slot != "B" || fm.drained {
			t.Errorf("%s: %+v", name, fm)
		}
	}
	a.runner.waitJobs()
	got, _ := a.machines.Get(m.ID)
	if got.Phase != machines.PhaseReady || got.Error != "" || got.Spec.Version != upgradeTo || got.Version != upgradeTo {
		t.Errorf("the machine after the update: %s %q spec %s, runs %s", got.Phase, got.Error, got.Spec.Version, got.Version)
	}
	if ev := eventsOf(got); !strings.Contains(ev, "updating: cluster edge's update to Janus "+upgradeTo) || !strings.Contains(ev, "the node runs Janus "+upgradeTo+" - updated") {
		t.Errorf("the machine's history: %s", ev)
	}
	view := a.clusterView(cur)
	for _, mv := range view.Members {
		if mv.State.Stage != "" || mv.State.Drained {
			t.Errorf("%s in the view: %+v", mv.Name, mv.State)
		}
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), `"stage"`) {
		t.Errorf("a stage left in the view: %s", raw)
	}
}

// Another image for every member, with new_image: the node told the
// schematic may change, the machine's extensions follow.
func TestClusterUpgradeNewImage(t *testing.T) {
	a, f, c, m, u := clusterUpgradeApp(t)
	code, body := postUpgrade(t, a, c, map[string]any{"version": upgradeTo, "extensions": []string{"keepalived"}, "new_image": true})
	if code != http.StatusAccepted {
		t.Fatalf("POST: %d %s", code, body)
	}
	a.clusters.wait()
	if cur, _ := a.clusters.Store.Get(c.ID); cur.Error != "" {
		t.Fatalf("the update: %s", cur.Error)
	}
	for _, src := range u.upgrades {
		if !src.AllowSchematicChange {
			t.Errorf("the node wasn't told its image changes: %+v", src)
		}
	}
	a.runner.waitJobs()
	if got, _ := a.machines.Get(m.ID); !slices.Equal(got.Spec.Extensions, []string{"keepalived"}) {
		t.Errorf("the machine's extensions: %v", got.Spec.Extensions)
	}
	if f.nodes["lb-2"].schematic == schematic.DefaultID() {
		t.Error("lb-2 still on the default image")
	}
}

// PATCH /api/machines/{id} changing a member's version goes through the
// cluster's update for it alone; with its network too, refused.
func TestMachineUpdateOfAClusterMember(t *testing.T) {
	a, f, c, m, u := clusterUpgradeApp(t)
	if code, body := patch(t, a, m.ID, map[string]any{"version": upgradeTo, "dns": []string{"10.0.0.53"}}); code != http.StatusBadRequest || !strings.Contains(body, "change its version alone") {
		t.Errorf("version and network: %d %s", code, body)
	}
	if len(u.upgraded) > 0 {
		t.Fatal("touched")
	}
	code, body := patch(t, a, m.ID, map[string]any{"version": upgradeTo})
	if code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	a.clusters.wait()
	a.runner.waitJobs()
	if cur, _ := a.clusters.Store.Get(c.ID); cur.Error != "" {
		t.Fatalf("the update: %s", cur.Error)
	}
	if !slices.Equal(u.upgraded, []string{"lb-1"}) || f.nodes["lb-2"].version != upgradeFrom {
		t.Errorf("updated %v, lb-2 on %s", u.upgraded, f.nodes["lb-2"].version)
	}
	if calls := f.callsOf("lb-1", 0); !inOrder(calls, "ClusterDrain", "BootTime", "ClusterUndrain") {
		t.Errorf("lb-1 not drained around its update: %v", calls)
	}
	if got, _ := a.machines.Get(m.ID); got.Spec.Version != upgradeTo || got.Phase != machines.PhaseReady {
		t.Errorf("the machine: %s %s", got.Spec.Version, got.Phase)
	}
	if !upgradeJournal(a, c.ID, "lb-1's machine lb-1 changed to Janus "+upgradeTo+": through the cluster's update") {
		t.Error("not journaled")
	}
}
