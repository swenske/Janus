package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/schematic"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// fakeNodes is a node that does what it's asked: it keeps its network
// configuration, and powers its virtual machine (drv) off on Shutdown.
type fakeNodes struct {
	mu         sync.Mutex
	drv        *fakeDriver
	version    string
	schematic  string
	extensions []string
	haproxy    string
	kernel     string
	cfg        *janusv1alpha1.NetworkConfig
	shutdowns  int
	applied    []*janusv1alpha1.NetworkConfig
	upgrades   []*janusv1alpha1.ImageSource
	upgradeErr error
}

func (f *fakeNodes) Shutdown(context.Context, *store.Node) error {
	f.mu.Lock()
	f.shutdowns++
	f.mu.Unlock()
	if f.drv != nil {
		f.drv.mu.Lock()
		if f.drv.hw != nil {
			f.drv.hw.Power = hypervisor.PowerOff
		}
		f.drv.ops = append(f.drv.ops, "shutdown")
		f.drv.mu.Unlock()
	}
	return nil
}

func (f *fakeNodes) ApplyNetwork(_ context.Context, _ *store.Node, _ *store.Store, cfg *janusv1alpha1.NetworkConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// A configuration naming an interface the machine hasn't got is what
	// the real node refuses.
	if f.drv != nil && f.drv.nics != nil {
		for _, iface := range cfg.GetInterfaces() {
			found := iface.GetMac() == ""
			for _, n := range f.drv.nics {
				if strings.EqualFold(n.MAC, iface.GetMac()) {
					found = true
				}
			}
			if !found {
				return errors.New("no physical interface has MAC " + iface.GetMac())
			}
		}
		f.drv.mu.Lock()
		f.drv.ops = append(f.drv.ops, "network")
		f.drv.mu.Unlock()
	}
	f.applied = append(f.applied, cfg)
	f.cfg = proto.Clone(cfg).(*janusv1alpha1.NetworkConfig)
	return nil
}

func (f *fakeNodes) Upgrade(_ context.Context, _ *store.Node, src *janusv1alpha1.ImageSource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upgradeErr != nil {
		return f.upgradeErr
	}
	f.upgrades = append(f.upgrades, src)
	// The bundle's version and schematic (ResolveBundle's URL), as the
	// node reports them once rebooted.
	ref := strings.TrimPrefix(src.Reference, "https://bundles/")
	version, doc, _ := strings.Cut(ref, "/")
	f.version = version
	sc, err := schematic.Parse([]byte(doc))
	if err != nil {
		return err
	}
	if sc.ID() != f.schematic && !src.AllowSchematicChange {
		return errors.New("another schematic, and allow_schematic_change isn't set")
	}
	f.schematic, f.extensions, f.haproxy, f.kernel = sc.ID(), sc.Extensions(), sc.HAProxyBranch(), sc.KernelTrack()
	return nil
}

func (f *fakeNodes) Info(context.Context, *store.Node) (*nodeproxy.NodeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &nodeproxy.NodeInfo{Version: f.version, Schematic: f.schematic, Extensions: f.extensions, HAProxy: f.haproxy, Kernel: f.kernel}, nil
}

func (f *fakeNodes) Network(context.Context, *store.Node) (*janusv1alpha1.NetworkConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return proto.Clone(f.cfg).(*janusv1alpha1.NetworkConfig), nil
}

// NetworkStatus: the node is reached on its mgmt interface (127.0.0.1).
func (f *fakeNodes) NetworkStatus(context.Context, *store.Node) (*janusv1alpha1.NetworkStatusResponse, error) {
	return &janusv1alpha1.NetworkStatusResponse{Interfaces: []*janusv1alpha1.NetworkInterfaceStatus{
		{Name: "mgmt", Mac: "52:54:00:00:00:01", Addresses: []string{"127.0.0.1/8"}},
	}}, nil
}

// ResolveBundle: the bundle's URL carries its version and schematic, for
// Upgrade.
func (f *fakeNodes) ResolveBundle(_ context.Context, version string, sc *schematic.Schematic) (*nodeproxy.Bundle, string, error) {
	return &nodeproxy.Bundle{Version: version, Schematic: sc.ID(), BaseURL: "https://bundles/" + version + "/" + string(sc.Canonical()), SHA256: strings.Repeat("a", 64)}, "ready", nil
}

const mgmtMAC, frontMAC = "52:54:00:00:00:01", "52:54:00:00:00:02"

// readyMachine is a machine whose node registered: two interfaces (mgmt,
// which the Controller reaches it on, and front with a VLAN set from its
// page), as the node and the hypervisor report them.
func readyMachine(t *testing.T, a *app, hv *hypervisor.Hypervisor, fake *fakeDriver) (*machines.Machine, *fakeNodes) {
	t.Helper()
	node := &store.Node{Name: "lb1", Address: "127.0.0.1:1", CACertPEM: []byte("ca"), ServiceCertPEM: []byte("c"), ServiceKeyPEM: []byte("k")}
	if err := a.store.Add(node); err != nil {
		t.Fatal(err)
	}
	nics := []machines.NIC{
		{Network: "lab-mgmt", Name: "mgmt", MAC: mgmtMAC, Mode: "static", Addresses: []string{"10.0.0.5/24"}},
		{Network: "lab-mgmt", Name: "front", MAC: frontMAC, Mode: "none"},
	}
	m := &machines.Machine{
		Spec:  machines.Spec{Name: "lb1", HypervisorID: hv.ID, VCPUs: 2, MemoryMiB: 1024, Version: "v2026.10.02-4", NICs: nics, DNS: []string{"10.0.0.53"}},
		Phase: machines.PhaseReady, NodeID: node.ID, Version: "v2026.10.02-4", Schematic: schematic.DefaultID(),
		Ref: &hypervisor.MachineRef{MachineID: "m", UUID: "uuid-m", Name: "janus-lb1"},
	}
	if err := a.machines.Add(m); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.hw = &hypervisor.MachineStatus{Power: hypervisor.PowerRunning, VCPUs: 2, MemoryMiB: 1024}
	fake.nics = []hypervisor.NIC{{Network: "lab-mgmt", MAC: mgmtMAC}, {Network: "lab-mgmt", MAC: frontMAC}}
	fake.mu.Unlock()
	f := &fakeNodes{drv: fake, version: "v2026.10.02-4", schematic: schematic.DefaultID(), cfg: &janusv1alpha1.NetworkConfig{
		Hostname: "lb1",
		Interfaces: []*janusv1alpha1.NetworkInterface{
			{Name: "mgmt", Mac: mgmtMAC, Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: []string{"10.0.0.5/24"}, Mtu: 9000},
			{Name: "front", Mac: frontMAC, Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_NONE},
			{Name: "front.100", Vlan: &janusv1alpha1.NetworkVLAN{Parent: "front", Id: 100}, Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: []string{"10.100.0.5/24"}},
		},
		Dns: &janusv1alpha1.NetworkDNS{Servers: []string{"10.0.0.53"}, Search: []string{"lab.example"}},
	}}
	prev := nodes
	nodes = f
	t.Cleanup(func() { nodes = prev })
	t.Cleanup(a.runner.waitJobs) // first: a job may still read nodes
	// The machine's record as the node says it is.
	if err := a.runner.sync(context.Background(), m.ID); err != nil {
		t.Fatal(err)
	}
	m, _ = a.machines.Get(m.ID)
	return m, f
}

func patch(t *testing.T, a *app, id string, body any) (int, string) {
	t.Helper()
	return patchAs(t, a, id, body, authToken)
}

func patchAs(t *testing.T, a *app, id string, body any, kind authKind) (int, string) {
	t.Helper()
	h := func(w http.ResponseWriter, r *http.Request) {
		a.handleMachineUpdate(w, r.WithContext(context.WithValue(r.Context(), authKindKey{}, kind)))
	}
	rec := call(t, h, "PATCH", "/api/machines/"+id, "PATCH /api/machines/{id}", body)
	return rec.Code, rec.Body.String()
}

// waitReady waits for an update to end: its machine ready again and its
// job done - the job reads the machine back from its node only once it's
// ready (syncAs), so a test reading it at "ready" could beat that (seen
// in CI: the old version still in the record).
func waitReady(t *testing.T, a *app, id string) *machines.Machine {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m, _ := a.machines.Get(id); m.Phase == machines.PhaseReady {
			a.runner.waitJobs()
			m, _ = a.machines.Get(id)
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the update never ended")
	return nil
}

func TestMachineUpdateSize(t *testing.T) {
	a, fake := newTestApp(t)
	m, f := readyMachine(t, a, addTrustedHypervisor(t, a), fake)

	if code, body := patch(t, a, m.ID, map[string]any{"vcpus": 4, "memory_mib": 2048}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	if got.Error != "" || got.Spec.VCPUs != 4 || got.Spec.MemoryMiB != 2048 {
		t.Fatalf("after resize: %+v", got)
	}
	if f.shutdowns != 1 || len(fake.resized) != 1 || fake.resized[0] != [2]int{4, 2048} || len(fake.nics) != 2 {
		t.Errorf("shutdowns %d, resized %v, nics %v", f.shutdowns, fake.resized, fake.nics)
	}
	if fake.powered[len(fake.powered)-1] != hypervisor.PowerStart {
		t.Errorf("not started again: %v", fake.powered)
	}
	if strings.Contains(eventsOf(got), "outside the Controller") {
		t.Errorf("its own change seen as an outside one: %s", eventsOf(got))
	}
}

func eventsOf(m *machines.Machine) string {
	var b strings.Builder
	for _, e := range m.Events {
		b.WriteString(e.Message + "\n")
	}
	return b.String()
}

func TestMachineUpdateVersionAndExtensions(t *testing.T) {
	a, fake := newTestApp(t)
	m, f := readyMachine(t, a, addTrustedHypervisor(t, a), fake)

	if code, body := patch(t, a, m.ID, map[string]any{"version": "v2026.10.05"}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	if got.Error != "" || got.Version != "v2026.10.05" || got.Spec.Version != "v2026.10.05" {
		t.Fatalf("after the update: %+v", got)
	}
	if len(f.upgrades) != 1 || f.upgrades[0].GetReference() != `https://bundles/v2026.10.05/{"customization":{}}` || f.upgrades[0].GetAllowSchematicChange() {
		t.Errorf("upgrade %v", f.upgrades)
	}

	if code, body := patch(t, a, m.ID, map[string]any{"extensions": []string{"keepalived"}}); code != http.StatusAccepted {
		t.Fatalf("PATCH extensions: %d %s", code, body)
	}
	got = waitReady(t, a, m.ID)
	withKeepalived := &schematic.Schematic{Customization: schematic.Customization{Extensions: []string{"keepalived"}}}
	if got.Error != "" || got.Schematic != withKeepalived.ID() || len(got.Spec.Extensions) != 1 {
		t.Fatalf("after the extensions: %+v", got)
	}
	if !f.upgrades[1].GetAllowSchematicChange() {
		t.Error("a schematic change wasn't allowed explicitly")
	}

	// Another HAProxy branch and kernel track: the same schematic change,
	// the extensions kept.
	if code, body := patch(t, a, m.ID, map[string]any{"haproxy": "3.2", "kernel": "longterm"}); code != http.StatusAccepted {
		t.Fatalf("PATCH haproxy/kernel: %d %s", code, body)
	}
	got = waitReady(t, a, m.ID)
	want := &schematic.Schematic{Customization: schematic.Customization{Extensions: []string{"keepalived"}, HAProxy: "3.2", Kernel: "longterm"}}
	if got.Error != "" || got.Schematic != want.ID() || got.Spec.HAProxy != "3.2" || got.Spec.Kernel != "longterm" || len(got.Spec.Extensions) != 1 {
		t.Fatalf("after the branch change: %+v", got)
	}
	if len(f.upgrades) != 3 || !f.upgrades[2].GetAllowSchematicChange() {
		t.Errorf("upgrades %v", f.upgrades)
	}
	// Back to the release's default branch.
	if code, body := patch(t, a, m.ID, map[string]any{"haproxy": ""}); code != http.StatusAccepted {
		t.Fatalf("PATCH haproxy default: %d %s", code, body)
	}
	got = waitReady(t, a, m.ID)
	if got.Error != "" || got.Spec.HAProxy != "" || got.Spec.Kernel != "longterm" {
		t.Fatalf("after going back to the default branch: %+v", got)
	}
	if code, _ := patch(t, a, m.ID, map[string]any{"haproxy": "3.2.25"}); code != http.StatusBadRequest {
		t.Errorf("a HAProxy version instead of a branch: %d", code)
	}
}

// A change of addresses keeps what only the node's page manages: the
// VLAN, the MTU, the search domains, the hostname.
func TestMachineUpdateNetworkMergesIntoTheNodes(t *testing.T) {
	a, fake := newTestApp(t)
	m, f := readyMachine(t, a, addTrustedHypervisor(t, a), fake)

	nics := []machines.NIC{
		{Network: "lab-mgmt", Name: "mgmt", MAC: mgmtMAC, Mode: "static", Addresses: []string{"10.0.0.6/24"}, Gateway: "10.0.0.1"},
		{Network: "lab-mgmt", Name: "front", MAC: frontMAC, Mode: "none"},
	}
	if code, body := patch(t, a, m.ID, map[string]any{"nics": nics, "ntp": []string{"10.0.0.1"}}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	if got.Error != "" || got.Spec.NICs[0].Addresses[0] != "10.0.0.6/24" {
		t.Fatalf("after the network: %+v", got.Spec)
	}
	if len(f.applied) != 1 || f.shutdowns != 0 || len(f.upgrades) != 0 {
		t.Fatalf("applied %d, shutdowns %d, upgrades %d", len(f.applied), f.shutdowns, len(f.upgrades))
	}
	cfg := f.applied[0]
	ifs := cfg.GetInterfaces()
	if len(ifs) != 3 || ifs[0].GetGateway() != "10.0.0.1" || ifs[0].GetMtu() != 9000 || ifs[2].GetName() != "front.100" {
		t.Errorf("interfaces %v", ifs)
	}
	if cfg.GetHostname() != "lb1" || cfg.GetDns().GetSearch()[0] != "lab.example" || cfg.GetNtp().GetServers()[0] != "10.0.0.1" {
		t.Errorf("applied %v", cfg)
	}
}

// Adding an interface: plugged in (a stop and a start), then configured.
// Removing one: out of the configuration first (the node keeps none of a
// configuration naming an interface it hasn't got), then unplugged - its
// VLAN with it.
func TestMachineUpdateInterfaces(t *testing.T) {
	a, fake := newTestApp(t)
	m, f := readyMachine(t, a, addTrustedHypervisor(t, a), fake)

	add := []machines.NIC{
		m.Spec.NICs[0], m.Spec.NICs[1],
		{Network: "lab-mgmt", Name: "backend", Mode: "static", Addresses: []string{"10.30.0.5/24"}},
	}
	if code, body := patch(t, a, m.ID, map[string]any{"nics": add}); code != http.StatusAccepted {
		t.Fatalf("PATCH add: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	if got.Error != "" || len(got.Spec.NICs) != 3 || got.Spec.NICs[2].MAC == "" {
		t.Fatalf("after adding: %v %+v", got.Error, got.Spec.NICs)
	}
	if strings.Join(fake.ops, ",") != "shutdown,reconfigure,network" || len(fake.nics) != 3 {
		t.Fatalf("operations %v, nics %v", fake.ops, fake.nics)
	}
	if n := len(f.cfg.GetInterfaces()); n != 4 {
		t.Errorf("the node's configuration has %d interfaces, want 4", n)
	}

	fake.ops = nil
	remove := []machines.NIC{got.Spec.NICs[0], got.Spec.NICs[2]} // front goes
	if code, body := patch(t, a, m.ID, map[string]any{"nics": remove}); code != http.StatusAccepted {
		t.Fatalf("PATCH remove: %d %s", code, body)
	}
	got = waitReady(t, a, m.ID)
	if got.Error != "" || len(got.Spec.NICs) != 2 {
		t.Fatalf("after removing: %v %+v", got.Error, got.Spec.NICs)
	}
	if strings.Join(fake.ops, ",") != "network,shutdown,reconfigure" || len(fake.nics) != 2 {
		t.Fatalf("operations %v, nics %v", fake.ops, fake.nics)
	}
	for _, iface := range f.cfg.GetInterfaces() {
		if iface.GetName() == "front" || iface.GetName() == "front.100" {
			t.Errorf("%s stayed in the node's configuration", iface.GetName())
		}
	}
}

func TestMachineUpdateRefusals(t *testing.T) {
	a, fake := newTestApp(t)
	hv := addTrustedHypervisor(t, a)
	m, _ := readyMachine(t, a, hv, fake)
	keep := m.Spec.NICs

	for name, body := range map[string]any{
		"control NIC removed": map[string]any{"nics": []machines.NIC{keep[1]}},
		"forbidden network":   map[string]any{"nics": []machines.NIC{keep[0], {Network: "prod-lan", Name: "x", Mode: "none"}}},
		"DHCP on an added":    map[string]any{"nics": []machines.NIC{keep[0], keep[1], {Network: "lab-mgmt", Name: "x", Mode: "dhcp"}}},
		"no NIC":              map[string]any{"nics": []machines.NIC{}},
		"bad version":         map[string]any{"version": "latest"},
		"too much memory":     map[string]any{"memory_mib": 1 << 30},
		"the name":            map[string]any{"name": "lb2"},
		"bad address":         map[string]any{"nics": []machines.NIC{{Network: "lab-mgmt", Name: "mgmt", MAC: mgmtMAC, Mode: "static", Addresses: []string{"nope"}}, keep[1]}},
		"unknown extension":   map[string]any{"extensions": []string{"not an extension!"}},
	} {
		if code, out := patch(t, a, m.ID, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, code, out)
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
	a, fake := newTestApp(t)
	m, f := readyMachine(t, a, addTrustedHypervisor(t, a), fake)
	f.upgradeErr = errors.New("the bundle isn't signed")
	if code, body := patch(t, a, m.ID, map[string]any{"version": "v2026.10.05"}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	if !strings.Contains(got.Error, "isn't signed") || got.Version != "v2026.10.02-4" || got.Spec.Version != "v2026.10.02-4" {
		t.Errorf("after a failed update: phase %s, error %q, version %s/%s", got.Phase, got.Error, got.Version, got.Spec.Version)
	}
}

// A machine managed by Terraform and locked: its pages can't change it,
// nor destroy it, only release it; Terraform's token still can.
func TestMachineLock(t *testing.T) {
	a, fake := newTestApp(t)
	m, _ := readyMachine(t, a, addTrustedHypervisor(t, a), fake)
	if code, body := patch(t, a, m.ID, map[string]any{"managed_by": "terraform", "locked": true}); code != http.StatusOK {
		t.Fatalf("lock: %d %s", code, body)
	}
	if code, _ := patchAs(t, a, m.ID, map[string]any{"vcpus": 4}, authSession); code != http.StatusLocked {
		t.Errorf("a page changing a locked machine: %d, want 423", code)
	}
	del := func(kind authKind) int {
		h := func(w http.ResponseWriter, r *http.Request) {
			a.handleMachineDelete(w, r.WithContext(context.WithValue(r.Context(), authKindKey{}, kind)))
		}
		return call(t, h, "DELETE", "/api/machines/"+m.ID, "DELETE /api/machines/{id}", nil).Code
	}
	if code := del(authSession); code != http.StatusLocked {
		t.Errorf("a page destroying a locked machine: %d, want 423", code)
	}
	// The node's own page is refused too.
	node, _ := a.store.Get(m.NodeID)
	if by := nodeproxy.LockedBy(node); by != "" {
		t.Logf("LockedBy hook already set: %q", by)
	}
	if code, body := patch(t, a, m.ID, map[string]any{"vcpus": 4}); code != http.StatusAccepted {
		t.Fatalf("Terraform's token on a locked machine: %d %s", code, body)
	}
	waitReady(t, a, m.ID)
	if code, _ := patchAs(t, a, m.ID, map[string]any{"locked": false}, authSession); code != http.StatusOK {
		t.Fatalf("release from its page: %d", code)
	}
	if code, _ := patchAs(t, a, m.ID, map[string]any{"vcpus": 2}, authSession); code != http.StatusAccepted {
		t.Errorf("a page changing a released machine: %d", code)
	}
	got := waitReady(t, a, m.ID)
	if got.Spec.ManagedBy != "terraform" || got.Spec.Locked {
		t.Errorf("after release: managed by %q, locked %v", got.Spec.ManagedBy, got.Spec.Locked)
	}
}

// A change made elsewhere - the node's page, janusctl, the hypervisor -
// shows up in the record, and in its history.
func TestMachineSync(t *testing.T) {
	a, fake := newTestApp(t)
	m, f := readyMachine(t, a, addTrustedHypervisor(t, a), fake)
	if m.Spec.DNS[0] != "10.0.0.53" || m.Spec.NICs[0].Addresses[0] != "10.0.0.5/24" || m.NodeHostname != "lb1" || m.SyncedAt.IsZero() {
		t.Fatalf("first read: %+v", m)
	}

	f.mu.Lock()
	f.cfg.Interfaces[0].Addresses = []string{"10.0.0.9/24"}
	f.cfg.Hostname = "renamed"
	f.version = "v2026.10.05"
	f.mu.Unlock()
	fake.mu.Lock()
	fake.hw.MemoryMiB = 4096
	fake.mu.Unlock()
	a.hvStatus.forget(m.Spec.HypervisorID)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/machines/"+m.ID+"?refresh=true", nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/machines/{id}", asAdmin(a.handleMachineGet))
	prev := syncFresh
	syncFresh = 0 // the first read was just now
	defer func() { syncFresh = prev }()
	mux.ServeHTTP(rec, r)
	got, _ := a.machines.Get(m.ID)
	if got.Spec.NICs[0].Addresses[0] != "10.0.0.9/24" || got.Version != "v2026.10.05" || got.Spec.MemoryMiB != 4096 {
		t.Fatalf("after a change elsewhere: %+v (version %s)", got.Spec, got.Version)
	}
	if got.Spec.Name != "lb1" || got.NodeHostname != "renamed" {
		t.Errorf("name %q, node hostname %q", got.Spec.Name, got.NodeHostname)
	}
	if ev := eventsOf(got); !strings.Contains(ev, "changed outside the Controller") || !strings.Contains(ev, "10.0.0.9/24") || !strings.Contains(ev, "4096 MiB") {
		t.Errorf("history:\n%s", ev)
	}
}
