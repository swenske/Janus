package haproxy

import (
	"errors"
	"log"
	"time"

	"github.com/swenske/Janus/internal/events"
)

// HAProxy that exits on its own - the kernel's OOM killer took it, a
// signal, a crash - is started again, with the configuration on disk (the
// one it ran: a rejected Apply puts the previous one back). Nothing else
// would start it: a node alone stopped serving until someone called
// ServiceStart or rebooted it (v2026.10.14-2's CI: the OOM killer took
// the serving process after the new one).
//
// The wait doubles after each exit that comes soon after the start, from
// restartFirst up to restartMax - a configuration that crashes HAProxy
// under traffic doesn't make it spin -, and is back to restartFirst once
// a process ran restartSettled. A start or stop asked of the Manager in
// the meantime (Apply, Reload, ServiceStart, Stop - a reboot's included)
// cancels the planned restart. Manager's RestartFirst, RestartMax and
// RestartSettled change them (tests); zero: these.
const (
	restartFirst   = time.Second
	restartMax     = 30 * time.Second
	restartSettled = time.Minute
)

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// planRestart schedules a restart after p exited on its own and returns
// the wait. Called with mu held.
func (m *Manager) planRestart(p *process) time.Duration {
	switch {
	case m.restartDelay == 0 || time.Since(p.started) >= orDefault(m.RestartSettled, restartSettled):
		m.restartDelay = orDefault(m.RestartFirst, restartFirst)
	default:
		m.restartDelay = min(m.restartDelay*2, orDefault(m.RestartMax, restartMax))
	}
	gen, wait := m.gen, m.restartDelay
	time.AfterFunc(wait, func() { m.restart(gen, wait) })
	return wait
}

// restart starts HAProxy again unless someone started or stopped it since
// the exit (gen moved) or a process runs. A start that fails is tried
// again, waiting twice as long: a process that exits at once has its next
// restart planned by startLocked (nothing serves), one that couldn't even
// be started here.
func (m *Manager) restart(gen uint64, waited time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != gen || (m.cur != nil && !m.cur.exited()) {
		return
	}
	if err := m.startLocked(true); err != nil {
		if !errors.As(err, new(*StartError)) {
			m.restartDelay = min(m.restartDelay*2, orDefault(m.RestartMax, restartMax))
			wait := m.restartDelay
			time.AfterFunc(wait, func() { m.restart(gen, wait) })
		}
		log.Printf("haproxy: starting it again failed (%v) - next try in %s", err, m.restartDelay)
		events.Publish("haproxy.restart_failed", map[string]any{"error": err.Error(), "retry_in_seconds": m.restartDelay.Seconds()})
		return
	}
	m.counters.Restarts.Add(1)
	pid := m.cur.cmd.Process.Pid
	log.Printf("haproxy: started again after %s (pid %d)", waited, pid)
	events.Publish("haproxy.restarted", map[string]any{"pid": pid, "after_seconds": waited.Seconds()})
}
