package haproxy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/events"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// fakeHAProxy stands in for the real binary: prints a line, exits on
// SIGUSR1 (HAProxy's soft-stop signal), otherwise runs forever.
func fakeHAProxy(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "haproxy")
	script := "#!/bin/sh\ntrap 'echo soft-stop; exit 0' USR1\necho started $$ args: \"$*\"\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestManagerProcessLifecycle(t *testing.T) {
	dir := t.TempDir()
	out := &syncBuffer{}
	m := NewManager(fakeHAProxy(t), filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), filepath.Join(dir, "sock"))
	m.Output = out

	_, evStart := events.Since(^uint64(0) - 1)
	if m.Running() {
		t.Fatal("Running before any start")
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	first := m.cur
	waitFor(t, "output captured", func() bool { return strings.Contains(out.String(), "started") })
	if !m.Running() {
		t.Fatal("not Running after start")
	}

	// A second start replaces the first as "current"; once the first
	// exits it must be reaped, not left as a zombie.
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	_ = first.cmd.Process.Signal(syscall.SIGKILL)
	waitFor(t, "replaced process reaped", first.exited)
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(first.cmd.Process.Pid))); !os.IsNotExist(err) {
		t.Errorf("replaced haproxy pid %d still present in /proc (zombie?): %v", first.cmd.Process.Pid, err)
	}
	if !m.Running() {
		t.Fatal("current process reported not running after the old one exited")
	}

	if err := os.WriteFile(m.PidPath, []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if m.Running() {
		t.Fatal("Running after Stop")
	}
	if !strings.Contains(out.String(), "soft-stop") {
		t.Errorf("Stop didn't soft-stop via SIGUSR1; output: %q", out.String())
	}
	if _, err := os.Stat(m.PidPath); !os.IsNotExist(err) {
		t.Errorf("pid file left behind after Stop: %v", err)
	}
	if err := m.Stop(time.Second); err != nil {
		t.Errorf("Stop on a stopped haproxy = %v, want nil", err)
	}

	var reasons []string
	waitFor(t, "exit events", func() bool {
		evs, _ := events.Since(evStart)
		reasons = reasons[:0]
		for _, e := range evs {
			if e.Type == "haproxy.exited" {
				var p struct{ Reason string }
				_ = json.Unmarshal(e.Payload, &p)
				reasons = append(reasons, p.Reason)
			}
		}
		return len(reasons) == 2
	})
	if strings.Join(reasons, ",") != "replaced by a reload,stopped" {
		t.Errorf("haproxy.exited reasons = %q", reasons)
	}
}

// HAProxy never writes its -p pid file in foreground mode, so the
// Manager must track the running pid itself for -sf - otherwise every
// reload leaves the old process (and its old config) serving too.
func TestManagerTakesOverWithSF(t *testing.T) {
	dir := t.TempDir()
	bin := fakeHAProxy(t)
	out := &syncBuffer{}
	newManager := func() *Manager {
		m := NewManager(bin, filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), filepath.Join(dir, "sock"))
		m.Output = out
		return m
	}
	m := newManager()
	defer func() {
		for _, line := range strings.Split(out.String(), "\n") {
			if f := strings.Fields(line); len(f) > 1 && f[0] == "started" {
				if pid, err := strconv.Atoi(f[1]); err == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
	}()

	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	first := m.cur.cmd.Process.Pid
	if got := readPID(t, m.PidPath); got != first {
		t.Errorf("pid file = %d, want %d", got, first)
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	second := m.cur.cmd.Process.Pid
	waitFor(t, "second start", func() bool { return strings.Contains(out.String(), "started "+strconv.Itoa(second)) })
	if want := "-sf " + strconv.Itoa(first); !strings.Contains(out.String(), "started "+strconv.Itoa(second)+" args: -f "+m.ConfigPath+" "+want+"\n") {
		t.Errorf("reload didn't pass %q:\n%s", want, out.String())
	}

	// A restarted janusd: a fresh Manager, haproxy still running.
	m2 := newManager()
	if err := m2.Reload(); err != nil {
		t.Fatal(err)
	}
	third := m2.cur.cmd.Process.Pid
	waitFor(t, "third start", func() bool { return strings.Contains(out.String(), "started "+strconv.Itoa(third)) })
	if !strings.Contains(out.String(), "started "+strconv.Itoa(third)+" args: -f "+m.ConfigPath+" -sf "+strconv.Itoa(second)+"\n") {
		t.Errorf("a restarted janusd didn't take over pid %d:\n%s", second, out.String())
	}

	// A pid file naming something that isn't haproxy is never signalled.
	m3 := newManager()
	if err := os.WriteFile(m3.PidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m3.previousPID(); got != 0 {
		t.Errorf("previousPID with a non-haproxy pid in the file = %d, want 0", got)
	}
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}
