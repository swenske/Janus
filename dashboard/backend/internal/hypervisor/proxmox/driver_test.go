package proxmox

import (
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

func TestNICParam(t *testing.T) {
	for _, tc := range []struct{ network, want string }{
		{"vmbr0", "virtio=52:54:00:AA:BB:01,bridge=vmbr0"},
		{"vmbr0.20", "virtio=52:54:00:AA:BB:01,bridge=vmbr0,tag=20"},
		{"lan", "virtio=52:54:00:AA:BB:01,bridge=lan"},
	} {
		got, err := nicParam(hypervisor.NIC{Network: tc.network, MAC: "52:54:00:aa:bb:01"})
		if err != nil || got != tc.want {
			t.Errorf("%s: %q %v, want %q", tc.network, got, err, tc.want)
		}
	}
	for _, bad := range []string{"vmbr0.0", "vmbr0.4095", "vm br0", "0vmbr", "vmbr0.20.3"} {
		if _, err := nicParam(hypervisor.NIC{Network: bad, MAC: "52:54:00:aa:bb:01"}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Interfaces are matched by MAC: one gone is deleted, one moved keeps its
// slot and MAC, one new takes the first free slot.
func TestReconfigureParams(t *testing.T) {
	cfg := map[string]any{
		"net0":   "virtio=52:54:00:AA:BB:01,bridge=vmbr0,tag=10",
		"net1":   "virtio=52:54:00:AA:BB:02,bridge=vmbr0,tag=20",
		"net3":   "virtio=52:54:00:AA:BB:04,bridge=vmbr0,tag=30",
		"cores":  1,
		"memory": "1024",
	}
	p, err := reconfigureParams(cfg, 2, 2048, []hypervisor.NIC{
		{Network: "vmbr0.10", MAC: "52:54:00:aa:bb:01"}, // kept
		{Network: "vmbr0.30", MAC: "52:54:00:aa:bb:02"}, // moved
		{Network: "vmbr0.10", MAC: "52:54:00:aa:bb:05"}, // new: slot 2
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"cores": "2", "sockets": "1", "memory": "2048",
		"net0":   "virtio=52:54:00:AA:BB:01,bridge=vmbr0,tag=10",
		"net1":   "virtio=52:54:00:AA:BB:02,bridge=vmbr0,tag=30",
		"net2":   "virtio=52:54:00:AA:BB:05,bridge=vmbr0,tag=10",
		"delete": "net3",
	}
	for k, v := range want {
		if p.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, p.Get(k), v)
		}
	}
	if len(p) != len(want) {
		t.Errorf("params %v", p)
	}
}

func TestMemoryMiB(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want int
	}{{float64(2048), 2048}, {"1536", 1536}, {"current=4096,min=1024", 4096}, {nil, 0}} {
		if got := (&vmConfig{Memory: tc.v}).memoryMiB(); got != tc.want {
			t.Errorf("%v: %d, want %d", tc.v, got, tc.want)
		}
	}
}

func TestTag(t *testing.T) {
	d := &Driver{controllerID: "ctl1"}
	desc := d.description(hypervisor.MachineSpec{Name: "janus-node1", MachineID: "m1"})
	m := tagRe.FindStringSubmatch(desc)
	if m == nil || m[1] != "ctl1" || m[2] != "m1" {
		t.Errorf("tag not read back from %q", desc)
	}
	if !ownsVolumeName("img:iso/janus-cidata-m1.iso", "m1") || ownsVolumeName("img:iso/janus-cidata-m10.iso", "m1") || ownsVolumeName("img:iso/other.iso", "") {
		t.Error("ownsVolumeName")
	}
}

func TestFingerprint(t *testing.T) {
	fp := Fingerprint([]byte("certificate"))
	if len(fp) != 95 || strings.ToUpper(fp) != fp || strings.Count(fp, ":") != 31 {
		t.Errorf("fingerprint %q", fp)
	}
}
