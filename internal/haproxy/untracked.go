package haproxy

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/sockdiag"
)

// An HAProxy process janusd doesn't run that still listens: v2026.10.14-5's
// lab saw a node answer on its HTTP port for minutes while every HAProxy
// janusd started was being killed - most likely an earlier process, still
// listening and serving its own configuration, unknown to janusd. Never
// reproduced since; this makes the next one visible. A process replaced
// by a reload closes its listeners at once, then finishes its connections:
// it isn't one.

// Untracked is an HAProxy process that listens but isn't the one janusd
// runs.
type Untracked struct {
	Pid       int
	Args      []string
	Started   time.Time // zero when /proc doesn't tell
	Listeners []netip.AddrPort
}

// ProcRoot is /proc - a var for tests.
var ProcRoot = "/proc"

// listening lists the system's listening TCP sockets by inode - a var for
// tests.
var listening = func() (map[uint32]netip.AddrPort, error) {
	out := map[uint32]netip.AddrPort{}
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := sockdiag.TCP(family, sockdiag.States(sockdiag.Listen), func(s sockdiag.Socket) { out[s.Inode] = s.Src }); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Untracked lists the HAProxy processes - by name, the Manager's binary's -
// that hold a listening socket but aren't the one the Manager runs, nor
// one it has just started (a reload: both listen until the old one hears
// -sf).
func (m *Manager) Untracked() ([]Untracked, error) {
	m.mu.Lock()
	var mine []int
	if m.cur != nil && !m.cur.exited() {
		mine = append(mine, m.cur.cmd.Process.Pid)
	}
	m.mu.Unlock()
	socks, err := listening()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(ProcRoot)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(m.BinaryPath)
	var out []Untracked
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || slices.Contains(mine, pid) {
			continue
		}
		dir := filepath.Join(ProcRoot, e.Name())
		comm, err := os.ReadFile(filepath.Join(dir, "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != name {
			continue
		}
		u := Untracked{Pid: pid, Listeners: listenersOf(dir, socks)}
		if len(u.Listeners) == 0 {
			continue // finishing its connections after a reload, or starting
		}
		if cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
			u.Args = strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		}
		u.Started = startedAt(dir)
		out = append(out, u)
	}
	return out, nil
}

// listenersOf is what process dir's file descriptors hold of socks.
func listenersOf(dir string, socks map[uint32]netip.AddrPort) []netip.AddrPort {
	fds, err := os.ReadDir(filepath.Join(dir, "fd"))
	if err != nil {
		return nil
	}
	var out []netip.AddrPort
	for _, fd := range fds {
		link, err := os.Readlink(filepath.Join(dir, "fd", fd.Name()))
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 32)
		if err != nil {
			continue
		}
		if a, ok := socks[uint32(n)]; ok && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b netip.AddrPort) int { return a.Compare(b) })
	return out
}

// startedAt is when process dir started: its start time in clock ticks
// since boot (/proc/<pid>/stat field 22) after the boot time (/proc/stat
// btime). USER_HZ is 100 on every architecture Janus runs.
func startedAt(dir string) time.Time {
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return time.Time{}
	}
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return time.Time{}
	}
	f := strings.Fields(s[i+1:]) // from field 3, state
	if len(f) < 20 {
		return time.Time{}
	}
	ticks, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return time.Time{}
	}
	sys, err := os.ReadFile(filepath.Join(ProcRoot, "stat"))
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(sys), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if btime, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return time.Unix(btime, 0).Add(time.Duration(ticks) * time.Second / 100)
			}
		}
	}
	return time.Time{}
}

// WatchUntracked looks for untracked HAProxy processes every interval:
// one seen at two looks in a row is reported - an haproxy.untracked event
// and a log line, once per process - and counted while it lasts
// (Counters.UntrackedListening, janus_haproxy_untracked_processes). Only
// janusd on a node: elsewhere, other HAProxy processes are the host's.
func (m *Manager) WatchUntracked(ctx context.Context, interval time.Duration) {
	seen := map[int]bool{}
	reported := map[int]bool{}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		found, err := m.Untracked()
		if err != nil {
			log.Printf("haproxy: looking for untracked processes: %v", err)
			continue
		}
		now := map[int]bool{}
		lasting := int64(0)
		for _, u := range found {
			now[u.Pid] = true
			if !seen[u.Pid] {
				continue // a reload's old process in the middle of stopping
			}
			lasting++
			if reported[u.Pid] {
				continue
			}
			reported[u.Pid] = true
			listeners := make([]string, len(u.Listeners))
			for i, a := range u.Listeners {
				listeners[i] = a.String()
			}
			ev := map[string]any{"pid": u.Pid, "args": u.Args, "listeners": listeners}
			age := ""
			if !u.Started.IsZero() {
				ev["started"] = u.Started.UTC().Format(time.RFC3339)
				age = fmt.Sprintf(", started %s ago", time.Since(u.Started).Round(time.Second))
			}
			log.Printf("haproxy: pid %d listens on %s but isn't the HAProxy janusd runs%s - it may serve another configuration", u.Pid, strings.Join(listeners, " "), age)
			events.Publish("haproxy.untracked", ev)
		}
		seen = now
		for pid := range reported {
			if !now[pid] {
				delete(reported, pid)
			}
		}
		m.counters.UntrackedListening.Store(lasting)
	}
}
