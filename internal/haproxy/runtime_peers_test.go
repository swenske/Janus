package haproxy

import (
	"testing"
	"time"
)

// testdata/show-peers*.txt: this project's HAProxy 3.4 build, two peers
// lb-1 and lb-2 over TLS seen from lb-1 (established, then just after
// lb-1's reload), and a node alone with its local peer. The one-way and
// peer-behind captures: two peers on loopback, traffic to lb-1 only
// (3.4.7).
func TestParseShowPeers(t *testing.T) {
	peers, err := parseShowPeers(fixture(t, "show-peers-established.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("%d peers", len(peers))
	}
	remote, local := peers[0], peers[1]
	if remote.Name != "lb-2" || remote.Local || !remote.Active || !remote.Up() || remote.Address != "127.0.0.1:20058" ||
		remote.AppState != "RUNNING" || remote.LearnState != "NOTASSIGNED" || remote.LastHandshake != "2s" || remote.NewConnections != 1 || remote.ProtocolErrors != 0 {
		t.Errorf("remote: %+v", remote)
	}
	if len(remote.Tables) != 1 {
		t.Fatalf("remote tables: %+v", remote.Tables)
	}
	if tb := remote.Tables[0]; tb.Name != "fe" || tb.LastAcked != 2 || tb.LastPushed != 2 || tb.LastGet != 2 || tb.Update != 2 || tb.TableUpdate != 3 || tb.TableLocalUpdate != 2 || !tb.Synced() {
		t.Errorf("remote table: %+v", tb)
	}
	if local.Name != "lb-1" || !local.Local || local.Active || local.Up() || local.LastStatus != "NONE" || local.LastHandshake != "<NEVER>" || local.Address != "127.0.0.1:20057" {
		t.Errorf("local: %+v", local)
	}

	peers, err = parseShowPeers(fixture(t, "show-peers-after-reload.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// The new process has no update yet, local or learnt: nothing to
	// teach, so the peer counts as in sync.
	if tb := peers[0].Tables[0]; !peers[0].Up() || peers[0].LastHandshake != "1s" || tb.LastAcked != 0 || tb.TableUpdate != 0 || tb.TableLocalUpdate != 0 || !tb.Synced() {
		t.Errorf("after reload: %+v %+v", peers[0], tb)
	}

	peers, err = parseShowPeers(fixture(t, "show-peers-alone-bind.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || !peers[0].Local || peers[0].Name != "lonely" || len(peers[0].Tables) != 1 || peers[0].Tables[0].TableLocalUpdate != 3 {
		t.Errorf("alone: %+v", peers)
	}

	if _, err := parseShowPeers("No such peers section.\n"); err == nil {
		t.Error("an unknown section parsed")
	}
}

// Synced reads the peer's acknowledgement of this node's updates
// (HAProxy's "update"), not last_acked - this node's of the peer's,
// which stays 0 on the member serving the traffic (v2026.10.24's lab:
// the VIP's master showed its peer "catching up" for good).
func TestShowPeersSynced(t *testing.T) {
	for _, c := range []struct {
		file, peer   string
		acked, local uint64
		lastAcked    uint64
		synced       bool
	}{
		// lb-1 served 15 requests, lb-2 acknowledged them all.
		{"show-peers-one-way-pusher.txt", "lb-2", 15, 15, 0, true},
		// lb-2 only learnt: nothing of its own to teach.
		{"show-peers-one-way-receiver.txt", "lb-1", 0, 0, 15, true},
		// lb-2 stopped, lb-1 served 3 more: behind.
		{"show-peers-peer-behind.txt", "lb-2", 15, 18, 0, false},
	} {
		peers, err := parseShowPeers(fixture(t, c.file))
		if err != nil {
			t.Fatal(err)
		}
		var tb *PeerTable
		for _, p := range peers {
			if p.Name == c.peer {
				tb = &p.Tables[0]
			}
		}
		if tb == nil || tb.Update != c.acked || tb.TableLocalUpdate != c.local || tb.LastAcked != c.lastAcked || tb.Synced() != c.synced {
			t.Errorf("%s: %+v, want acked %d, local %d, last_acked %d, synced %v", c.file, tb, c.acked, c.local, c.lastAcked, c.synced)
		}
	}
	// The counters wrap at 2^32, as HAProxy's do.
	for _, c := range []struct {
		acked, local uint64
		synced       bool
	}{
		{4, 6, false},
		{6, 6, true},
		{0xfffffffe, 3, false},
		{3, 3, true},
		{2, 0xfffffffe, true}, // acknowledged past the wrap
	} {
		if got := (PeerTable{Update: c.acked, TableLocalUpdate: c.local}).Synced(); got != c.synced {
			t.Errorf("acked %#x, local %#x: synced %v, want %v", c.acked, c.local, got, c.synced)
		}
	}
}

// A section's tables are learnt once both resyncs finished: not right
// after a reload among remote peers, nor while a new member waits for a
// peer to teach it (fixtures captured from 3.4.7).
func TestPeersLearnt(t *testing.T) {
	for file, want := range map[string]bool{
		"show-peers-after-reload.txt":        false, // flags=0x0
		"show-peers-established.txt":         false, // 0x28900001: local only
		"show-peers-alone-bind.txt":          true,  // 0x300003
		"show-peers-alone-bind-reloaded.txt": true,  // 0x11000003
	} {
		if got := peersLearnt(fixture(t, file)); got != want {
			t.Errorf("%s: learnt %v, want %v", file, got, want)
		}
	}
	if peersLearnt("No such peers section.\n") {
		t.Error("no section, learnt")
	}
}

// Under a steady stream of updates a peer is never at the very last one:
// read again, it is in sync if it acknowledged by then everything the
// first read showed - and still behind if it didn't.
func TestPeersSettle(t *testing.T) {
	peers := func(lb2, lb3, local uint64) []Peer {
		return []Peer{
			{Name: "lb-1", Local: true, Tables: []PeerTable{{Name: "fe", TableLocalUpdate: local}}},
			{Name: "lb-2", Tables: []PeerTable{{Name: "fe", Update: lb2, TableLocalUpdate: local}}},
			{Name: "lb-3", Tables: []PeerTable{{Name: "fe", Update: lb3, TableLocalUpdate: local}}},
		}
	}
	before, after := peers(90, 40, 100), peers(120, 60, 130)
	if before[1].Tables[0].Synced() || after[1].Tables[0].Synced() {
		t.Fatal("a peer behind the last update reads in sync before settling")
	}
	settle(before, after)
	if !after[1].Tables[0].Synced() || after[2].Tables[0].Synced() || after[0].Tables[0].KeptUp {
		t.Errorf("settled: lb-2 %+v, lb-3 %+v, local %+v", after[1].Tables[0], after[2].Tables[0], after[0].Tables[0])
	}
	// The 32-bit counters wrap.
	before, after = peers(0xfffffff0, 0, 0xfffffff8), peers(0x10, 0, 0x20)
	settle(before, after)
	if !after[1].Tables[0].Synced() {
		t.Error("a peer that kept up across the wrap reads behind")
	}
}

// A peer behind at the first read is read again until it has
// acknowledged what the first read had - kept up -, or the window ends.
func TestPeersSettled(t *testing.T) {
	section := func(lb2, local uint64) *PeersSection {
		return &PeersSection{Peers: []Peer{
			{Name: "lb-1", Local: true, Tables: []PeerTable{{Name: "fe", TableLocalUpdate: local}}},
			{Name: "lb-2", Tables: []PeerTable{{Name: "fe", Update: lb2, TableLocalUpdate: local}}},
		}}
	}
	reads := func(s ...*PeersSection) (func() (*PeersSection, error), *int) {
		n := 0
		return func() (*PeersSection, error) {
			r := s[min(n, len(s)-1)]
			n++
			return r, nil
		}, &n
	}
	// In sync at once: one read.
	read, n := reads(section(100, 100))
	if got, _ := settled(read, time.Second, time.Millisecond); !got.Peers[1].Tables[0].Synced() || *n != 1 {
		t.Errorf("in sync: %d reads, %+v", *n, got.Peers[1].Tables[0])
	}
	// Under traffic, slow: 100 acknowledged at the third read.
	read, n = reads(section(90, 100), section(95, 110), section(100, 120), section(130, 140))
	if got, _ := settled(read, time.Second, time.Millisecond); !got.Peers[1].Tables[0].Synced() || *n != 3 {
		t.Errorf("kept up: %d reads, %+v", *n, got.Peers[1].Tables[0])
	}
	// Really behind: still so when the window ends.
	read, n = reads(section(10, 100), section(12, 110))
	if got, _ := settled(read, 20*time.Millisecond, time.Millisecond); got.Peers[1].Tables[0].Synced() || *n < 3 {
		t.Errorf("behind: %d reads, %+v", *n, got.Peers[1].Tables[0])
	}
}
