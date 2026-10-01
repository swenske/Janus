// Package consoledrain lets a process that's about to reboot or power off
// the machine wait until what it last wrote has reached the console. On a
// node, janusd's and rootfs/init's output goes through a pipe that init
// copies to the serial console and the screen (rootfs/init/console.go):
// a write returns once the bytes are in the pipe, and a reboot right after
// would lose them - "rebooting to complete the revert" never printed.
package consoledrain

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Wait blocks until the pipe behind f holds nothing - init has read it -
// or max passes, then leaves init a moment to write the last chunk out.
// f that isn't a pipe (a terminal, a file: janusd run natively, tests)
// returns at once.
func Wait(f *os.File, max time.Duration) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFIFO {
		return
	}
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		n, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCINQ) // FIONREAD
		if err != nil || n == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
}
