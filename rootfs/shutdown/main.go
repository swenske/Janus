// Command shutdown is /sbin/shutdown on a Janus node: the one entry point
// other software has to power the node off or reboot it - the QEMU guest
// agent runs "/sbin/shutdown -h -P +0 ..." when the hypervisor asks for a
// clean shutdown. There's no shell and no real shutdown(8): this only
// signals PID 1 (rootfs/init), with busybox init's conventions - SIGUSR2
// powers off, SIGUSR1 halts, SIGTERM reboots. rootfs/init then stops
// janusd gracefully (HAProxy soft-stop, extension services) before it
// cuts the power. The time and message arguments are accepted and ignored:
// the node always goes down now.
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

func main() {
	sig, what := syscall.SIGUSR2, "power-off"
	for _, arg := range os.Args[1:] {
		if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
			continue // a time ("+0", "now") or a message
		}
		switch {
		case strings.Contains(arg, "r"):
			sig, what = syscall.SIGTERM, "reboot"
		case strings.Contains(arg, "H"):
			sig, what = syscall.SIGUSR1, "halt"
		}
	}
	if err := syscall.Kill(1, sig); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown: signal init for a %s: %v\n", what, err)
		os.Exit(1)
	}
	fmt.Printf("shutdown: %s requested\n", what)
}
