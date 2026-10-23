package haproxy

import "testing"

// testdata/show-peers*.txt: this project's HAProxy 3.4 build, two peers
// lb-1 and lb-2 over TLS seen from lb-1 (established, then just after
// lb-1's reload), and a node alone with its local peer.
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

func TestShowPeersUnsynced(t *testing.T) {
	// A peer behind on the local updates: not synced.
	tb := PeerTable{LastAcked: 4, TableLocalUpdate: 6}
	if tb.Synced() {
		t.Error("behind, yet synced")
	}
}
