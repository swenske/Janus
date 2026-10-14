package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVmstatCounter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vmstat")
	// A real /proc/vmstat's neighbourhood of oom_kill (kernel 7.2).
	sample := "pgmajfault 1046\npgrefill 0\noom_kill 3\nnuma_pte_updates 0\n"
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vmstatCounter(path, "oom_kill"); got != 3 {
		t.Errorf("oom_kill = %d, want 3", got)
	}
	if got := vmstatCounter(path, "oom"); got != 0 {
		t.Errorf("a prefix matched: oom = %d", got)
	}
	if got := vmstatCounter(filepath.Join(t.TempDir(), "none"), "oom_kill"); got != 0 {
		t.Errorf("no file: %d", got)
	}
}
