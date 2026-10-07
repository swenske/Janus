package secureboot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name string, value byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte{7, 0, 0, 0, value}, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEnabled(t *testing.T) {
	dir := t.TempDir()
	Dir = dir
	if _, err := Enabled(); !errors.Is(err, ErrNoEFI) {
		t.Fatalf("no variables: err = %v, want ErrNoEFI", err)
	}
	write(t, dir, secureBootVar, 0)
	if on, err := Enabled(); err != nil || on {
		t.Fatalf("SecureBoot=0: %v, %v", on, err)
	}
	write(t, dir, secureBootVar, 1)
	if on, err := Enabled(); err != nil || !on {
		t.Fatalf("SecureBoot=1, no SetupMode: %v, %v", on, err)
	}
	write(t, dir, setupModeVar, 1)
	if on, err := Enabled(); err != nil || on {
		t.Fatalf("SetupMode=1: %v, %v", on, err)
	}
	write(t, dir, setupModeVar, 0)
	if on, err := Enabled(); err != nil || !on {
		t.Fatalf("SetupMode=0: %v, %v", on, err)
	}
	if err := os.WriteFile(filepath.Join(dir, secureBootVar), []byte{1}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Enabled(); err == nil {
		t.Fatal("a truncated variable: no error")
	}
}
