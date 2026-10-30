package proxmox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// What qemu-server's x86-64 levels ask of the host (/proc/cpuinfo's
// names); host and the named models aren't checked by the Controller.
func TestMissingCPUFlags(t *testing.T) {
	// An i9-13900H: x86-64-v3, no AVX-512.
	const raptorLake = "fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat pse36 clflush mmx fxsr sse sse2 ss ht syscall nx pdpe1gb rdtscp lm " +
		"pni pclmulqdq ssse3 fma cx16 sse4_1 sse4_2 x2apic movbe popcnt aes xsave avx f16c rdrand lahf_lm abm 3dnowprefetch fsgsbase bmi1 avx2 smep bmi2 erms invpcid adx sha_ni"
	for _, tc := range []struct {
		model, flags string
		missing      []string
	}{
		{"x86-64-v2-AES", raptorLake, nil},
		{"x86-64-v3", raptorLake, nil},
		{"x86-64-v4", raptorLake, []string{"avx512f", "avx512bw", "avx512cd", "avx512dq", "avx512vl"}},
		{"x86-64-v3", "fpu pni ssse3 sse4_1 sse4_2 popcnt aes avx xsave", []string{"avx2", "bmi1", "bmi2", "f16c", "fma", "abm", "movbe"}},
		{"x86-64-v2-AES", "fpu pni ssse3 sse4_1 sse4_2 popcnt", []string{"aes"}},
		{"host", "fpu", nil},
		{"EPYC-Rome", "fpu", nil},
		{"x86-64-v4", "", nil}, // the host didn't say
	} {
		if got := missingCPUFlags(tc.model, tc.flags); !slices.Equal(got, tc.missing) {
			t.Errorf("%s: missing %v, want %v", tc.model, got, tc.missing)
		}
	}
}

// The setting's names: Proxmox's own, never one without AES-NI.
func TestCPUModelNames(t *testing.T) {
	for _, ok := range []string{"x86-64-v2-AES", "x86-64-v3", "x86-64-v4", "host", "Haswell-noTSX", "Skylake-Client-v4", "EPYC-Rome", "EPYC-Genoa", "Westmere", "Opteron_G4"} {
		if !hypervisor.IsProxmoxCPUModel(ok) {
			t.Errorf("%s refused", ok)
		}
	}
	for _, bad := range []string{"", "kvm64", "qemu64", "x86-64-v2", "max", "custom-janus", "Nehalem", "Penryn", "core2duo", "Opteron_G3", "pentium", "x86-64-V3", "host,flags=+aes"} {
		if hypervisor.IsProxmoxCPUModel(bad) {
			t.Errorf("%s accepted", bad)
		}
	}
}

// fakePVE is the part of Proxmox VE's API a machine's CPU model goes
// through: the node's status (its processor's flags), the cluster's VMs,
// one VM's configuration and state, and its changes.
type fakePVE struct {
	mu    sync.Mutex
	flags string
	cpu   string // VM 101's cpu line, "" for none
	puts  []url.Values
	paths []string
}

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	var data any
	switch p := strings.TrimPrefix(r.URL.Path, "/api2/json"); {
	case r.Method == http.MethodGet && p == "/nodes/pve1/status":
		data = map[string]any{"cpu": 0.1, "cpuinfo": map[string]any{"model": "13th Gen Intel(R) Core(TM) i9-13900H", "cpus": 20, "mhz": "2995.2", "flags": f.flags},
			"memory": map[string]any{"total": 64 << 30, "available": 32 << 30}, "pveversion": "pve-manager/9.0.11/abc", "current-kernel": map[string]any{"release": "6.14.11-4-pve"}}
	case r.Method == http.MethodGet && p == "/nodes/pve1/qemu":
		data = []map[string]any{}
	case r.Method == http.MethodGet && p == "/cluster/resources":
		data = []map[string]any{{"vmid": 101, "node": "pve1"}}
	case r.Method == http.MethodGet && p == "/nodes/pve1/qemu/101/config":
		cfg := map[string]any{"name": "janus-lb1", "cores": 2, "sockets": 1, "memory": "2048",
			"description": "Janus node lb1, created by a Janus Controller - change it there, not here.\n\njanus-controller=ctl1 janus-machine=m1"}
		if f.cpu != "" {
			cfg["cpu"] = f.cpu
		}
		data = cfg
	case r.Method == http.MethodGet && p == "/nodes/pve1/qemu/101/status/current":
		data = map[string]any{"status": "stopped", "qmpstatus": "stopped"}
	case r.Method == http.MethodPut && p == "/nodes/pve1/qemu/101/config":
		_ = r.ParseForm()
		f.puts = append(f.puts, r.PostForm)
		if c := r.PostForm.Get("cpu"); c != "" {
			f.cpu = c
		}
	default:
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "not in the fake: " + r.Method + " " + p})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// state reads the fake's state, or changes it, under its lock.
func (f *fakePVE) state(fn func(*fakePVE)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func fakeDriver(t *testing.T, pve *fakePVE, model string) *Driver {
	t.Helper()
	srv := httptest.NewTLSServer(pve)
	t.Cleanup(srv.Close)
	h := &hypervisor.Hypervisor{Kind: hypervisor.KindProxmox, Name: "pve1", TokenSecret: "s3cr3t", Proxmox: &hypervisor.ProxmoxConfig{
		URL: srv.URL, Node: "pve1", TokenID: "janus-ctl@pve!controller", Pool: "janus", Storage: "local-lvm", ImageStorage: "janus-images",
		Networks: []string{"vmbr0"}, CPUModel: model, Fingerprint: Fingerprint(srv.Certificate().Raw),
	}}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	d, err := New(h, "ctl1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// A hypervisor's CPU model through the driver, against Proxmox's API:
// a machine with an earlier setting's model shows it, Reconfigure gives
// it the new one - only on a host that can run it, checked before
// anything changes -, a model chosen on the host is left alone, and a
// model the host can't run is said on the hypervisor's card and refused
// for a new machine before anything is uploaded.
func TestCPUModelThroughTheAPI(t *testing.T) {
	ctx := context.Background()
	const v3Host = "fpu pni pclmulqdq ssse3 fma sse4_1 sse4_2 movbe popcnt aes xsave avx f16c abm bmi1 avx2 bmi2 adx"
	pve := &fakePVE{flags: "fpu pni ssse3 sse4_1 sse4_2 popcnt aes", cpu: "x86-64-v2-AES"} // a host without AVX2 first
	ref := hypervisor.MachineRef{MachineID: "m1", UUID: "101", Name: "janus-lb1"}
	hw := hypervisor.Hardware{VCPUs: 2, MemoryMiB: 2048}

	// The hypervisor's setting moved to x86-64-v3 after the machine was
	// created: reported, never changed by a read.
	d := fakeDriver(t, pve, "x86-64-v3")
	st, err := d.MachineStatus(ctx, ref)
	if err != nil || len(st.Drift) != 1 || st.Drift[0] != (hypervisor.Drift{Setting: "cpu", Have: "x86-64-v2-AES", Want: "x86-64-v3"}) {
		t.Fatalf("status: %+v %v", st, err)
	}
	host, err := d.HostInfo(ctx)
	if err != nil || !strings.Contains(host.MachineCPUError, "x86-64-v3 needs avx avx2 bmi1 bmi2 f16c fma abm movbe") {
		t.Fatalf("host: %q %v", host.MachineCPUError, err)
	}
	// This host can't run it: refused, nothing written.
	if err := d.Reconfigure(ctx, ref, hw, nil, hypervisor.Settings{}); err == nil || !strings.Contains(err.Error(), "avx2") {
		t.Fatalf("Reconfigure on a host without AVX2: %v", err)
	}
	pve.state(func(f *fakePVE) {
		if len(f.puts) != 0 || f.cpu != "x86-64-v2-AES" {
			t.Fatalf("written anyway: %v, cpu %s", f.puts, f.cpu)
		}
	})
	if _, err := d.CreateMachine(ctx, hypervisor.MachineSpec{MachineID: "m2", Name: "janus-lb2", Hardware: hw, NICs: []hypervisor.NIC{{Network: "vmbr0", MAC: "52:54:00:aa:bb:02"}}}); err == nil || !strings.Contains(err.Error(), "wouldn't start") {
		t.Fatalf("CreateMachine on a host without AVX2: %v", err)
	}
	pve.state(func(f *fakePVE) {
		for _, p := range f.paths {
			if strings.Contains(p, "/upload") || strings.HasPrefix(p, "POST ") {
				t.Errorf("CreateMachine went on: %s", p)
			}
		}
		// A host that can: the model is set, and nothing differs anymore.
		f.flags = v3Host
	})
	if host, err = d.HostInfo(ctx); err != nil || host.MachineCPUError != "" {
		t.Fatalf("host: %q %v", host.MachineCPUError, err)
	}
	if err := d.Reconfigure(ctx, ref, hw, nil, hypervisor.Settings{}); err != nil {
		t.Fatal(err)
	}
	pve.state(func(f *fakePVE) {
		if len(f.puts) != 1 || f.puts[0].Get("cpu") != "x86-64-v3" {
			t.Fatalf("Reconfigure wrote %v", f.puts)
		}
	})
	if st, err = d.MachineStatus(ctx, ref); err != nil || len(st.Drift) != 0 {
		t.Fatalf("after Reconfigure: %+v %v", st, err)
	}

	// A model with flags someone added on the host: theirs - not
	// reported, not overwritten by a hardware change.
	pve.state(func(f *fakePVE) { f.cpu = "host,flags=+pcid" })
	if st, err = d.MachineStatus(ctx, ref); err != nil || len(st.Drift) != 0 {
		t.Fatalf("a model chosen on the host: %+v %v", st, err)
	}
	if err := d.Reconfigure(ctx, ref, hw, nil, hypervisor.Settings{}); err != nil {
		t.Fatal(err)
	}
	pve.state(func(f *fakePVE) {
		if len(f.puts) != 2 || f.puts[1].Has("cpu") || f.cpu != "host,flags=+pcid" {
			t.Fatalf("Reconfigure changed a model chosen on the host: %v", f.puts)
		}
	})

	// x86-64-v4 on this host (no AVX-512): the card says so.
	d = fakeDriver(t, pve, "x86-64-v4")
	if host, err = d.HostInfo(ctx); err != nil || !strings.Contains(host.MachineCPUError, "avx512f") {
		t.Fatalf("x86-64-v4: %q %v", host.MachineCPUError, err)
	}
	// host: never checked by the Controller - any processor runs it.
	pve.state(func(f *fakePVE) { f.flags = "fpu" })
	if host, err = fakeDriver(t, pve, "host").HostInfo(ctx); err != nil || host.MachineCPUError != "" {
		t.Fatalf("host: %q %v", host.MachineCPUError, err)
	}
}
