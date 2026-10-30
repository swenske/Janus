package logship

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/logvol"
)

// testFeed is a feed kept in memory: the event log as a target reads it.
type testFeed struct {
	mu      sync.Mutex
	items   []FeedItem
	changed chan struct{}
}

func newTestFeed() *testFeed { return &testFeed{changed: make(chan struct{})} }

func (f *testFeed) feed() Feed {
	return Feed{
		Name: SourceEvents,
		Since: func(id uint64) ([]FeedItem, uint64) {
			f.mu.Lock()
			defer f.mu.Unlock()
			var out []FeedItem
			for _, it := range f.items {
				if it.ID > id {
					out = append(out, it)
				}
			}
			return out, id
		},
		Latest: func() uint64 {
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.items) == 0 {
				return 0
			}
			return f.items[len(f.items)-1].ID
		},
		Changed: func() <-chan struct{} { f.mu.Lock(); defer f.mu.Unlock(); return f.changed },
	}
}

func (f *testFeed) publish(msg string) {
	f.mu.Lock()
	id := uint64(len(f.items) + 1)
	f.items = append(f.items, FeedItem{ID: id, Record: Record{Time: time.Now(), Source: SourceEvents, Service: "janusd", Message: msg}})
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

func testVolume(t *testing.T, size int64, boot byte) *logvol.Volume {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logs.img")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
	vol, err := logvol.Open(path, logvol.Options{BootID: [16]byte{boot}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vol.Close() })
	return vol
}

// count is how many messages the server holds with want.
func (srv *testServer) count(want string) int {
	n := 0
	for _, m := range srv.got() {
		if strings.Contains(m, want) {
			n++
		}
	}
	return n
}

// A paused target (docs/guide/log-forwarding.md, "Pausing a target")
// sends nothing and keeps its place: resumed, what the LOGS partition
// and the feeds took meanwhile goes from its cursors - across a restart
// of the shipper (janusd) too; what waits in memory only (the access
// logs, never on disk) isn't kept for it. Removed while paused, it
// forgets its place: added again, it starts from now.
func TestPausedTargetKeepsItsPlace(t *testing.T) {
	vol := testVolume(t, 8<<20, 3)
	events := newTestFeed()
	cursorFile := filepath.Join(t.TempDir(), "cursors.json")
	opts := Options{Volume: vol, Feeds: []Feed{events.feed()}, CursorFile: cursorFile}
	srv := newServer(t, "tcp", "octet-counting", nil)
	target := Target{Name: "t", Sources: []string{SourceJanusd, SourceEvents, SourceAccess}, Syslog: &SyslogTarget{Address: srv.addr(), Transport: "tcp"}}
	cfgWith := func(paused bool) Config {
		tt := target
		tt.Disabled = paused
		c := Config{Targets: []Target{tt}}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	line := vol.Line("janusd")
	flush := func() {
		t.Helper()
		if err := vol.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	access := func(sh *Shipper, msg string) {
		sh.Offer(Record{Time: time.Now(), Source: SourceAccess, Service: "haproxy", Severity: SevInfo, Message: msg})
	}

	sh := New(opts)
	sh.Apply(cfgWith(false))
	line("2026/10/29 22:00:00 before the pause")
	flush()
	events.publish("event before the pause")
	access(sh, "access before the pause")
	srv.waitFor(t, "before the pause", 3)

	// Paused: nothing goes, the status says so.
	sh.Apply(cfgWith(true))
	if st := sh.Status(); len(st) != 1 || st[0].State != "disabled" {
		t.Fatalf("paused: status %+v", st)
	}
	for i := range 3 {
		line(fmt.Sprintf("2026/10/29 22:00:01 during the pause %d", i))
	}
	flush()
	events.publish("event during the pause")
	access(sh, "access during the pause")

	// janusd restarts meanwhile: still paused, its place kept.
	sh.Close()
	line("2026/10/29 22:00:02 after a restart, still paused")
	flush()
	sh = New(opts)
	defer sh.Close()
	sh.Apply(cfgWith(true))
	access(sh, "access after the restart, still paused")
	time.Sleep(300 * time.Millisecond)
	if n := srv.count("pause"); n != 3 {
		t.Fatalf("a paused target sent: %q", srv.got())
	}

	// Resumed: from its cursors, in order - but not what waited in
	// memory only.
	sh.Apply(cfgWith(false))
	got := srv.waitFor(t, "during the pause", 3)
	if !strings.HasSuffix(got[0], "during the pause 0") || !strings.HasSuffix(got[2], "during the pause 2") {
		t.Fatalf("order: %q", got)
	}
	srv.waitFor(t, "after a restart, still paused", 1)
	srv.waitFor(t, "event during the pause", 1)
	access(sh, "access once resumed")
	srv.waitFor(t, "access once resumed", 1)
	if n := srv.count("access during the pause") + srv.count("access after the restart"); n != 0 {
		t.Errorf("access logs offered while paused were sent: %q", srv.got())
	}
	for _, m := range []string{" janusd - before the pause", "during the pause 1", "event during the pause"} {
		if n := srv.count(m); n != 1 {
			t.Errorf("%q sent %d times", m, n)
		}
	}

	// Paused, then removed: added again, from now.
	sh.Apply(cfgWith(true))
	line("2026/10/29 22:00:03 paused, then removed")
	flush()
	events.publish("event while removed")
	sh.Apply(Config{})
	sh.Apply(cfgWith(false))
	line("2026/10/29 22:00:04 added again")
	flush()
	srv.waitFor(t, "added again", 1)
	time.Sleep(300 * time.Millisecond)
	if n := srv.count("paused, then removed") + srv.count("event while removed"); n != 0 {
		t.Errorf("a target removed while paused, added again, sent what came before: %q", srv.got())
	}
}

// Paused longer than the LOGS partition holds: resumed, the lines it
// wrote over meanwhile are counted dropped, the rest sent - no error.
func TestPausedLongerThanTheVolumeCountsLost(t *testing.T) {
	vol := testVolume(t, 4<<20, 4)
	srv := newServer(t, "tcp", "octet-counting", nil)
	target := Target{Name: "t", Sources: []string{SourceJanusd}, Syslog: &SyslogTarget{Address: srv.addr(), Transport: "tcp"}}
	cfg := Config{Targets: []Target{target}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sh := New(Options{Volume: vol})
	defer sh.Close()
	sh.Apply(cfg)
	line, pad := vol.Line("janusd"), strings.Repeat("x", 1000)
	line("2026/10/29 22:00:00 before the pause")
	if err := vol.Flush(); err != nil {
		t.Fatal(err)
	}
	srv.waitFor(t, "before the pause", 1)

	paused := cfg
	paused.Targets = []Target{cfg.Targets[0]}
	paused.Targets[0].Disabled = true
	sh.Apply(paused)
	// 8 MiB of lines through a 4 MiB volume.
	const total = 8000
	for i := range total {
		line(fmt.Sprintf("2026/10/29 22:00:01 line %d %s", i, pad))
		if i%500 == 499 {
			if err := vol.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	sh.Apply(cfg)
	var st TargetStatus
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if st = sh.Status()[0]; st.Sent+st.Dropped >= total && st.QueuedRecords == 0 {
			break
		}
	}
	if st.Sent+st.Dropped != total || st.Dropped == 0 || st.Sent < total/4 || st.State != "ok" || st.LastError != "" {
		t.Fatalf("resumed: sent %d + dropped %d, want %d with some dropped; waiting %d, state %s %q", st.Sent, st.Dropped, total, st.QueuedRecords, st.State, st.LastError)
	}
}
