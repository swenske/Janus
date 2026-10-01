package modcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stand-in daemon: "checks" a file by refusing any line saying "bad".
func fakeModule(t *testing.T) *Module {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "daemon")
	script := "#!/bin/sh\nif grep -n bad \"$2\"; then echo \"($2: refused)\"; exit 5; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	Dir = t.TempDir()
	return &Module{Name: "daemon", Binary: bin, File: "daemon.conf", RunDir: t.TempDir(),
		CheckArgs: func(p string) []string { return []string{"-t", p} }}
}

func TestModule(t *testing.T) {
	m := fakeModule(t)
	if !m.Available() {
		t.Fatal("not available")
	}
	if _, isDefault, err := m.Saved(); !isDefault || err != nil {
		t.Fatal("something saved at first")
	}
	errs, err := m.Check("good\nbad line\n")
	if err == nil || len(errs) != 2 || errs[0] != "2:bad line" || !strings.Contains(errs[1], "(daemon.conf: refused)") {
		t.Fatalf("Check of a bad config = %q %v", errs, err)
	}
	if errs, err := m.Check("good\n"); err != nil {
		t.Fatalf("Check = %q %v", errs, err)
	}
	if left, _ := filepath.Glob(filepath.Join(m.RunDir, ".check-*")); len(left) != 0 {
		t.Fatalf("check files left: %v", left)
	}
	if err := m.Save("good\n"); err != nil {
		t.Fatal(err)
	}
	if c, isDefault, _ := m.Saved(); isDefault || c != "good\n" {
		t.Fatalf("Saved = %q", c)
	}
	if data, _ := os.ReadFile(m.RunPath()); string(data) != "good\n" {
		t.Fatalf("run copy %q", data)
	}
	os.Remove(m.RunPath())
	if err := m.Boot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.RunPath()); err != nil {
		t.Fatal("Boot didn't put the run copy back")
	}
	if err := m.Save(""); err != nil {
		t.Fatal(err)
	}
	if _, isDefault, _ := m.Saved(); !isDefault {
		t.Fatal("an empty config is still saved")
	}
	if _, err := os.Stat(m.RunPath()); !os.IsNotExist(err) {
		t.Fatal("the run copy survived an empty config")
	}
}
