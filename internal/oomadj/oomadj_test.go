package oomadj

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

func TestSet(t *testing.T) {
	Proc = t.TempDir()
	t.Cleanup(func() { Proc = "/proc"; enabled.Store(false) })
	if err := os.MkdirAll(filepath.Join(Proc, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(Proc, "42", "oom_score_adj")
	if err := os.WriteFile(file, []byte("-900"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Not enabled (a host, a CI runner): nothing written.
	if err := Set(42, HAProxy); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != "-900" {
		t.Errorf("written while not enabled: %q", b)
	}
	Enable()
	if err := Set(42, HAProxy); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != strconv.Itoa(HAProxy) {
		t.Errorf("got %q, want %d", b, HAProxy)
	}
	// Never created: the file is the kernel's.
	if err := Set(43, Default); err == nil {
		t.Error("a missing process's file was created")
	}
}

func TestOrder(t *testing.T) {
	if Janusd >= HAProxy || HAProxy >= Routing || Routing >= Default || MinExtension <= HAProxy {
		t.Errorf("janusd %d, HAProxy %d, routing %d, default %d, an extension's lowest %d", Janusd, HAProxy, Routing, Default, MinExtension)
	}
	for in, want := range map[int]int{-1000: Routing, -500: Routing, Routing: Routing, 0: 0, 300: 300, 2000: 1000} {
		if got := Clamp(in); got != want {
			t.Errorf("Clamp(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestRun(t *testing.T) {
	// Not enabled: just runs.
	if err := Run(exec.Command("true"), Default); err != nil {
		t.Fatal(err)
	}
}

func TestChildren(t *testing.T) {
	Proc = t.TempDir()
	t.Cleanup(func() { Proc = "/proc" })
	for pid, stat := range map[string]string{
		"10": "10 (keepalived) S 1 10 10 0",
		"11": "11 (keepalived) S 10 10 10 0",
		"12": "12 (a (b) c) S 10 10 10 0",
		"13": "13 (other) S 1 13 13 0",
	} {
		if err := os.MkdirAll(filepath.Join(Proc, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(Proc, pid, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := children(10)
	slices.Sort(got)
	if !slices.Equal(got, []int{11, 12}) {
		t.Errorf("children(10) = %v, want [11 12]", got)
	}
}
