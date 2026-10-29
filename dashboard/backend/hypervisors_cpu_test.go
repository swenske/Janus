package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
	"github.com/swenske/Janus/dashboard/backend/internal/machines"
)

func pveBody(model string) map[string]any {
	p := map[string]any{
		"url": "https://pve1.example.net", "node": "pve1", "token_id": "janus-ctl@pve!controller", "pool": "janus",
		"storage": "local-lvm", "image_storage": "janus-images", "networks": []string{"lab-mgmt"},
		"ca_cert": "",
	}
	if model != "" {
		p["cpu_model"] = model
	}
	return map[string]any{"name": "pve1", "token_secret": "s3cr3t-0000", "proxmox": p}
}

func hypervisorOf(t *testing.T, code int, body string) hypervisorView {
	t.Helper()
	var v hypervisorView
	if code != http.StatusOK && code != http.StatusCreated || json.Unmarshal([]byte(body), &v) != nil || v.Proxmox == nil {
		t.Fatalf("%d %s", code, body)
	}
	return v
}

// A Proxmox hypervisor's CPU model: x86-64-v2-AES unless given - shown
// as such, not written down -, validated against the names Proxmox
// VE knows with AES-NI, kept when a change leaves it out (a client that
// doesn't know it), and absent from the host's preparation.
func TestProxmoxHypervisorCPUModelAPI(t *testing.T) {
	a, _ := newTestApp(t)
	create := func(body any) (int, string) {
		rec := call(t, a.handleHypervisorCreate, "POST", "/api/hypervisors", "POST /api/hypervisors", body)
		return rec.Code, rec.Body.String()
	}
	patch := func(id string, body any) (int, string) {
		rec := call(t, a.handleHypervisorUpdate, "PATCH", "/api/hypervisors/"+id, "PATCH /api/hypervisors/{id}", body)
		return rec.Code, rec.Body.String()
	}
	prep := func(body any) string {
		rec := call(t, a.handleHypervisorPreparation, "POST", "/api/hypervisors/preparation", "POST /api/hypervisors/preparation", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("preparation: %d %s", rec.Code, rec.Body)
		}
		return rec.Body.String()
	}

	v := hypervisorOf(t, http.StatusCreated, func() string { _, b := create(pveBody("")); return b }())
	if v.Proxmox.CPUModel != "x86-64-v2-AES" {
		t.Errorf("default model shown: %q", v.Proxmox.CPUModel)
	}
	meta := filepath.Join(a.dataDir, "hypervisors", v.ID, "meta.json")
	if raw, _ := os.ReadFile(meta); strings.Contains(string(raw), "cpu_model") {
		t.Errorf("the default written down: %s", raw)
	}

	for _, tc := range []struct{ model, refusal string }{
		{"kvm64", "no AES-NI"}, {"qemu64", "no AES-NI"}, {"x86-64-v2", "no AES-NI"},
		{"x86-64-V3", "isn't one the Controller gives"}, {"custom-janus", "isn't one"}, {"max", "isn't one"},
		{"host,flags=+aes", "isn't one"}, {"Penryn", "isn't one"},
	} {
		if code, body := create(pveBody(tc.model)); code != http.StatusBadRequest || !strings.Contains(body, tc.refusal) {
			t.Errorf("create with %s: %d %s", tc.model, code, body)
		}
		if code, body := patch(v.ID, pveBody(tc.model)); code != http.StatusBadRequest || !strings.Contains(body, tc.refusal) {
			t.Errorf("change to %s: %d %s", tc.model, code, body)
		}
	}

	for _, model := range []string{"x86-64-v3", "x86-64-v4", "host", "EPYC-Rome", "x86-64-v2-AES"} {
		code, body := patch(v.ID, pveBody(model))
		if got := hypervisorOf(t, code, body); got.Proxmox.CPUModel != model {
			t.Errorf("changed to %s: %q", model, got.Proxmox.CPUModel)
		}
		if h, _ := a.hypervisors.Get(v.ID); h.Proxmox.CPU() != model {
			t.Errorf("stored %q, want %s", h.Proxmox.CPUModel, model)
		}
	}
	patch(v.ID, pveBody("host"))
	// Left out: kept - the settings without it, or the labels alone.
	code, body := patch(v.ID, pveBody(""))
	if got := hypervisorOf(t, code, body); got.Proxmox.CPUModel != "host" {
		t.Errorf("left out of a change: %q", got.Proxmox.CPUModel)
	}
	code, body = patch(v.ID, map[string]any{"labels": map[string]string{"dc": "lgs"}})
	if got := hypervisorOf(t, code, body); got.Proxmox.CPUModel != "host" {
		t.Errorf("after a change of labels: %q", got.Proxmox.CPUModel)
	}
	// The list says it too, and a Controller restarted reads it back.
	rec := call(t, a.handleHypervisorList, "GET", "/api/hypervisors", "GET /api/hypervisors", nil)
	if !strings.Contains(rec.Body.String(), `"cpu_model":"host"`) {
		t.Errorf("list: %s", rec.Body)
	}
	again, err := hypervisor.Open(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if h, ok := again.Get(v.ID); !ok || h.Proxmox.CPUModel != "host" {
		t.Errorf("read back: %+v", h)
	}

	// The host's preparation doesn't name it: no right more for any.
	if with, without := prep(pveBody("host")), prep(pveBody("")); with != without {
		t.Errorf("the preparation changes with the model:\n%s\n%s", with, without)
	}
}

// A hypervisor's new CPU model restarts nothing: its machines show the
// difference at their next sync, said once in their history, and get it
// only when asked - Apply settings.
func TestHypervisorCPUModelChangeRestartsNothing(t *testing.T) {
	a, fake := newTestApp(t)
	code, body := func() (int, string) {
		rec := call(t, a.handleHypervisorCreate, "POST", "/api/hypervisors", "POST /api/hypervisors", pveBody(""))
		return rec.Code, rec.Body.String()
	}()
	v := hypervisorOf(t, code, body)
	h, _ := a.hypervisors.Get(v.ID)
	m, f := readyMachine(t, a, h, fake)

	// The fake hypervisor's machine has the model it was created with;
	// what it reports differs from the record's setting as the Proxmox
	// driver's does (cpuDrift, tested there), and its host may not run
	// the setting's (missingCPUFlags, tested there).
	have, hostLacks := "x86-64-v2-AES", ""
	prev := newDriver
	newDriver = func(h *hypervisor.Hypervisor, _ string) (hypervisor.Driver, error) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.drift = nil
		if want := h.Proxmox.CPU(); want != have && len(fake.settings) == 0 {
			fake.drift = []hypervisor.Drift{{Setting: "cpu", Have: have, Want: want}}
		}
		return cpuHost{fake, hostLacks}, nil
	}
	t.Cleanup(func() { newDriver = prev })
	sync := func() *machines.Machine {
		t.Helper()
		a.hvStatus.forget(h.ID)
		if err := a.runner.sync(context.Background(), m.ID); err != nil {
			t.Fatal(err)
		}
		got, _ := a.machines.Get(m.ID)
		return got
	}
	if got := sync(); len(got.HardwareDrift) != 0 {
		t.Fatalf("drift before the change: %+v", got.HardwareDrift)
	}

	rec := call(t, a.handleHypervisorUpdate, "PATCH", "/api/hypervisors/"+h.ID, "PATCH /api/hypervisors/{id}", pveBody("x86-64-v3"))
	if rec.Code != http.StatusOK {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}
	for range 3 {
		sync()
	}
	got := sync()
	if len(got.HardwareDrift) != 1 || got.HardwareDrift[0].String() != "cpu x86-64-v2-AES → x86-64-v3" {
		t.Fatalf("drift: %+v", got.HardwareDrift)
	}
	if n := strings.Count(eventsOf(got), "(cpu x86-64-v2-AES → x86-64-v3)"); n != 1 {
		t.Errorf("said %d times:\n%s", n, eventsOf(got))
	}
	fake.mu.Lock()
	touched := len(fake.powered) + len(fake.settings)
	fake.mu.Unlock()
	if f.shutdowns != 0 || touched != 0 {
		t.Fatalf("the change touched the machine: %d shutdowns, %d power/reconfigure", f.shutdowns, touched)
	}

	// The host can't run it: refused before the node is stopped.
	hostLacks = "CPU model x86-64-v3 needs avx2, which pve1's processor hasn't got"
	a.hvStatus.forget(h.ID)
	if code, body := applySettings(t, a, m.ID); code != http.StatusConflict || !strings.Contains(body, "needs avx2") {
		t.Fatalf("apply-settings on a host without AVX2: %d %s", code, body)
	}
	if got, _ := a.machines.Get(m.ID); f.shutdowns != 0 || got.Phase != machines.PhaseReady {
		t.Fatalf("refused, yet %d shutdowns, phase %s", f.shutdowns, got.Phase)
	}
	hostLacks = ""
	a.hvStatus.forget(h.ID)

	// Asked for: a clean shutdown, Reconfigure, a start.
	if code, body := applySettings(t, a, m.ID); code != http.StatusAccepted {
		t.Fatalf("apply-settings: %d %s", code, body)
	}
	got = waitPhase(t, a, m.ID, machines.PhaseReady)
	fake.mu.Lock()
	reconfigured := len(fake.settings)
	fake.mu.Unlock()
	if got.Error != "" || reconfigured != 1 || f.shutdowns != 1 {
		t.Fatalf("applied: error %q, %d reconfigure, %d shutdowns", got.Error, reconfigured, f.shutdowns)
	}
	if got = sync(); len(got.HardwareDrift) != 0 {
		t.Errorf("drift after Apply settings: %+v", got.HardwareDrift)
	}
}

// cpuHost is a fake hypervisor whose host can't run its machines' CPU
// model when lacks says why.
type cpuHost struct {
	*fakeDriver
	lacks string
}

func (c cpuHost) HostInfo(ctx context.Context) (*hypervisor.HostInfo, error) {
	info, err := c.fakeDriver.HostInfo(ctx)
	if err == nil && c.lacks != "" {
		cp := *info
		cp.MachineCPUError = c.lacks
		info = &cp
	}
	return info, err
}
