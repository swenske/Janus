package haproxy

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/events"
)

// restartRig is a Manager running the real build/haproxy (bin: what the
// Manager runs - a wrapper around it, or it) serving body on a free port.
type restartRig struct {
	m    *Manager
	port int
	real string
}

func newRestartRig(t *testing.T, wrap string, first, maxWait, settled time.Duration) *restartRig {
	t.Helper()
	real, _ := filepath.Abs("../../build/haproxy")
	if _, err := os.Stat(real); err != nil {
		t.Skip("build/haproxy not built (make haproxy-build)")
	}
	dir := t.TempDir()
	bin := real
	if wrap != "" {
		// A wrapper named haproxy, as the real one: previousPID checks the
		// process name.
		bin = filepath.Join(dir, "bin", "haproxy")
		_ = os.MkdirAll(filepath.Dir(bin), 0o755)
		if err := os.WriteFile(bin, []byte(fmt.Sprintf(wrap, real)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManager(bin, filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), filepath.Join(dir, "admin.sock"))
	m.Output = &testWriter{t}
	m.RestartFirst, m.RestartMax, m.RestartSettled = first, maxWait, settled
	r := &restartRig{m: m, port: freePort(t), real: real}
	t.Cleanup(func() { _ = m.Stop(5 * time.Second) })
	return r
}

func (r *restartRig) cfg(body string) []byte {
	return fmt.Appendf(nil, `global
    stats socket %s mode 600 level admin
defaults
    mode http
    timeout connect 1s
    timeout client 1s
    timeout server 1s
frontend web
    bind 127.0.0.1:%d
    http-request return status 200 content-type text/plain string %s
`, r.m.StatsSocketPath, r.port, body)
}

func (r *restartRig) get() string {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", r.port))
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	b := make([]byte, 64)
	n, _ := resp.Body.Read(b)
	return string(b[:n])
}

// servesAgain waits until body is served by a process other than old.
func (r *restartRig) servesAgain(t *testing.T, body string, old int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if pid := r.m.Pid(); pid != 0 && pid != old && r.get() == body {
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("not serving %q again within %s (pid %d, serves %q)", body, within, r.m.Pid(), r.get())
	return 0
}

func kill(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
}

// TestRestartAfterExit: HAProxy killed (as the kernel's OOM killer does)
// is started again with its configuration - counted, said - and the
// wait doubles while it keeps dying soon after its start.
func TestRestartAfterExit(t *testing.T) {
	r := newRestartRig(t, "", 100*time.Millisecond, 400*time.Millisecond, time.Hour)
	if errs, err := r.m.Apply(r.cfg("one")); err != nil {
		t.Fatal(errs, err)
	}
	_, from := events.Since(0)
	pid := r.m.Pid()
	for i, want := range []time.Duration{100, 200, 400, 400} {
		kill(t, pid)
		pid = r.servesAgain(t, "one", pid, 5*time.Second)
		r.m.mu.Lock()
		got := r.m.restartDelay
		r.m.mu.Unlock()
		if got != want*time.Millisecond {
			t.Errorf("restart %d waited %s, want %s", i+1, got, want*time.Millisecond)
		}
	}
	c := r.m.Counters()
	if c.UnexpectedExits.Load() != 4 || c.Restarts.Load() != 4 {
		t.Errorf("counted %d exits, %d restarts; want 4, 4", c.UnexpectedExits.Load(), c.Restarts.Load())
	}
	evs, _ := events.Since(from)
	exited, restarted := 0, 0
	for _, e := range evs {
		switch e.Type {
		case "haproxy.exited":
			if bytes.Contains(e.Payload, []byte(`"restart_in_seconds"`)) {
				exited++
			}
		case "haproxy.restarted":
			restarted++
		}
	}
	if exited != 4 || restarted != 4 {
		t.Errorf("events: %d exits saying when it restarts, %d restarts; want 4, 4", exited, restarted)
	}
	if !r.m.Serving() {
		t.Error("serving again, but not marked serving")
	}
}

// TestRestartWaitSettles: a process that ran long enough brings the wait
// back to its first value.
func TestRestartWaitSettles(t *testing.T) {
	r := newRestartRig(t, "", 100*time.Millisecond, time.Second, 300*time.Millisecond)
	if errs, err := r.m.Apply(r.cfg("one")); err != nil {
		t.Fatal(errs, err)
	}
	pid := r.m.Pid()
	kill(t, pid)
	pid = r.servesAgain(t, "one", pid, 5*time.Second)
	kill(t, pid) // soon after its start: 200 ms
	pid = r.servesAgain(t, "one", pid, 5*time.Second)
	time.Sleep(400 * time.Millisecond) // settled
	kill(t, pid)
	r.servesAgain(t, "one", pid, 5*time.Second)
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if r.m.restartDelay != 100*time.Millisecond {
		t.Errorf("after a settled process, waited %s, want 100ms", r.m.restartDelay)
	}
}

// TestStopCancelsRestart: stopped (a reboot, ServiceStop) during the
// wait, it stays stopped.
func TestStopCancelsRestart(t *testing.T) {
	r := newRestartRig(t, "", 300*time.Millisecond, time.Second, time.Hour)
	if errs, err := r.m.Apply(r.cfg("one")); err != nil {
		t.Fatal(errs, err)
	}
	pid := r.m.Pid()
	kill(t, pid)
	for r.m.Running() {
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.m.Stop(time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if r.m.Running() || r.m.Serving() || r.m.Counters().Restarts.Load() != 0 {
		t.Errorf("started again after a stop: running %v, serving %v, %d restarts", r.m.Running(), r.m.Serving(), r.m.Counters().Restarts.Load())
	}
}

// TestRestartAfterBothDie: v2026.10.14-2's CI - a new configuration the
// kernel kills HAProxy for, and the OOM killer takes the serving process
// too. Here the new process kills the old one and exits: nothing serves,
// the previous configuration is put back, and HAProxy starts again with it.
func TestRestartAfterBothDie(t *testing.T) {
	// With -sf OLD: kill OLD, exit at once. Otherwise: the real HAProxy.
	r := newRestartRig(t, `#!/bin/sh
prev=""
for a in "$@"; do [ "$prev" = -sf ] && { kill -9 "$a"; echo "[ALERT] killed with the previous one"; exit 1; }; prev=$a; done
exec %s "$@"
`, 100*time.Millisecond, time.Second, time.Hour)
	if errs, err := r.m.Apply(r.cfg("one")); err != nil {
		t.Fatal(errs, err)
	}
	pid := r.m.Pid()
	if _, err := r.m.Apply(r.cfg("two")); err == nil {
		t.Fatal("a reload whose process died was accepted")
	}
	r.servesAgain(t, "one", pid, 5*time.Second)
	c := r.m.Counters()
	if c.UnexpectedExits.Load() != 1 || c.Restarts.Load() != 1 {
		t.Errorf("counted %d exits, %d restarts; want 1, 1", c.UnexpectedExits.Load(), c.Restarts.Load())
	}
}
