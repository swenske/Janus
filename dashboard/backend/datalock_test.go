package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockDataDir(t *testing.T) {
	saved := dataLock
	t.Cleanup(func() { dataLock = saved })
	dir := t.TempDir()

	if err := lockDataDir(dir); err != nil {
		t.Fatalf("first lock: %v", err)
	}
	first := dataLock
	data, err := os.ReadFile(filepath.Join(dir, dataLockFile))
	if err != nil || !strings.HasPrefix(string(data), "pid ") {
		t.Fatalf("lock file = %q, %v: want who holds it", data, err)
	}

	// A second Controller on the same directory - another open file, as
	// another process's would be - is refused, and told who holds it.
	err = lockDataDir(dir)
	if err == nil {
		t.Fatal("a second lock on the same data directory was taken")
	}
	if !strings.Contains(err.Error(), "another Controller runs on") || !strings.Contains(err.Error(), string(data[:len(data)-1])) {
		t.Errorf("second lock: %v - want who holds it", err)
	}
	if dataLock != first {
		t.Error("a refused lock replaced the held one")
	}

	// Once the first lets go (its process ended, or restartSelf's exec
	// closed it), the next start takes it.
	first.Close()
	if err := lockDataDir(dir); err != nil {
		t.Fatalf("lock after the holder let go: %v", err)
	}
	dataLock.Close()
}

func TestBackupSkipsTheLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, dataLockFile), []byte("pid 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir: %v %v", entries, err)
	}
	if !backupSkipped(dataLockFile, entries[0]) {
		t.Error("a backup takes the running Controller's lock file")
	}
}
