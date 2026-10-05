package api

import (
	"os"
	"testing"
	"testing/fstest"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// The fixtures are real /proc/cpuinfo content: an x86 host (Linux 6.18,
// first two logical CPUs) and a Janus arm64 kernel booted under QEMU
// virt with -cpu cortex-a72 -smp 4 (no "model name", no "cpu MHz").

func TestParseCPUInfoAMD64(t *testing.T) {
	f, err := os.Open("testdata/cpuinfo-amd64-ryzen.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cpus := parseCPUInfo(f)
	if len(cpus) != 2 {
		t.Fatalf("parseCPUInfo = %d CPUs, want 2", len(cpus))
	}
	for i, c := range cpus {
		if c.GetProcessor() != uint32(i) || c.GetModelName() != "AMD Ryzen AI 9 HX 370 w/ Radeon 890M" || c.GetMhz() < 1000 {
			t.Errorf("cpu %d = %+v", i, c)
		}
	}
}

func TestParseCPUInfoARM64(t *testing.T) {
	f, err := os.Open("testdata/cpuinfo-arm64-qemu-cortex-a72.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cpus := parseCPUInfo(f)
	if len(cpus) != 4 {
		t.Fatalf("parseCPUInfo = %d CPUs, want 4", len(cpus))
	}
	for i, c := range cpus {
		if c.GetProcessor() != uint32(i) || c.GetModelName() != "ARM Cortex-A72" || c.GetMhz() != 0 {
			t.Errorf("cpu %d = %+v", i, c)
		}
	}
}

func TestARMCoreName(t *testing.T) {
	for _, tc := range []struct{ impl, part, want string }{
		{"0x41", "0xd0b", "ARM Cortex-A76"},
		{"0x41", "0xfff", "ARM part 0xfff"},
		{"0x61", "0x022", "Apple part 0x22"},
		{"0x99", "0xd08", "implementer 0x99 part 0xd08"},
		{"0x41", "", "ARM"},
		{"bogus", "0xd08", ""},
	} {
		if got := armCoreName(tc.impl, tc.part); got != tc.want {
			t.Errorf("armCoreName(%q, %q) = %q, want %q", tc.impl, tc.part, got, tc.want)
		}
	}
}

func TestCPUTopology(t *testing.T) {
	cpus := []*janusv1alpha1.CPUInfo{{Processor: 0}, {Processor: 1}, {Processor: 2}, {Processor: 3}}
	// Two packages of one core with two threads each (SMT).
	smt := fstest.MapFS{}
	for i, pc := range [][2]string{{"0", "0"}, {"0", "0"}, {"1", "0"}, {"1", "0"}} {
		dir := "cpu" + string(rune('0'+i)) + "/topology/"
		smt[dir+"physical_package_id"] = &fstest.MapFile{Data: []byte(pc[0] + "\n")}
		smt[dir+"core_id"] = &fstest.MapFile{Data: []byte(pc[1] + "\n")}
	}
	if s, c := cpuTopology(smt, cpus); s != 2 || c != 2 {
		t.Errorf("cpuTopology(SMT) = %d sockets, %d cores; want 2, 2", s, c)
	}
	// A CPU without topology files: the kernel doesn't say, so neither do we.
	delete(smt, "cpu3/topology/core_id")
	if s, c := cpuTopology(smt, cpus); s != 0 || c != 0 {
		t.Errorf("cpuTopology(incomplete) = %d, %d; want 0, 0", s, c)
	}
}

func TestFillMaxMHz(t *testing.T) {
	cpus := []*janusv1alpha1.CPUInfo{{Processor: 0}, {Processor: 1, Mhz: 2400}, {Processor: 2}}
	fillMaxMHz(fstest.MapFS{
		"cpu0/cpufreq/cpuinfo_max_freq": {Data: []byte("1800000\n")},
		"cpu1/cpufreq/cpuinfo_max_freq": {Data: []byte("1500000\n")},
	}, cpus)
	if cpus[0].Mhz != 1800 || cpus[1].Mhz != 2400 || cpus[2].Mhz != 0 {
		t.Errorf("fillMaxMHz = %v, %v, %v; want 1800, 2400 (kept), 0", cpus[0].Mhz, cpus[1].Mhz, cpus[2].Mhz)
	}
}
