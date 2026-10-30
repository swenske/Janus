package proxmox

import (
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

func TestHotplugMemoryAligned(t *testing.T) {
	for m, want := range map[int]bool{
		512: false, 1024: true, 1536: true, 1300: false, 2048: true,
		17408: true, 17920: false, 18432: true, 20480: true,
	} {
		if got := hotplugMemoryAligned(m); got != want {
			t.Errorf("hotplugMemoryAligned(%d) = %v, want %v", m, got, want)
		}
	}
}

func TestCheckHardware(t *testing.T) {
	for _, tc := range []struct {
		hw hypervisor.Hardware
		ok bool
	}{
		{hypervisor.Hardware{VCPUs: 2, MemoryMiB: 768}, true}, // fixed: anything
		{hypervisor.Hardware{VCPUs: 1, MemoryMiB: 1024, MaxVCPUs: 4, MaxMemoryMiB: 4096}, true},
		{hypervisor.Hardware{VCPUs: 1, MemoryMiB: 768, MaxMemoryMiB: 4096}, false},  // below the static 1024
		{hypervisor.Hardware{VCPUs: 1, MemoryMiB: 1024, MaxMemoryMiB: 3000}, false}, // not a module boundary
		{hypervisor.Hardware{VCPUs: 1, MemoryMiB: 768, MaxVCPUs: 4}, true},          // vCPUs only
	} {
		if err := checkHardware(tc.hw); (err == nil) != tc.ok {
			t.Errorf("checkHardware(%+v) = %v", tc.hw, err)
		}
	}
}

func TestHardwareParams(t *testing.T) {
	p, del := hardwareParams(nil, hypervisor.Hardware{VCPUs: 2, MemoryMiB: 1024, MaxVCPUs: 4, MaxMemoryMiB: 4096})
	want := map[string]string{"cores": "4", "vcpus": "2", "sockets": "1", "memory": "1024", "numa": "1", "hotplug": "network,disk,usb,cpu,memory"}
	for k, v := range want {
		if p.Get(k) != v {
			t.Errorf("scalable: %s = %q, want %q", k, p.Get(k), v)
		}
	}
	if len(p) != len(want) || len(del) != 0 {
		t.Errorf("scalable: %v, deletes %v", p, del)
	}

	// Memory only: every vCPU plugged, no cpu hot-plug.
	p, _ = hardwareParams(nil, hypervisor.Hardware{VCPUs: 2, MemoryMiB: 1024, MaxMemoryMiB: 2048})
	if p.Get("cores") != "2" || p.Get("vcpus") != "" || p.Get("hotplug") != "network,disk,usb,memory" {
		t.Errorf("memory only: %v", p)
	}

	// Back to fixed: what made it scalable is deleted, if it's there.
	cfg := map[string]any{"cores": 4, "vcpus": 2, "numa": 1, "hotplug": "network,disk,usb,cpu,memory", "memory": "1024"}
	p, del = hardwareParams(cfg, hypervisor.Hardware{VCPUs: 2, MemoryMiB: 2048})
	if p.Get("cores") != "2" || p.Get("vcpus") != "" || p.Get("numa") != "" || p.Get("hotplug") != "" || len(del) != 3 {
		t.Errorf("fixed again: %v, deletes %v", p, del)
	}
	if _, del = hardwareParams(map[string]any{"cores": 2}, hypervisor.Hardware{VCPUs: 2, MemoryMiB: 2048}); len(del) != 0 {
		t.Errorf("deletes what isn't there: %v", del)
	}

	// Through reconfigureParams, with an interface removed: one delete.
	p, err := reconfigureParams(map[string]any{"vcpus": 2, "net1": "virtio=52:54:00:AA:BB:02,bridge=vmbr0"}, hypervisor.Hardware{VCPUs: 1, MemoryMiB: 1024}, nil, hypervisor.Settings{}, hypervisor.DefaultProxmoxCPUModel)
	if err != nil || p.Get("delete") != "net1,vcpus" {
		t.Errorf("reconfigureParams deletes %q (%v)", p.Get("delete"), err)
	}
}

func TestStatusOf(t *testing.T) {
	c := &vmConfig{Cores: 4, Sockets: 1, Vcpus: 2, Numa: 1, Hotplug: "network,disk,usb,cpu,memory", Memory: "current=3072"}
	v, maxV, m, maxM, step := statusOf(c)
	if v != 2 || maxV != 4 || m != 3072 || maxM != 0 || step != 512 {
		t.Errorf("scalable: %d/%d vCPUs, %d/%d MiB, step %d", v, maxV, m, maxM, step)
	}
	c = &vmConfig{Cores: 2, Memory: float64(1024)}
	v, maxV, m, maxM, step = statusOf(c)
	if v != 2 || maxV != 2 || m != 1024 || maxM != 1024 || step != 0 {
		t.Errorf("fixed: %d/%d vCPUs, %d/%d MiB, step %d", v, maxV, m, maxM, step)
	}
	if (&vmConfig{Hotplug: "1"}).hotplugs("memory") || !(&vmConfig{}).hotplugs("disk") {
		t.Error("hotplug defaults")
	}
}
