package api

import (
	"fmt"
	"log"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/swenske/Janus/internal/consoledrain"
	"github.com/swenske/Janus/internal/haproxy"
	"github.com/swenske/Janus/internal/shutdown"
)

// Taking the machine down - a reboot, through the firmware or kexec, or
// a power-off - after the RPC that asked for it has answered: HAProxy
// soft-stopped, the extension services stopped (StopServices), the
// shutdown hooks (what must survive: the event log,
// the audit, the log shipper's cursors), the disks synced, the console
// drained, then reboot(2). Each step is timed, in one line logged just
// before the console is drained: a reboot that takes long says whether
// janusd took the time, or what came after it - the kernel's own
// shutdown, the firmware, the hypervisor. The log shipper is then given
// a moment (DrainLogs) to send that line and what came before it: a node
// without LOGS loses whatever it hadn't sent.

// stepTimer times consecutive steps.
type stepTimer struct {
	now   func() time.Time
	last  time.Time
	steps []string
}

func newStepTimer(now func() time.Time) *stepTimer {
	return &stepTimer{now: now, last: now()}
}

// done ends the step named name: its duration, since the previous one.
func (t *stepTimer) done(name string) {
	now := t.now()
	t.steps = append(t.steps, fmt.Sprintf("%s %s", name, now.Sub(t.last).Round(time.Millisecond)))
	t.last = now
}

func (t *stepTimer) String() string { return strings.Join(t.steps, ", ") }

// powerVerb is what reboot(2)'s cmd does, in a word or two.
func powerVerb(cmd int) string {
	switch cmd {
	case syscall.LINUX_REBOOT_CMD_POWER_OFF:
		return "powering off"
	case syscall.LINUX_REBOOT_CMD_KEXEC:
		return "rebooting through kexec"
	}
	return "rebooting"
}

// DrainLogs, when set (janusd: the log shipper's Drain), waits until the
// log targets have sent what was logged, its argument at most.
var DrainLogs func(time.Duration)

// StopServices, when set (janusd, on a node with extension services),
// stops them before the machine goes down, after HAProxy's soft stop -
// whose drain has already moved the traffic: Consul's services put in
// maintenance while its agent runs, then each daemon on its SIGTERM,
// keepalived and BIRD last. BIRD then closes its BGP sessions (a Cease)
// and keepalived sends its priority 0; left running into reboot(2), a
// peer kept its session with the node - and the routes it had from it -
// until its hold timer (#446).
var StopServices func()

// logsDrain bounds DrainLogs: a working target sends in milliseconds.
const logsDrain = time.Second

// powerDown takes the machine down with cmd, replyGrace after the call -
// who names the caller in the log. It doesn't return when it works.
func powerDown(who string, h *haproxy.Manager, cmd int) {
	time.Sleep(replyGrace)
	t := newStepTimer(time.Now)
	if h != nil {
		if err := h.Stop(5 * time.Second); err != nil {
			log.Printf("%s: stop haproxy: %v", who, err)
		}
		t.done("haproxy stopped")
	}
	if StopServices != nil {
		StopServices()
		t.done("extension services")
	}
	shutdown.Run()
	t.done("shutdown hooks")
	syscall.Sync()
	t.done("sync")
	log.Printf("%s: %s now - %s; the console drained next (2s at most)", who, powerVerb(cmd), t)
	if DrainLogs != nil {
		DrainLogs(logsDrain)
	}
	consoledrain.Wait(os.Stderr, 2*time.Second)
	if err := syscall.Reboot(cmd); err != nil {
		log.Printf("%s: reboot(%#x): %v", who, cmd, err)
	}
}
