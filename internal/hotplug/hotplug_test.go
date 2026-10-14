package hotplug

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestParseList(t *testing.T) {
	for in, want := range map[string][]int{
		"":          nil,
		"0\n":       {0},
		"0-3":       {0, 1, 2, 3},
		"0-1,4,6-7": {0, 1, 4, 6, 7},
		"3,1,1-2":   {1, 2, 3},
		"0-511":     nil, // checked by length below
	} {
		got, err := ParseList(in)
		if err != nil {
			t.Fatalf("ParseList(%q): %v", in, err)
		}
		if in == "0-511" {
			if len(got) != 512 {
				t.Errorf("ParseList(%q) = %d CPUs, want 512", in, len(got))
			}
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ParseList(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"a", "1-", "3-1", "-1", "0-9000", "1,,2"} {
		if _, err := ParseList(bad); err == nil {
			t.Errorf("ParseList(%q) accepted", bad)
		}
	}
}

// fakeCPUs lays out a sysfs CPU directory: present and online lists,
// and an online switch for each CPU in switches.
func fakeCPUs(t *testing.T, present, online string, switches ...int) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("present", present+"\n")
	write("online", online+"\n")
	for _, c := range switches {
		write(filepath.Join("cpu"+itoa(c), "online"), "0\n")
	}
	return dir
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestOnlineCPUs(t *testing.T) {
	// CPU 3 has no switch (unplugged meanwhile): neither onlined nor a
	// failure.
	dir := fakeCPUs(t, "0-3", "0-1", 1, 2)
	onlined, failed := OnlineCPUs(dir)
	if !reflect.DeepEqual(onlined, []int{2}) || len(failed) != 0 {
		t.Fatalf("OnlineCPUs = %v, %v; want [2], none", onlined, failed)
	}
	// Written over the fake's "0\n" without truncating it, as sysfs
	// takes it.
	data, _ := os.ReadFile(filepath.Join(dir, "cpu2", "online"))
	if string(data) != "1\n" {
		t.Errorf("cpu2/online = %q, want 1", data)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "cpu1", "online"))
	if string(data) != "0\n" {
		t.Errorf("cpu1/online (already online) was written: %q", data)
	}

	// Everything online: nothing to do.
	if onlined, failed := OnlineCPUs(fakeCPUs(t, "0-1", "0-1", 1)); len(onlined)+len(failed) != 0 {
		t.Errorf("all online: OnlineCPUs = %v, %v", onlined, failed)
	}

	// A switch that can't be written is a failure.
	dir = fakeCPUs(t, "0-1", "0", 1)
	if err := os.Chmod(filepath.Join(dir, "cpu1", "online"), 0o444); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if _, failed := OnlineCPUs(dir); failed[1] == nil {
			t.Errorf("read-only switch: no failure reported")
		}
	}

	if _, failed := OnlineCPUs(t.TempDir()); failed[-1] == nil {
		t.Errorf("no present list: no failure reported")
	}
}

const meminfo = `MemTotal:        2014636 kB
MemFree:         1715020 kB
MemAvailable:    1792428 kB
`

func TestRead(t *testing.T) {
	dir := fakeCPUs(t, "0-3", "0-2")
	mi := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(mi, []byte(meminfo), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir, mi)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Sizes{CPUs: 3, MemoryKiB: 2014636}); got != want {
		t.Errorf("Read = %+v, want %+v", got, want)
	}
	if got.MemoryMiB() != 1967 {
		t.Errorf("MemoryMiB = %d, want 1967", got.MemoryMiB())
	}
	if err := os.WriteFile(mi, []byte("MemFree: 1 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir, mi); err == nil {
		t.Errorf("meminfo without MemTotal accepted")
	}
}

func TestSettle(t *testing.T) {
	mib := func(cpus int, m uint64) Sizes { return Sizes{CPUs: cpus, MemoryKiB: m * 1024} }
	s := settle{reported: mib(1, 1024)}
	t0 := time.Unix(1000, 0)
	type step struct {
		at      time.Duration // since t0
		read    Sizes
		changed bool
		before  Sizes
	}
	for i, st := range []step{
		{at: 0, read: mib(1, 1024)},
		// Noise below a block isn't a change.
		{at: 2 * time.Second, read: Sizes{CPUs: 1, MemoryKiB: 1024*1024 + 1000}},
		{at: 10 * time.Second, read: Sizes{CPUs: 1, MemoryKiB: 1024*1024 + 1000}},
		// Two vCPUs added one after the other: one change, once it held
		// for SettleFor.
		{at: 12 * time.Second, read: mib(2, 1024)},
		{at: 14 * time.Second, read: mib(3, 1024)},
		{at: 16 * time.Second, read: mib(3, 1024)},
		{at: 18 * time.Second, read: mib(3, 1024)},
		{at: 20 * time.Second, read: mib(3, 1024), changed: true, before: mib(1, 1024)},
		{at: 22 * time.Second, read: mib(3, 1024)},
		// Proxmox taking memory modules out every 3 s: one change.
		{at: 24 * time.Second, read: mib(3, 2560)},
		{at: 26 * time.Second, read: mib(3, 2560)},
		{at: 28 * time.Second, read: mib(3, 2048)},
		{at: 30 * time.Second, read: mib(3, 2048)},
		{at: 32 * time.Second, read: mib(3, 1536)},
		{at: 34 * time.Second, read: mib(3, 1536)},
		{at: 36 * time.Second, read: mib(3, 1536)},
		{at: 38 * time.Second, read: mib(3, 1536), changed: true, before: mib(3, 1024)},
		// A change that went back before settling isn't one.
		{at: 40 * time.Second, read: mib(2, 1536)},
		{at: 42 * time.Second, read: mib(3, 1536)},
		{at: 50 * time.Second, read: mib(3, 1536)},
	} {
		before, after, changed := s.next(st.read, t0.Add(st.at))
		if changed != st.changed {
			t.Fatalf("step %d (%+v): changed = %v, want %v", i, st.read, changed, st.changed)
		}
		if changed && (before != st.before || after != st.read) {
			t.Fatalf("step %d: change %+v -> %+v, want %+v -> %+v", i, before, after, st.before, st.read)
		}
	}
}
