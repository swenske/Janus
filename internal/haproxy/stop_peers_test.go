package haproxy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStopDoesntWaitForPeers runs the real build/haproxy with a peers
// section of its own, as janusd gives every node: stopped for good, it
// waits for a successor to teach its stick tables to (6 s on 3.4.7) -
// Stop ends it once StopWatch says its clients are served.
func TestStopDoesntWaitForPeers(t *testing.T) {
	bin, _ := filepath.Abs("../../build/haproxy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("build/haproxy not built (make haproxy-build)")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "admin.sock")
	peers := filepath.Join(dir, "peers.cfg")
	if err := os.WriteFile(peers, []byte(fmt.Sprintf("peers janus\n    bind 127.0.0.1:%d\n    server lonely\n", freePort(t))), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`global
    stats socket %s mode 600 level admin
defaults
    mode http
    timeout connect 1s
    timeout client 1s
    timeout server 1s
frontend f
    bind 127.0.0.1:%d
    http-request return status 200
backend t
    stick-table type ip size 1k expire 1h store http_req_cnt peers janus
`, sock, freePort(t))
	m := NewManager(bin, filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), sock)
	m.Output = &testWriter{t}
	m.ExtraConfigs = func() []string { return []string{peers} }
	m.LocalPeer = func() string { return "lonely" }
	asked := 0
	m.StopWatch = func(int) func() bool { return func() bool { asked++; return false } }
	if errs, err := m.Apply([]byte(cfg)); err != nil {
		t.Fatalf("Apply: %v %v", err, errs)
	}
	start := time.Now()
	if err := m.Stop(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second || asked == 0 {
		t.Errorf("stopped in %s, asked %d times - HAProxy's own wait for its peers is 6 s", took, asked)
	}
}
