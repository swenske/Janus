package haproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHandoverConfig(t *testing.T) {
	dir := t.TempDir()
	m := NewManager("haproxy", filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), filepath.Join(dir, "admin.sock"))
	m.ExtraConfigs = func() []string { return []string{"peers.cfg"} }
	m.LocalPeer = func() string { return "lb-1" }
	if got := m.configArgs("haproxy.cfg"); !slices.Equal(got, []string{"-f", "haproxy.cfg", "-f", "peers.cfg", "-L", "lb-1"}) {
		t.Errorf("without Handover: %q", got)
	}
	m.Handover = HandoverSocketFor(m.StatsSocketPath)
	if m.Handover != filepath.Join(dir, "haproxy-handover.sock") {
		t.Errorf("Handover %s", m.Handover)
	}
	cfg := filepath.Join(dir, HandoverConfigName)
	if got := m.configArgs("haproxy.cfg"); !slices.Equal(got, []string{"-f", "haproxy.cfg", "-f", "peers.cfg", "-f", cfg, "-L", "lb-1"}) {
		t.Errorf("with Handover: %q", got)
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\nglobal\n    stats socket "+m.Handover+" mode 600 level user expose-fd listeners\n") {
		t.Errorf("%s:\n%s", HandoverConfigName, data)
	}
	// Written once: the same content isn't rewritten.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(cfg, old, old); err != nil {
		t.Fatal(err)
	}
	m.configArgs("haproxy.cfg")
	if st, _ := os.Stat(cfg); !st.ModTime().Equal(old) {
		t.Error("rewritten with the same content")
	}
	// Someone else's content is put right.
	if err := os.WriteFile(cfg, []byte("global\n    stats socket /tmp/x expose-fd listeners\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.configArgs("haproxy.cfg")
	if got, _ := os.ReadFile(cfg); string(got) != string(data) {
		t.Errorf("not rewritten:\n%s", got)
	}
}

// A reload hands the listeners over only to a process that would open
// them the same: what shapes them is in the fingerprint, nothing else.
func TestListenFingerprint(t *testing.T) {
	dir := t.TempDir()
	old := ProcRoot
	ProcRoot = dir
	t.Cleanup(func() { ProcRoot = old })
	somaxconn := filepath.Join(dir, "sys/net/core/somaxconn")
	if err := os.MkdirAll(filepath.Dir(somaxconn), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(somaxconn, "60000\n")
	cfg := filepath.Join(dir, "haproxy.cfg")
	base := `global
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    maxconn 20000
defaults
    mode http
    timeout client 30s
frontend web
    bind :80
    stats uri /stats
    default_backend app
backend app
    server a 10.0.0.1:80 maxconn 50
`
	args := []string{"-f", cfg, "-L", "lb-1"}
	write(cfg, base)
	fp := listenFingerprint(args)
	if fp == "" {
		t.Fatal("no fingerprint")
	}
	same := map[string]string{
		"a server changed":         strings.Replace(base, "10.0.0.1:80 maxconn 50", "10.0.0.2:80 maxconn 80", 1),
		"a timeout changed":        strings.Replace(base, "timeout client 30s", "timeout client 1m", 1),
		"a comment, other spacing": strings.Replace(base, "    bind :80", "    bind   :80   # the site", 1),
		"the stats page's uri":     strings.Replace(base, "stats uri /stats", "stats uri /haproxy", 1),
	}
	for what, c := range same {
		write(cfg, c)
		if got := listenFingerprint(args); got != fp {
			t.Errorf("%s: the fingerprint changed", what)
		}
	}
	different := map[string]string{
		"a bind changed":           strings.Replace(base, "bind :80", "bind :80 backlog 1", 1),
		"a bind added":             strings.Replace(base, "    bind :80\n", "    bind :80\n    bind :8080\n", 1),
		"the frontend's backlog":   strings.Replace(base, "    default_backend app\n", "    backlog 100\n    default_backend app\n", 1),
		"maxconn":                  strings.Replace(base, "maxconn 20000", "maxconn 30000", 1),
		"maxconn in the defaults":  strings.Replace(base, "    mode http\n", "    mode http\n    maxconn 500\n", 1),
		"threads":                  strings.Replace(base, "    maxconn 20000\n", "    maxconn 20000\n    nbthread 2\n", 1),
		"a tune.* setting":         strings.Replace(base, "    maxconn 20000\n", "    maxconn 20000\n    tune.bufsize 32768\n", 1),
		"a stats socket":           strings.Replace(base, "level admin", "level operator", 1),
		"the bind moved elsewhere": strings.Replace(base, "frontend web", "frontend www", 1),
	}
	for what, c := range different {
		write(cfg, c)
		if got := listenFingerprint(args); got == fp {
			t.Errorf("%s: the fingerprint didn't change", what)
		}
	}
	write(cfg, base)
	write(somaxconn, "4096\n")
	if listenFingerprint(args) == fp {
		t.Error("net.core.somaxconn changed, the fingerprint didn't")
	}
	write(somaxconn, "60000\n")
	if listenFingerprint([]string{"-f", cfg, "-L", "lb-2"}) == fp {
		t.Error("another local peer, the same fingerprint")
	}
	if listenFingerprint([]string{"-f", filepath.Join(dir, "nope.cfg")}) != "" {
		t.Error("a fingerprint without the file")
	}
}

// handoverHAProxy starts the real build/haproxy under a Manager with
// Handover set (or not): a frontend answering 200, and what it listens on.
func handoverHAProxy(t *testing.T, handover bool) (*Manager, string) {
	t.Helper()
	bin, _ := filepath.Abs("../../build/haproxy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("build/haproxy not built (make haproxy-build)")
	}
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	sock := filepath.Join(dir, "admin.sock")
	cfg := fmt.Sprintf(`global
    stats socket %s mode 600 level admin
defaults
    mode http
    timeout connect 1s
    timeout client 30s
    timeout server 1s
    option idle-close-on-response
frontend f
    bind %s
    http-request return status 200 content-type text/plain string "ok\n"
`, sock, addr)
	m := NewManager(bin, filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), sock)
	m.Output = &testWriter{t}
	if handover {
		m.Handover = HandoverSocketFor(sock)
	}
	if errs, err := m.Apply([]byte(cfg)); err != nil {
		t.Fatalf("Apply: %v %v", err, errs)
	}
	t.Cleanup(func() { _ = m.Stop(5 * time.Second) })
	return m, addr
}

// current is the running process: its pid, and whether it was handed
// the listeners.
func (m *Manager) current() (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur.cmd.Process.Pid, m.cur.handedOver
}

// idleClient opens a keep-alive connection to addr, asks once, and keeps
// it open: with idle-close-on-response the process that accepted it
// lives on through its soft-stop.
func idleClient(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(c).ReadString('\n'); err != nil || !strings.Contains(line, " 200 ") {
		t.Fatalf("idle client: %q %v", line, err)
	}
}

// #680: with Handover, reloads under a load of new connections - one per
// request, straight to HAProxy - fail none of them: the listeners are
// handed over, never closed, so nothing waiting in their queue is reset.
// Right after each Reload, the stats socket answers the new process only,
// though the previous one lives on (a keep-alive client keeps it).
func TestReloadHandsListenersOver(t *testing.T) {
	m, addr := handoverHAProxy(t, true)
	if _, handed := m.current(); handed {
		t.Error("a first start handed over")
	}
	var requests, failures atomic.Int64
	var firstFailure atomic.Value
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				requests.Add(1)
				if err := fresh(addr); err != nil {
					failures.Add(1)
					firstFailure.CompareAndSwap(nil, err.Error())
				}
			}
		}()
	}
	for i := range 20 {
		idleClient(t, addr)
		old, _ := m.current()
		if err := m.Reload(); err != nil {
			t.Fatalf("Reload %d: %v", i, err)
		}
		pid, handed := m.current()
		if !handed {
			t.Fatalf("reload %d: the listeners weren't handed over", i)
		}
		for range 20 {
			out, err := m.statsCommand("show info")
			if err != nil {
				t.Fatal(err)
			}
			if got := parseShowInfo(string(out)).Pid; got != pid {
				t.Fatalf("reload %d: the stats socket answered pid %d, the new process is %d", i, got, pid)
			}
		}
		if n := sharedSockets(old, pid); n != 0 {
			t.Errorf("reload %d: pid %d still holds %d of the listeners it handed over", i, old, n)
		}
		time.Sleep(200 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	t.Logf("%d requests over 20 reloads, %d failed", requests.Load(), failures.Load())
	if f := failures.Load(); f > 0 {
		t.Errorf("%d of %d requests failed across the reloads - the first: %v", f, requests.Load(), firstFailure.Load())
	}
}

// fresh asks addr once over a connection of its own.
func fresh(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		return err
	}
	resp, err := io.ReadAll(c)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(resp), "HTTP/1.0 200") && !strings.HasPrefix(string(resp), "HTTP/1.1 200") {
		return fmt.Errorf("answered %q", resp)
	}
	return nil
}

// A reload that changes a listener opens new ones - a socket handed over
// keeps the backlog it was opened with -, and the next reload with
// nothing changed hands over again. A janusd that restarted doesn't
// know what the running process opened its listeners with: its first
// reload opens new ones too.
func TestReloadChangingListenersOpensThem(t *testing.T) {
	m, addr := handoverHAProxy(t, true)
	cfg, err := os.ReadFile(m.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if errs, err := m.Apply([]byte(strings.Replace(string(cfg), "    bind "+addr+"\n", "    bind "+addr+" backlog 7\n", 1))); err != nil {
		t.Fatal(errs, err)
	}
	if _, handed := m.current(); handed {
		t.Error("a new backlog applied with the listeners handed over: it wouldn't take")
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, handed := m.current(); !handed {
		t.Error("a reload changing nothing didn't hand over")
	}

	// janusd restarted: a Manager of its own takes the process over -
	// the previous one gone, it doesn't see its process replaced.
	m.mu.Lock()
	m.cur = nil
	m.mu.Unlock()
	m2 := NewManager(m.BinaryPath, m.ConfigPath, m.PidPath, m.StatsSocketPath)
	m2.Output, m2.Handover = m.Output, m.Handover
	t.Cleanup(func() { _ = m2.Stop(5 * time.Second) })
	if err := m2.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, handed := m2.current(); handed {
		t.Error("handed over from a process this janusd didn't start")
	}
	if err := m2.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, handed := m2.current(); !handed {
		t.Error("the next reload didn't hand over")
	}
	if err := fresh(addr); err != nil {
		t.Fatal(err)
	}
}

// A process that doesn't answer on the Handover socket - started before
// Handover was set, by an older janusd - gets a plain -sf: -x to it would
// have the new HAProxy exit. The next reload hands over.
func TestReloadWithoutHandoverSocket(t *testing.T) {
	m, addr := handoverHAProxy(t, false)
	m.mu.Lock()
	m.Handover = HandoverSocketFor(m.StatsSocketPath)
	m.mu.Unlock()
	if err := m.Reload(); err != nil {
		t.Fatalf("reload from a process without the socket: %v", err)
	}
	if _, handed := m.current(); handed {
		t.Error("-x to a process that doesn't expose its listeners")
	}
	if err := m.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, handed := m.current(); !handed {
		t.Error("the next reload didn't hand over")
	}
	if err := fresh(addr); err != nil {
		t.Fatal(err)
	}
}
