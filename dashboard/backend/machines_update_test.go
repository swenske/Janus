package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/schematic"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// fakeNodes is a node that does what it's asked.
type fakeNodes struct {
	mu         sync.Mutex
	version    string
	schematic  string
	shutdowns  int
	networks   []*janusv1alpha1.NetworkConfig
	upgrades   []*janusv1alpha1.ImageSource
	upgradeErr error
}

func (f *fakeNodes) Shutdown(context.Context, *store.Node) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdowns++
	return nil
}

func (f *fakeNodes) ApplyNetwork(_ context.Context, _ *store.Node, _ *store.Store, cfg *janusv1alpha1.NetworkConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks = append(f.networks, cfg)
	return nil
}

func (f *fakeNodes) Upgrade(_ context.Context, _ *store.Node, src *janusv1alpha1.ImageSource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upgradeErr != nil {
		return f.upgradeErr
	}
	f.upgrades = append(f.upgrades, src)
	// The bundle's version, as the node reports it once rebooted.
	f.version = strings.TrimPrefix(src.Reference, "https://bundles/")
	if src.AllowSchematicChange {
		f.schematic = "changed"
	}
	return nil
}

func (f *fakeNodes) Version(context.Context, *store.Node) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, f.schematic, nil
}

func (f *fakeNodes) ResolveBundle(_ context.Context, version string, extensions []string) (*nodeproxy.Bundle, string, error) {
	sc := schematic.DefaultID()
	if len(extensions) > 0 {
		sc = "changed"
	}
	return &nodeproxy.Bundle{Version: version, Schematic: sc, BaseURL: "https://bundles/" + version, SHA256: strings.Repeat("a", 64)}, "ready", nil
}

// readyMachine is a machine whose node registered.
func readyMachine(t *testing.T, a *app, hv *hypervisor.Hypervisor) *machines.Machine {
	t.Helper()
	node := &store.Node{Name: "lb1", Address: "127.0.0.1:1", Port: 39599, CACertPEM: []byte("ca"), ServiceCertPEM: []byte("c"), ServiceKeyPEM: []byte("k")}
	if err := a.store.Add(node); err != nil {
		t.Fatal(err)
	}
	m := &machines.Machine{
		Spec: machines.Spec{Name: "lb1", HypervisorID: hv.ID, VCPUs: 2, MemoryMiB: 1024, Version: "v2026.10.02-4",
			NICs: []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", MAC: "52:54:00:00:00:01", Mode: "static", Addresses: []string{"10.0.0.5/24"}}}},
		Phase: machines.PhaseReady, NodeID: node.ID, Version: "v2026.10.02-4", Schematic: schematic.DefaultID(),
		Ref: &hypervisor.MachineRef{MachineID: "m", UUID: "uuid-m", Name: "janus-lb1"},
	}
	if err := a.machines.Add(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func withFakeNodes(t *testing.T, f *fakeNodes) {
	prev := nodes
	nodes = f
	t.Cleanup(func() { nodes = prev })
}

func patch(t *testing.T, a *app, id string, body any) (int, string) {
	t.Helper()
	rec := call(t, a.handleMachineUpdate, "PATCH", "/api/machines/"+id, "PATCH /api/machines/{id}", body)
	return rec.Code, rec.Body.String()
}

func waitReady(t *testing.T, a *app, id string) *machines.Machine {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, _ := a.machines.Get(id)
		if m.Phase == machines.PhaseReady {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the update never ended")
	return nil
}

func TestMachineUpdateSize(t *testing.T) {
	a, fake := newTestApp(t)
	f := &fakeNodes{version: "v2026.10.02-4", schematic: schematic.DefaultID()}
	withFakeNodes(t, f)
	m := readyMachine(t, a, addTrustedHypervisor(t, a))

	if code, body := patch(t, a, m.ID, map[string]any{"vcpus": 4, "memory_mib": 2048}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	if got.Error != "" || got.Spec.VCPUs != 4 || got.Spec.MemoryMiB != 2048 {
		t.Fatalf("after resize: %+v", got)
	}
	if f.shutdowns != 1 || len(fake.resized) != 1 || fake.resized[0] != [2]int{4, 2048} {
		t.Errorf("shutdowns %d, resized %v", f.shutdowns, fake.resized)
	}
	if fake.powered[len(fake.powered)-1] != hypervisor.PowerStart {
		t.Errorf("not started again: %v", fake.powered)
	}
}

func TestMachineUpdateVersionAndExtensions(t *testing.T) {
	a, _ := newTestApp(t)
	f := &fakeNodes{version: "v2026.10.02-4", schematic: schematic.DefaultID()}
	withFakeNodes(t, f)
	m := readyMachine(t, a, addTrustedHypervisor(t, a))

	if code, body := patch(t, a, m.ID, map[string]any{"version": "v2026.10.05"}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	if got.Error != "" || got.Version != "v2026.10.05" || got.Spec.Version != "v2026.10.05" {
		t.Fatalf("after the update: %+v", got)
	}
	if len(f.upgrades) != 1 || f.upgrades[0].GetReference() != "https://bundles/v2026.10.05" || f.upgrades[0].GetAllowSchematicChange() {
		t.Errorf("upgrade %v", f.upgrades)
	}

	// Extensions: another schematic, allowed explicitly.
	if code, body := patch(t, a, m.ID, map[string]any{"extensions": []string{"keepalived"}}); code != http.StatusAccepted {
		t.Fatalf("PATCH extensions: %d %s", code, body)
	}
	got = waitReady(t, a, m.ID)
	if got.Error != "" || got.Schematic != "changed" || len(got.Spec.Extensions) != 1 {
		t.Fatalf("after the extensions: %+v", got)
	}
	if !f.upgrades[1].GetAllowSchematicChange() {
		t.Error("a schematic change wasn't allowed explicitly")
	}
}

func TestMachineUpdateNetwork(t *testing.T) {
	a, _ := newTestApp(t)
	f := &fakeNodes{version: "v2026.10.02-4", schematic: schematic.DefaultID()}
	withFakeNodes(t, f)
	m := readyMachine(t, a, addTrustedHypervisor(t, a))

	nics := []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", Mode: "static", Addresses: []string{"10.0.0.6/24"}, Gateway: "10.0.0.1"}}
	if code, body := patch(t, a, m.ID, map[string]any{"nics": nics, "ntp": []string{"10.0.0.1"}}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	if got.Error != "" || got.Spec.NICs[0].Addresses[0] != "10.0.0.6/24" || got.Spec.NICs[0].MAC != "52:54:00:00:00:01" {
		t.Fatalf("after the network: %+v", got.Spec)
	}
	if len(f.networks) != 1 {
		t.Fatalf("networks applied: %d", len(f.networks))
	}
	cfg := f.networks[0]
	if cfg.GetHostname() != "lb1" || cfg.GetInterfaces()[0].GetMac() != "52:54:00:00:00:01" || cfg.GetInterfaces()[0].GetGateway() != "10.0.0.1" || cfg.GetNtp().GetServers()[0] != "10.0.0.1" {
		t.Errorf("applied %v", cfg)
	}
	if f.shutdowns != 0 || len(f.upgrades) != 0 {
		t.Error("a network change did more than the network")
	}
}

func TestMachineUpdateRefusals(t *testing.T) {
	a, _ := newTestApp(t)
	f := &fakeNodes{version: "v2026.10.02-4", schematic: schematic.DefaultID()}
	withFakeNodes(t, f)
	hv := addTrustedHypervisor(t, a)
	m := readyMachine(t, a, hv)

	for name, body := range map[string]any{
		"another network":   map[string]any{"nics": []machines.NIC{{Network: "lab-front", Name: "mgmt", Mode: "dhcp"}}},
		"another MAC":       map[string]any{"nics": []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", MAC: "52:54:00:00:00:02", Mode: "dhcp"}}},
		"one more NIC":      map[string]any{"nics": []machines.NIC{{Network: "lab-mgmt", Name: "mgmt"}, {Network: "lab-mgmt", Name: "eth1"}}},
		"bad version":       map[string]any{"version": "latest"},
		"too much memory":   map[string]any{"memory_mib": 1 << 30},
		"the name":          map[string]any{"name": "lb2"},
		"bad address":       map[string]any{"nics": []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", Mode: "static", Addresses: []string{"nope"}}}},
		"unknown extension": map[string]any{"extensions": []string{"not an extension!"}},
	} {
		if code, _ := patch(t, a, m.ID, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	// Nothing to change: answered at once.
	if code, _ := patch(t, a, m.ID, map[string]any{"vcpus": 2, "version": "v2026.10.02-4"}); code != http.StatusOK {
		t.Errorf("no-op: %d", code)
	}
	// An image-built machine keeps its image.
	if _, err := a.machines.Update(m.ID, func(m *machines.Machine) error {
		m.Spec.Image = &machines.ImageSource{URL: "https://x/y.qcow2", SHA256: strings.Repeat("b", 64)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := patch(t, a, m.ID, map[string]any{"version": "v2026.10.05"}); code != http.StatusBadRequest {
		t.Errorf("image-built machine's version: %d", code)
	}
	// Not ready: refused.
	if _, err := a.machines.Update(m.ID, func(m *machines.Machine) error { m.Phase = machines.PhaseFailed; return nil }); err != nil {
		t.Fatal(err)
	}
	if code, _ := patch(t, a, m.ID, map[string]any{"vcpus": 4}); code != http.StatusConflict {
		t.Errorf("failed machine: %d", code)
	}
}

func TestMachineUpdateFailureKeepsItReady(t *testing.T) {
	a, _ := newTestApp(t)
	f := &fakeNodes{version: "v2026.10.02-4", schematic: schematic.DefaultID(), upgradeErr: errors.New("the bundle isn't signed")}
	withFakeNodes(t, f)
	m := readyMachine(t, a, addTrustedHypervisor(t, a))
	if code, body := patch(t, a, m.ID, map[string]any{"version": "v2026.10.05"}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	if !strings.Contains(got.Error, "isn't signed") || got.Version != "v2026.10.02-4" || got.Spec.Version != "v2026.10.02-4" {
		t.Errorf("after a failed update: phase %s, error %q, version %s/%s", got.Phase, got.Error, got.Version, got.Spec.Version)
	}
}
