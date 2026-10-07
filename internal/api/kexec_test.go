package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/swenske/Janus/internal/secureboot"
)

func writeEFIVar(t *testing.T, dir, name string, value byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte{7, 0, 0, 0, value}, 0o644); err != nil {
		t.Fatal(err)
	}
}

// kexec keeps the firmware's rule: with Secure Boot enforced, a UKI
// that isn't signed by a release certificate is refused before anything
// is loaded; with it off (or no UEFI at all), the UKI goes to the
// kernel - here one that isn't a PE image, so the loader is what
// refuses it, past the rule. hack/qemu-kexec-test.sh reads the real
// variables on a real node.
func TestKexecKeepsTheFirmwaresRule(t *testing.T) {
	dir := t.TempDir()
	old := secureboot.Dir
	secureboot.Dir = dir
	t.Cleanup(func() { secureboot.Dir = old })
	notAUKI := []byte("MZ this is not a UKI")

	writeEFIVar(t, dir, "SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c", 1)
	err := loadKexec(notAUKI)
	if !errors.Is(err, errKexecUnsigned) {
		t.Fatalf("Secure Boot on, unsigned: %v, want the firmware's refusal", err)
	}

	writeEFIVar(t, dir, "SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c", 0)
	err = loadKexec(notAUKI)
	if err == nil || errors.Is(err, errKexecUnsigned) {
		t.Fatalf("Secure Boot off: %v, want the loader's refusal of a non-PE, not the rule's", err)
	}

	secureboot.Dir = filepath.Join(dir, "no-efi")
	err = loadKexec(notAUKI)
	if err == nil || errors.Is(err, errKexecUnsigned) {
		t.Fatalf("no UEFI: %v, want the loader's refusal, not the rule's", err)
	}
}
