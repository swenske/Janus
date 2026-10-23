package api

import (
	"syscall"
	"testing"
	"time"
)

func TestStepTimer(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tm := newStepTimer(func() time.Time { return now })
	now = now.Add(4712345 * time.Microsecond)
	tm.done("haproxy stopped")
	now = now.Add(3 * time.Millisecond)
	tm.done("shutdown hooks")
	now = now.Add(57 * time.Second)
	tm.done("sync")
	if got, want := tm.String(), "haproxy stopped 4.712s, shutdown hooks 3ms, sync 57s"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPowerVerb(t *testing.T) {
	for cmd, want := range map[int]string{
		syscall.LINUX_REBOOT_CMD_RESTART:   "rebooting",
		syscall.LINUX_REBOOT_CMD_KEXEC:     "rebooting through kexec",
		syscall.LINUX_REBOOT_CMD_POWER_OFF: "powering off",
	} {
		if got := powerVerb(cmd); got != want {
			t.Errorf("powerVerb(%#x) = %q, want %q", cmd, got, want)
		}
	}
}
