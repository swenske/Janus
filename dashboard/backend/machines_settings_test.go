package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/internal/clusters"
	"github.com/swenske/Janus/internal/schematic"
)

// A machine is created with the guest agent's setting when its schematic
// has the extension - not otherwise, nor from an image URL, whose
// extensions aren't known yet (#373).
func TestMachineCreateGuestAgent(t *testing.T) {
	a, fake := newTestApp(t)
	// Their consoles stay open while they wait for their nodes: none
	// opened again, with newDriver, while the test puts it back.
	fake.console = "booting\n"
	h := addTrustedHypervisor(t, a)
	image := append([]byte("QFI\xfb\x00\x00\x00\x03"), make([]byte, 4096)...)
	sum := sha256.Sum256(image)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(image) }))
	defer srv.Close()
	prev := resolveVMImage
	t.Cleanup(func() { resolveVMImage = prev })
	resolveVMImage = func(_ context.Context, version string, sc *schematic.Schematic, _ string) (*nodeproxy.VMImage, error) {
		return &nodeproxy.VMImage{State: "ready", Version: "v2026.10.28", Schematic: sc.ID(), URL: srv.URL + "/janus.qcow2", SHA256: hex.EncodeToString(sum[:])}, nil
	}
	for _, tc := range []struct {
		name  string
		spec  machines.Spec
		agent bool
	}{
		{"agent", machines.Spec{Extensions: []string{"keepalived", "qemu-guest-agent"}}, true},
		{"noagent", machines.Spec{Extensions: []string{"keepalived"}}, false},
		{"byurl", machines.Spec{Image: &machines.ImageSource{URL: srv.URL + "/janus-kvm.qcow2", SHA256: hex.EncodeToString(sum[:])}}, false},
	} {
		spec := tc.spec
		spec.Name, spec.HypervisorID = tc.name, h.ID
		spec.NICs = []machines.NIC{{Network: "lab-mgmt", Name: "mgmt"}}
		rec := call(t, a.handleMachineCreate, "POST", "/api/machines", "POST /api/machines", spec)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: create: %d %s", tc.name, rec.Code, rec.Body)
		}
		var v machineView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		m := waitPhase(t, a, v.ID, machines.PhaseRegistering)
		fake.mu.Lock()
		got := fake.vms[m.Ref.UUID]
		fake.mu.Unlock()
		if got.GuestAgent != tc.agent {
			t.Errorf("%s: guest agent %v, want %v", tc.name, got.GuestAgent, tc.agent)
		}
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		fake.mu.Lock()
		n := fake.consoles
		fake.mu.Unlock()
		if n == 3 {
			return
		}
	}
	t.Error("the waiting machines' consoles weren't opened")
}

// settingsMachine is a ready machine on a hypervisor with a guest agent
// option (Proxmox's), off, and an older Controller's CPU model; its node
// runs extensions.
func settingsMachine(t *testing.T, extensions ...string) (*app, *fakeDriver, *machines.Machine, *fakeNodes) {
	t.Helper()
	a, fake := newTestApp(t)
	off := false
	fake.agent = &off
	fake.drift = []hypervisor.Drift{{Setting: "cpu", Have: "kvm64 (Proxmox's default)", Want: "x86-64-v2-AES"}}
	m, f := readyMachine(t, a, addTrustedHypervisor(t, a), fake)
	if len(extensions) > 0 {
		sc := &schematic.Schematic{Customization: schematic.Customization{Extensions: extensions}}
		f.mu.Lock()
		f.schematic, f.extensions = sc.ID(), sc.Extensions()
		f.mu.Unlock()
		a.hvStatus.forget(m.Spec.HypervisorID)
		if err := a.runner.sync(context.Background(), m.ID); err != nil {
			t.Fatal(err)
		}
		m, _ = a.machines.Get(m.ID)
	}
	return a, fake, m, f
}

func applySettings(t *testing.T, a *app, id string) (int, string) {
	t.Helper()
	rec := call(t, a.handleMachineApplySettings, "POST", "/api/machines/"+id+"/apply-settings", "POST /api/machines/{id}/apply-settings", nil)
	return rec.Code, rec.Body.String()
}

// The sync reports the settings that aren't today's - never adopts them,
// never restarts anything -, and says so in the history once.
func TestMachineSyncReportsHardwareDrift(t *testing.T) {
	a, fake, m, f := settingsMachine(t, "qemu-guest-agent")
	want := []hypervisor.Drift{
		{Setting: "cpu", Have: "kvm64 (Proxmox's default)", Want: "x86-64-v2-AES"},
		{Setting: "guest-agent", Have: "off", Want: "on"},
	}
	if len(m.HardwareDrift) != 2 || m.HardwareDrift[0] != want[0] || m.HardwareDrift[1] != want[1] {
		t.Fatalf("drift %+v", m.HardwareDrift)
	}
	said := strings.Count(eventsOf(m), "hypervisor settings aren't the ones")
	for range 3 {
		a.hvStatus.forget(m.Spec.HypervisorID)
		if err := a.runner.sync(context.Background(), m.ID); err != nil {
			t.Fatal(err)
		}
	}
	m, _ = a.machines.Get(m.ID)
	if n := strings.Count(eventsOf(m), "hypervisor settings aren't the ones"); n != said || said == 0 {
		t.Errorf("said %d times, then %d:\n%s", said, n, eventsOf(m))
	}
	if !strings.Contains(eventsOf(m), "(cpu kvm64 (Proxmox's default) → x86-64-v2-AES; guest-agent off → on)") {
		t.Errorf("history:\n%s", eventsOf(m))
	}
	if f.shutdowns != 0 || len(fake.powered) != 0 || len(fake.settings) != 0 {
		t.Errorf("the sync touched the machine: %d shutdowns, powered %v, reconfigured %v", f.shutdowns, fake.powered, fake.settings)
	}
	var v machineView
	rec := call(t, a.handleMachineGet, "GET", "/api/machines/"+m.ID, "GET /api/machines/{id}", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || len(v.HardwareDrift) != 2 {
		t.Errorf("the machine's view: %s", rec.Body)
	}

	// Turned on by hand on the hypervisor: the drift goes, and the history
	// says so.
	on := true
	fake.mu.Lock()
	fake.agent, fake.drift = &on, nil
	fake.mu.Unlock()
	a.hvStatus.forget(m.Spec.HypervisorID)
	if err := a.runner.sync(context.Background(), m.ID); err != nil {
		t.Fatal(err)
	}
	m, _ = a.machines.Get(m.ID)
	if len(m.HardwareDrift) != 0 || !strings.Contains(eventsOf(m), "its hypervisor settings are the ones the Controller gives machines today") {
		t.Errorf("drift %+v, history:\n%s", m.HardwareDrift, eventsOf(m))
	}

	// A hypervisor without the option (libvirt): no guest agent drift.
	fake.mu.Lock()
	fake.agent = nil
	fake.mu.Unlock()
	a.hvStatus.forget(m.Spec.HypervisorID)
	_ = a.runner.sync(context.Background(), m.ID)
	if m, _ = a.machines.Get(m.ID); len(m.HardwareDrift) != 0 {
		t.Errorf("libvirt: drift %+v", m.HardwareDrift)
	}
}

// Applying them, when asked: a cold restart through the node's clean
// shutdown, at the machine's size, its settings given to Reconfigure -
// then nothing left to apply.
func TestMachineApplySettings(t *testing.T) {
	a, fake, m, f := settingsMachine(t, "qemu-guest-agent")
	if code, body := applySettings(t, a, m.ID); code != http.StatusAccepted {
		t.Fatalf("apply: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	if got.Error != "" || len(got.HardwareDrift) != 0 {
		t.Fatalf("after applying: error %q, drift %+v", got.Error, got.HardwareDrift)
	}
	fake.mu.Lock()
	ops, settings, resized := strings.Join(fake.ops, ","), fake.settings, fake.resized
	fake.mu.Unlock()
	if f.shutdowns != 1 || ops != "shutdown,reconfigure" || len(settings) != 1 || !settings[0].GuestAgent || len(resized) != 1 || resized[0] != [2]int{2, 1024} {
		t.Errorf("shutdowns %d, ops %s, settings %+v, resized %v", f.shutdowns, ops, settings, resized)
	}
	if fake.powered[len(fake.powered)-1] != hypervisor.PowerStart {
		t.Errorf("not started again: %v", fake.powered)
	}
	ev := eventsOf(got)
	for _, want := range []string{
		"applying its hypervisor settings (cpu kvm64 (Proxmox's default) → x86-64-v2-AES; guest-agent off → on)",
		"shutting the node down to apply its hypervisor settings: cpu kvm64 (Proxmox's default) → x86-64-v2-AES; guest-agent off → on",
		"its hypervisor settings are the ones the Controller gives machines today",
		"updated",
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("history without %q:\n%s", want, ev)
		}
	}
	if got.Spec.VCPUs != m.Spec.VCPUs || got.Spec.MemoryMiB != m.Spec.MemoryMiB || len(got.Spec.Extensions) != 1 {
		t.Errorf("spec changed: %+v", got.Spec)
	}

	// Nothing left: 200, the node untouched.
	if code, body := applySettings(t, a, m.ID); code != http.StatusOK {
		t.Fatalf("again: %d %s", code, body)
	}
	if f.shutdowns != 1 {
		t.Errorf("restarted with nothing to apply: %d shutdowns", f.shutdowns)
	}
}

// A cluster member in service isn't stopped: drained first - as its node
// says, or as the Controller last saw it when the node doesn't answer.
func TestMachineApplySettingsClusterMember(t *testing.T) {
	a, fake, m, f := settingsMachine(t)
	st, err := clusters.Open(a.dataDir, testSealer{})
	if err != nil {
		t.Fatal(err)
	}
	a.clusters = newClusterEngine(a, st)
	cf := &clusterFake{nodes: map[string]*fakeMember{"lb1": {}}, fail: map[string]error{}, unreach: map[[2]string]bool{}, byAddr: map[string]string{}, notAfter: time.Now().Add(time.Hour)}
	a.clusters.Nodes = cf
	t.Cleanup(a.clusters.wait)
	c, err := st.Create("edge", 10057, []string{"haproxy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(c.ID, func(c *clusters.Cluster) error {
		c.Members = append(c.Members, clusters.Member{NodeID: m.NodeID, Name: "lb1", Address: "10.0.0.5"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	code, body := applySettings(t, a, m.ID)
	if code != http.StatusConflict || !strings.Contains(body, "a member of cluster edge in service: drain it first") {
		t.Fatalf("undrained member: %d %s", code, body)
	}
	// The Controller saw it drained, but its node says it's back in
	// service (#356): refused.
	if err := st.UpdateState(c.ID, func(s *clusters.State) { s.Members["lb1"] = clusters.MemberState{Drained: true} }); err != nil {
		t.Fatal(err)
	}
	if code, _ := applySettings(t, a, m.ID); code != http.StatusConflict {
		t.Fatalf("drained in the record, not on the node: %d", code)
	}
	if f.shutdowns != 0 {
		t.Fatalf("a member in service was stopped")
	}
	// The node doesn't answer: the record's word.
	cf.mu.Lock()
	cf.fail["lb1 ClusterGet"] = context.DeadlineExceeded
	cf.mu.Unlock()
	if code, body := applySettings(t, a, m.ID); code != http.StatusAccepted {
		t.Fatalf("drained in the record, node silent: %d %s", code, body)
	}
	waitReady(t, a, m.ID)
	// Drained on the node: applied.
	cf.mu.Lock()
	delete(cf.fail, "lb1 ClusterGet")
	cf.nodes["lb1"].drained = true
	cf.mu.Unlock()
	fake.mu.Lock()
	fake.drift = []hypervisor.Drift{{Setting: "cpu", Have: "kvm64", Want: "x86-64-v2-AES"}}
	fake.mu.Unlock()
	if code, body := applySettings(t, a, m.ID); code != http.StatusAccepted {
		t.Fatalf("drained member: %d %s", code, body)
	}
	if got := waitReady(t, a, m.ID); got.Error != "" || f.shutdowns != 2 {
		t.Errorf("error %q, %d shutdowns", got.Error, f.shutdowns)
	}
}

// Adding or removing the guest agent's extension on a hypervisor with an
// option for it: the option follows, at a stop and a start before the
// update - none on one without (libvirt), none when the option already
// says so, none for an update that doesn't change it.
func TestMachineUpdateGuestAgent(t *testing.T) {
	a, fake, m, f := settingsMachine(t)
	fake.mu.Lock()
	fake.drift = nil
	fake.mu.Unlock()

	if code, body := patch(t, a, m.ID, map[string]any{"extensions": []string{"qemu-guest-agent"}}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	fake.mu.Lock()
	ops, settings := strings.Join(fake.ops, ","), append([]hypervisor.Settings(nil), fake.settings...)
	fake.mu.Unlock()
	if got.Error != "" || len(f.upgrades) != 1 || f.shutdowns != 1 || ops != "shutdown,reconfigure" || len(settings) != 1 || !settings[0].GuestAgent {
		t.Fatalf("adding the agent: error %q, %d upgrades, %d shutdowns, ops %s, settings %+v", got.Error, len(f.upgrades), f.shutdowns, ops, settings)
	}
	if len(got.HardwareDrift) != 0 || !strings.Contains(eventsOf(got), "shutting the node down to apply its hypervisor settings: guest-agent off → on") {
		t.Errorf("drift %+v, history:\n%s", got.HardwareDrift, eventsOf(got))
	}

	// A version alone: the agent stays as it is, no restart.
	if code, body := patch(t, a, m.ID, map[string]any{"version": "v2026.10.05"}); code != http.StatusAccepted {
		t.Fatalf("PATCH version: %d %s", code, body)
	}
	if got = waitReady(t, a, m.ID); got.Error != "" || len(f.upgrades) != 2 || f.shutdowns != 1 {
		t.Fatalf("version: error %q, %d upgrades, %d shutdowns", got.Error, len(f.upgrades), f.shutdowns)
	}

	// Removed: cleared the same way.
	if code, body := patch(t, a, m.ID, map[string]any{"extensions": []string{}}); code != http.StatusAccepted {
		t.Fatalf("PATCH no extensions: %d %s", code, body)
	}
	got = waitReady(t, a, m.ID)
	fake.mu.Lock()
	settings = append([]hypervisor.Settings(nil), fake.settings...)
	agent := *fake.agent
	fake.mu.Unlock()
	if got.Error != "" || f.shutdowns != 2 || len(settings) != 2 || settings[1].GuestAgent || agent || len(got.HardwareDrift) != 0 {
		t.Fatalf("removing the agent: error %q, %d shutdowns, settings %+v, agent %v, drift %+v", got.Error, f.shutdowns, settings, agent, got.HardwareDrift)
	}

	// Set by hand already: nothing to restart for.
	on := true
	fake.mu.Lock()
	fake.agent = &on
	fake.mu.Unlock()
	a.hvStatus.forget(m.Spec.HypervisorID)
	if code, body := patch(t, a, m.ID, map[string]any{"extensions": []string{"qemu-guest-agent"}}); code != http.StatusAccepted {
		t.Fatalf("PATCH again: %d %s", code, body)
	}
	if got = waitReady(t, a, m.ID); got.Error != "" || f.shutdowns != 2 || len(f.upgrades) != 4 {
		t.Fatalf("option already on: error %q, %d shutdowns, %d upgrades", got.Error, f.shutdowns, len(f.upgrades))
	}

	// No option (libvirt): the update alone.
	fake.mu.Lock()
	fake.agent = nil
	fake.mu.Unlock()
	if code, body := patch(t, a, m.ID, map[string]any{"extensions": []string{}}); code != http.StatusAccepted {
		t.Fatalf("PATCH libvirt: %d %s", code, body)
	}
	if got = waitReady(t, a, m.ID); got.Error != "" || f.shutdowns != 2 || len(f.upgrades) != 5 {
		t.Fatalf("libvirt: error %q, %d shutdowns, %d upgrades", got.Error, f.shutdowns, len(f.upgrades))
	}
}

// A hardware change brings the settings along in the same restart, and
// says so.
func TestMachineUpdateSizeAppliesSettings(t *testing.T) {
	a, fake, m, f := settingsMachine(t, "qemu-guest-agent")
	if code, body := patch(t, a, m.ID, map[string]any{"vcpus": 4}); code != http.StatusAccepted {
		t.Fatalf("PATCH: %d %s", code, body)
	}
	got := waitReady(t, a, m.ID)
	fake.mu.Lock()
	settings := fake.settings
	fake.mu.Unlock()
	if got.Error != "" || f.shutdowns != 1 || len(settings) != 1 || !settings[0].GuestAgent || len(got.HardwareDrift) != 0 {
		t.Fatalf("error %q, %d shutdowns, settings %+v, drift %+v", got.Error, f.shutdowns, settings, got.HardwareDrift)
	}
	if !strings.Contains(eventsOf(got), "2 interfaces - and its hypervisor settings: cpu kvm64 (Proxmox's default) → x86-64-v2-AES; guest-agent off → on") {
		t.Errorf("history:\n%s", eventsOf(got))
	}
}
