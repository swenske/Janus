package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// janusdStopTimeout bounds how long a requested power-off or reboot waits
// for janusd to stop HAProxy and the extension services gracefully.
const janusdStopTimeout = 20 * time.Second

// handlePowerSignals turns the signals /sbin/shutdown sends PID 1 (busybox
// init's conventions: SIGUSR2 power-off, SIGUSR1 halt, SIGTERM reboot)
// into a clean power action: supervision stops, janusd gets SIGTERM and
// stops HAProxy and the extension services, then the machine goes down.
// done is closed when the supervisor has returned (janusd exited).
func handlePowerSignals(sv *Supervisor, done <-chan struct{}) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		sig := <-ch
		cmd, what := syscall.LINUX_REBOOT_CMD_POWER_OFF, "power-off"
		switch sig {
		case syscall.SIGTERM:
			cmd, what = syscall.LINUX_REBOOT_CMD_RESTART, "reboot"
		case syscall.SIGUSR1:
			cmd, what = syscall.LINUX_REBOOT_CMD_HALT, "halt"
		}
		fmt.Printf("init: %s requested (%v) - stopping janusd\n", what, sig)
		sv.Stop(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(janusdStopTimeout):
			fmt.Printf("init: janusd didn't stop within %s - going down anyway\n", janusdStopTimeout)
		}
		syscall.Sync()
		if err := syscall.Reboot(cmd); err != nil {
			fmt.Printf("init: reboot(%#x): %v\n", cmd, err)
		}
	}()
}

// linkVirtioPorts creates /dev/virtio-ports/<name> for every named
// virtio-serial port - what udev does on other systems, and where the
// QEMU guest agent looks for its channel (org.qemu.guest_agent.0).
// devtmpfs only creates the anonymous /dev/vportNpM nodes.
func linkVirtioPorts() {
	names, err := filepath.Glob("/sys/class/virtio-ports/*/name")
	if err != nil || len(names) == 0 {
		return
	}
	if err := os.MkdirAll("/dev/virtio-ports", 0o755); err != nil {
		fmt.Printf("init: mkdir /dev/virtio-ports: %v\n", err)
		return
	}
	for _, f := range names {
		data, err := os.ReadFile(f)
		name := strings.TrimSpace(string(data))
		if err != nil || name == "" || strings.ContainsAny(name, "/") || name == "." || name == ".." {
			continue
		}
		port := filepath.Base(filepath.Dir(f)) // vportNpM
		link := filepath.Join("/dev/virtio-ports", name)
		if err := os.Symlink("../"+port, link); err != nil && !os.IsExist(err) {
			fmt.Printf("init: link %s: %v\n", link, err)
			continue
		}
		fmt.Printf("init: virtio port %s -> /dev/%s\n", name, port)
	}
}
