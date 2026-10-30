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

func TestCurrent(t *testing.T) {
	dir := t.TempDir()
	Dir = filepath.Join(dir, "efivars")
	if got := Current(); got != StateNoUEFI {
		t.Fatalf("no efivarfs: %v, want StateNoUEFI", got)
	}
	if err := os.Mkdir(Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Current(); got != StateUnknown {
		t.Fatalf("efivarfs not mounted (empty directory): %v, want StateUnknown", got)
	}
	write(t, Dir, "Boot0000-"+guid, 0)
	if got := Current(); got != StateOff {
		t.Fatalf("variables but no SecureBoot one: %v, want StateOff", got)
	}
	write(t, Dir, secureBootVar, 0)
	if got := Current(); got != StateOff {
		t.Fatalf("SecureBoot=0: %v, want StateOff", got)
	}
	write(t, Dir, secureBootVar, 1)
	if got := Current(); got != StateOn {
		t.Fatalf("SecureBoot=1: %v, want StateOn", got)
	}
	write(t, Dir, setupModeVar, 1)
	if got := Current(); got != StateOff {
		t.Fatalf("SecureBoot=1 in setup mode: %v, want StateOff", got)
	}
	if err := os.WriteFile(filepath.Join(Dir, secureBootVar), []byte{1}, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Current(); got != StateUnknown {
		t.Fatalf("a truncated variable: %v, want StateUnknown", got)
	}
}
