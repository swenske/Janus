package api

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"

	"github.com/swenske/Janus/internal/espswitch"
	"github.com/swenske/Janus/internal/kexec"
	"github.com/swenske/Janus/internal/secureboot"
)

// A reboot through kexec (RebootMode KEXEC) loads a UKI's kernel and
// jumps into it without the firmware. It keeps the firmware's rule on
// what may boot: with Secure Boot enforced, only a UKI signed by a
// Janus release certificate (internal/releasetrust - what the
// firmware's db holds); with it off, whatever the ESP holds, as the
// firmware would boot it. Load errors leave nothing changed: the caller
// reboots through the firmware instead, or refuses.

// errKexecUnsigned: Secure Boot is on and the UKI isn't signed by a
// release certificate - the firmware would refuse it too.
var errKexecUnsigned = errors.New("with Secure Boot enabled, this UKI isn't signed by a Janus release certificate: the firmware would refuse it, so does kexec")

// loadKexec stages uki's kernel for a kexec reboot, under the Secure
// Boot rule above.
func loadKexec(uki []byte) error {
	enforced, err := secureboot.Enabled()
	state := "Secure Boot off"
	switch {
	case errors.Is(err, secureboot.ErrNoEFI):
		state = "no UEFI variables, Secure Boot can't be on"
	case err != nil:
		return fmt.Errorf("reading the Secure Boot state: %w", err)
	case enforced:
		if err := verifyUKI(uki); err != nil {
			return fmt.Errorf("%w (%v)", errKexecUnsigned, err)
		}
		state = "Secure Boot enforced, UKI signature verified"
	}
	if err := kexec.Load(uki); err != nil {
		return err
	}
	log.Printf("kexec: the next reboot jumps into the loaded kernel without the firmware (%s)", state)
	return nil
}

// loadKexecActive stages the UKI the firmware would boot next: the ESP's
// \EFI\BOOT\BOOT*.EFI - the active slot, or the one an Upgrade just
// switched to.
func loadKexecActive() error {
	bc, err := resolveBootContext()
	if err != nil {
		return err
	}
	if err := espswitch.Mount(bc.espDevice); err != nil {
		return err
	}
	uki, readErr := os.ReadFile(filepath.Join(espswitch.Mountpoint, "EFI", "BOOT", espswitch.BootFilename))
	if err := espswitch.Unmount(); err != nil {
		log.Printf("kexec: unmount %s: %v", espswitch.Mountpoint, err)
	}
	if readErr != nil {
		return fmt.Errorf("read the active UKI: %w", readErr)
	}
	return loadKexec(uki)
}

// rebootCmd is reboot(2)'s command for a RebootMode: kexec once a kernel
// is loaded, the firmware otherwise.
func rebootCmd(kexecLoaded bool) int {
	if kexecLoaded {
		return syscall.LINUX_REBOOT_CMD_KEXEC
	}
	return syscall.LINUX_REBOOT_CMD_RESTART
}
