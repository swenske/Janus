package boottime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseUptime(t *testing.T) {
	up, err := parseUptime("12345.67 98765.43\n")
	if err != nil || up != 12345.67 {
		t.Fatalf("parseUptime = %v, %v", up, err)
	}
	if _, err := parseUptime(""); err == nil {
		t.Fatal("empty: no error")
	}
}

// The real /proc.
func TestReal(t *testing.T) {
	if _, err := os.Stat("/proc/uptime"); err != nil {
		t.Skip("no /proc")
	}
	up, err := Uptime()
	if err != nil {
		t.Fatal(err)
	}
	if up <= 0 {
		t.Fatalf("uptime %v", up)
	}
}

func TestSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot-times.json")
	if got, err := Load(path); err != nil || got != nil {
		t.Fatalf("Load(missing) = %v, %v", got, err)
	}
	// init's half first, then janusd's.
	if err := (Times{Kernel: 1.5, Init: 1.9}).Save(path); err != nil {
		t.Fatal(err)
	}
	half, err := Load(path)
	if err != nil || half == nil || half.API != 0 || half.Kernel != 1.5 {
		t.Fatalf("Load(init's) = %v, %v", half, err)
	}
	want := Times{Kernel: 1.5, Init: 1.9, HAProxy: 2.4, API: 3.1}
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got == nil || *got != want {
		t.Fatalf("Load = %v, %v, want %v", got, err, want)
	}
	if s := want.Stages(); len(s) != 4 || s[0].Name != "kernel" || s[3].Seconds != 3.1 {
		t.Fatalf("Stages = %v", s)
	}
	if want.String() != "api listening 3.1s after the kernel started (kernel 1.5s, init 1.9s, haproxy 2.4s)" {
		t.Fatalf("String = %q", want.String())
	}
}
