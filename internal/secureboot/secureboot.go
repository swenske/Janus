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

// State is what the firmware does with Secure Boot, as a node reports
// it (VersionResponse's secure_boot).
type State int

const (
	// StateUnknown: the variables couldn't be read.
	StateUnknown State = iota
	// StateNoUEFI: not a UEFI boot - no efivarfs at all.
	StateNoUEFI
	// StateOff: a UEFI boot that enforces nothing - Secure Boot off, in
	// setup mode, or a firmware built without it (no SecureBoot
	// variable).
	StateOff
	// StateOn: the firmware boots only what its db allows.
	StateOn
)

// Current reads the firmware's Secure Boot state.
func Current() State {
	if _, err := os.Stat(Dir); err != nil {
		return StateNoUEFI
	}
	on, err := Enabled()
	switch {
	case err == nil && on:
		return StateOn
	case err == nil:
		return StateOff
	case errors.Is(err, ErrNoEFI):
		// No SecureBoot variable: a firmware without Secure Boot - unless
		// efivarfs isn't mounted there, its directory left empty.
		f, err := os.Open(Dir)
		if err != nil {
			return StateUnknown
		}
		defer f.Close()
		if names, _ := f.Readdirnames(1); len(names) == 0 {
			return StateUnknown
		}
		return StateOff
	}
	return StateUnknown
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
