package haproxy

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReloadWaitsForTheTablesLearnt runs two real build/haproxy members of
// one peers section, started together - so neither has learnt its tables
// for the next ~5 s (each one's lesson from the other is partial) - and
// reloads one of them inside that window, as #701's lab campaign did
// under load. Without PeersSection, the new process starts with an empty
// table: the old one, not done learning, taught it nothing. With it, the
// reload waits until the running process has learnt, and the new one has
// the table at once.
func TestReloadWaitsForTheTablesLearnt(t *testing.T) {
	bin, _ := filepath.Abs("../../build/haproxy")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("build/haproxy not built (make haproxy-build)")
	}
	if testing.Short() {
		t.Skip("about 15 s of HAProxy resync timeouts")
	}
	for _, wait := range []bool{false, true} {
		t.Run(fmt.Sprintf("wait=%v", wait), func(t *testing.T) {
			a, b := newMember(t, bin, "a"), newMember(t, bin, "b")
			a.peersWith(t, b)
			b.peersWith(t, a)
			if wait {
				a.m.PeersSection = "janus"
			}
			for _, x := range []*member{a, b} {
				if errs, err := x.m.Apply([]byte(x.config())); err != nil {
					t.Fatalf("Apply %s: %v %v", x.name, err, errs)
				}
			}
			t.Cleanup(func() {
				_ = a.m.Stop(10 * time.Second)
				_ = b.m.Stop(10 * time.Second)
			})
			for range 5 {
				req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", a.front), nil)
				req.Header.Set("x-key", "k")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
			}
			waitFor(t, "b to have the key", func() bool { return b.count() == 5 })
			if s, err := a.m.ShowPeersSection("janus"); err != nil || s.Learnt {
				t.Fatalf("a has learnt its tables already (%v) - the window this test needs is gone", err)
			}

			start := time.Now()
			if err := a.m.Reload(); err != nil {
				t.Fatal(err)
			}
			took := time.Since(start)
			got := -1
			for range 5 {
				if got = a.count(); got == 5 {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Logf("reload took %s, the new process's count: %d", took.Round(100*time.Millisecond), got)
			switch {
			case wait && got != 5:
				t.Errorf("the new process's count is %d, want 5: the reload didn't wait for the tables", got)
			case wait && took < time.Second:
				t.Errorf("the reload took %s: it didn't wait for the running process to learn", took)
			case !wait && got == 5:
				// HAProxy taught a process replaced before it had learnt:
				// waitLearnt may no longer be needed (src/peers.c).
				t.Errorf("without the wait the new process had the table at once - HAProxy changed")
			}
		})
	}
}

// member is one HAProxy of the test's peers section: its Manager, its
// frontend's port and its peers listener's.
type member struct {
	m            *Manager
	name         string
	front, peers int
}

func newMember(t *testing.T, bin, name string) *member {
	t.Helper()
	dir := t.TempDir()
	m := NewManager(bin, filepath.Join(dir, "haproxy.cfg"), filepath.Join(dir, "haproxy.pid"), filepath.Join(dir, "admin.sock"))
	m.Output = &testWriter{t}
	m.LearnWait = 8 * time.Second
	m.LocalPeer = func() string { return name }
	peers := filepath.Join(dir, "peers.cfg")
	m.ExtraConfigs = func() []string { return []string{peers} }
	return &member{m: m, name: name, front: freePort(t), peers: freePort(t)}
}

// peersWith writes x's peers section as janusd's cluster.PeersConfig
// does: a bind, the local peer without an address, the other one with.
func (x *member) peersWith(t *testing.T, other *member) {
	t.Helper()
	cfg := fmt.Sprintf("peers janus\n    bind 127.0.0.1:%d\n    server %s\n    server %s 127.0.0.1:%d\n", x.peers, x.name, other.name, other.peers)
	if err := os.WriteFile(x.m.ExtraConfigs()[0], []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// config counts the requests of each x-key in a table the peers share.
func (x *member) config() string {
	return fmt.Sprintf(`global
    stats socket %s mode 600 level admin
defaults
    mode http
    timeout connect 1s
    timeout client 1s
    timeout server 1s
frontend f
    bind 127.0.0.1:%d
    http-request track-sc0 req.hdr(x-key) table st
    http-request return status 200
backend st
    stick-table type string len 32 size 1k expire 1h store http_req_cnt peers janus
`, x.m.StatsSocketPath, x.front)
}

// count is key k's http_req_cnt in x's table, -1 without it.
func (x *member) count() int {
	out, err := x.m.statsCommand("show table st")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(out), "\n") {
		i := strings.Index(line, "http_req_cnt=")
		if !strings.Contains(line, " key=k ") || i < 0 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(line[i:], "http_req_cnt=%d", &n); err == nil {
			return n
		}
	}
	return -1
}
