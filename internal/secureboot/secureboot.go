// Package secureboot reads whether the firmware enforces Secure Boot,
// from the UEFI variables efivarfs exposes (rootfs/init mounts it at
// /sys/firmware/efi/efivars, read-only). What the node does with it:
// a reboot through kexec, which skips the firmware, keeps the
// firmware's rule - with Secure Boot on, only a signed UKI boots.
package secureboot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Dir is where init mounts efivarfs; a var for tests.
var Dir = "/sys/firmware/efi/efivars"

// The global EFI variables: 4 bytes of attributes, then 1 byte.
const (
	guid          = "8be4df61-93ca-11d2-aa0d-00e098032b8c"
	secureBootVar = "SecureBoot-" + guid
	setupModeVar  = "SetupMode-" + guid
)

// ErrNoEFI: the machine didn't boot through UEFI (or efivarfs isn't
// mounted): Secure Boot can't be on.
var ErrNoEFI = errors.New("no UEFI variables: not a UEFI boot")

// Enabled reports whether the firmware enforces Secure Boot: the
// SecureBoot variable is 1 and the firmware isn't in setup mode (keys
// being enrolled, nothing enforced). ErrNoEFI without UEFI variables.
func Enabled() (bool, error) {
	sb, err := readByte(secureBootVar)
	if err != nil {
		return false, err
	}
	if sb != 1 {
		return false, nil
	}
	setup, err := readByte(setupModeVar)
	if err != nil && !errors.Is(err, ErrNoEFI) {
		return false, err
	}
	return setup != 1, nil
}

func readByte(name string) (byte, error) {
	data, err := os.ReadFile(filepath.Join(Dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNoEFI
	}
	if err != nil {
		return 0, err
	}
	if len(data) < 5 {
		return 0, fmt.Errorf("%s: %d bytes, want attributes and a value", name, len(data))
	}
	return data[4], nil
}
