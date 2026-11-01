package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/pairing"
)

// TestBootRescue: no file, no window; init's file opens one for
// RescueLifetime - a janusd restarted in the same boot keeps its end -;
// none once its certificate was issued, or past its time, or for a file
// it can't read.
func TestBootRescue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rescue.json")
	now := time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)
	if r, _ := bootRescue(path, now); r != nil {
		t.Fatal("a window without a rescue volume")
	}
	if r, _ := bootRescue("", now); r != nil {
		t.Fatal("a window without a path")
	}
	if err := os.WriteFile(path, []byte(`{"firewall":"skip"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, until := bootRescue(path, now)
	if r == nil || r.Firewall != "skip" || !until.Equal(now.Add(pairing.RescueLifetime)) {
		t.Fatalf("the first start: %+v until %s", r, until)
	}
	if r, again := bootRescue(path, now.Add(10*time.Minute)); r == nil || !again.Equal(until) {
		t.Errorf("restarted: %+v until %s, want %s", r, again, until)
	}
	if r, _ := bootRescue(path, until); r != nil {
		t.Error("a window past its time")
	}
	markRescued(path)
	if r, _ := bootRescue(path, now.Add(time.Minute)); r != nil {
		t.Error("a window once its certificate was issued")
	}

	if err := os.Remove(rescueStatePath(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"firewall":"off"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, _ := bootRescue(path, now); r != nil {
		t.Error("a window for a file it can't read")
	}
	if rescuePath("auto", false) != "" || rescuePath("auto", true) == "" || rescuePath("/x", false) != "/x" {
		t.Error("rescuePath")
	}
}
