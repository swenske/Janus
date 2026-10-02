package haproxy

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRefusedStart runs the real build/haproxy with a configuration
// `haproxy -c` accepts but HAProxy won't start with (a ulimit-n no process
// can get): Apply reports it with HAProxy's reason, the previous process
// keeps serving, the previous configuration is back on disk, and the next
// good Apply takes over from that process.
func TestRefusedStart(t *testing.T) {
	bin, _ := filepath.Abs("../../build/haproxy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("build/haproxy not built (make haproxy-build)")
	}
	dir := t.TempDir()
	m := NewManager(bin, filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), filepath.Join(dir, "admin.sock"))
	m.Output = &testWriter{t}
	port := freePort(t)
	cfg := func(body, global string) []byte {
		return fmt.Appendf(nil, `global
    stats socket %s mode 600 level admin
%s
defaults
    mode http
    timeout connect 1s
    timeout client 1s
    timeout server 1s
frontend web
    bind 127.0.0.1:%d
    http-request return status 200 content-type text/plain string %s
`, m.StatsSocketPath, global, port, body)
	}
	get := func() string {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			return err.Error()
		}
		defer resp.Body.Close()
		b := make([]byte, 64)
		n, _ := resp.Body.Read(b)
		return string(b[:n])
	}
	const impossible = "    ulimit-n 2000000000"

	// Nothing runs yet: refused, nothing serves.
	if errs, err := m.Apply(cfg("never", impossible)); !errors.As(err, new(*StartError)) || !strings.Contains(strings.Join(errs, "\n"), "FD limit") {
		t.Fatalf("first start: %v %v", errs, err)
	}
	if m.Running() || m.Serving() {
		t.Error("a refused first start left HAProxy marked running")
	}

	if errs, err := m.Apply(cfg("one", "")); err != nil {
		t.Fatal(errs, err)
	}
	t.Cleanup(func() { _ = m.Stop(5 * time.Second) })
	before, _ := m.ShowInfo()

	errs, err := m.Apply(cfg("two", impossible))
	if !errors.As(err, new(*StartError)) || !strings.Contains(err.Error(), "FD limit") {
		t.Fatalf("a refused reload: %v %v", errs, err)
	}
	if got := get(); got != "one" {
		t.Errorf("after a refused reload, served %q", got)
	}
	if after, _ := m.ShowInfo(); after.Pid != before.Pid || !m.Running() || !m.Serving() {
		t.Errorf("the previous process isn't the current one: %d -> %d", before.Pid, after.Pid)
	}
	if disk, _ := os.ReadFile(m.ConfigPath); string(disk) != string(cfg("one", "")) {
		t.Error("the refused configuration stayed on disk")
	}

	if errs, err := m.Apply(cfg("three", "")); err != nil {
		t.Fatal(errs, err)
	}
	if got := get(); got != "three" {
		t.Errorf("served %q", got)
	}
	// The first process got -sf: it's gone.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", before.Pid)); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("haproxy %d still runs after the next reload", before.Pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
