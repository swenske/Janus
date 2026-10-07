package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStartupChecks: the data directory and the master key file are
// checked before anything opens them, each refusal naming the chown to
// run; a missing key file is fine (made at first start), so is no key
// file configured.
func TestStartupChecks(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads anything")
	}
	dir := t.TempDir()
	if err := dataWritable(dir); err != nil {
		t.Fatalf("a writable directory: %v", err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := dataWritable(locked); err == nil || !strings.Contains(err.Error(), "chown -R") {
		t.Fatalf("an unwritable directory: %v", err)
	}
	if err := keyReadable(""); err != nil {
		t.Fatalf("no key file configured: %v", err)
	}
	if err := keyReadable(filepath.Join(dir, "none")); err != nil {
		t.Fatalf("a key file not made yet: %v", err)
	}
	key := filepath.Join(dir, "master.key")
	if err := os.WriteFile(key, []byte("k"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := keyReadable(key); err == nil || !strings.Contains(err.Error(), "chown") {
		t.Fatalf("an unreadable key: %v", err)
	}
}
