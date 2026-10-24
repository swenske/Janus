package haproxy

// HAProxy's "show peers": the peers section janusd generates for a
// cluster (internal/cluster) - each peer connected or not, and for each
// stick table whether this node has taught the peer every local update.
// Answers captured from this project's HAProxy 3.4 build
// (testdata/show-peers*.txt).

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Peer is one peer of a peers section as HAProxy reports it.
type Peer struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	// Local: this node itself.
	Local bool `json:"local"`
	// Active: HAProxy connects to it (a local peer is inactive).
	Active bool `json:"active"`
	// HAProxy's words: last_status (NONE, HSHK, ESTA, ERR...), app_state
	// (STOPPED, RUNNING), learn_state (NOTASSIGNED, ASSIGNED,
	// PROCESSING, FINISHED).
	LastStatus string `json:"last_status"`
	AppState   string `json:"app_state"`
	LearnState string `json:"learn_state"`
	// LastHandshake as HAProxy prints it: "3s", "<NEVER>".
	LastHandshake  string      `json:"last_handshake"`
	NewConnections uint64      `json:"new_connections"`
	ProtocolErrors uint64      `json:"protocol_errors"`
	Tables         []PeerTable `json:"tables"`
}

// Up reports whether the peer is connected.
func (p Peer) Up() bool { return p.LastStatus == "ESTA" }

// PeerTable is one stick table as seen from one peer.
type PeerTable struct {
	Name string `json:"name"`
	// HAProxy's counters for the table on this peer (src/peers.c):
	// LastGet is the peer's last update this node received, LastAcked
	// the last this node acknowledged to it; LastPushed is this node's
	// last update sent to the peer, Update the last the peer
	// acknowledged.
	LastAcked      uint64 `json:"last_acked"`
	LastPushed     uint64 `json:"last_pushed"`
	LastGet        uint64 `json:"last_get"`
	TeachingOrigin uint64 `json:"teaching_origin"`
	Update         uint64 `json:"update"`
	// The table's own counters: its last update id (from anywhere), and
	// its last local one.
	TableUpdate      uint64 `json:"table_update"`
	TableLocalUpdate uint64 `json:"table_local_update"`
}

// Synced reports whether the peer acknowledged every update made on
// this node: its acknowledgement (Update) reached the table's last
// local update. LastAcked goes the other way - on the member that
// serves the traffic and the peer that only learns, it stays 0. The
// counters are 32 bits and wrap: compared as HAProxy does.
func (t PeerTable) Synced() bool {
	return int32(uint32(t.TableLocalUpdate)-uint32(t.Update)) <= 0 //nolint:gosec // G115: HAProxy's unsigned 32-bit counters, compared modulo 2^32
}

var (
	peerIDRE    = regexp.MustCompile(`^([^()]+)\((local|remote),(active|inactive)\)$`)
	peersHeadRE = regexp.MustCompile(`^0x[0-9a-f]+: \[[^\]]*\] (id=.*)$`)
)

// PeersSection is a peers section as HAProxy reports it.
type PeersSection struct {
	Peers []Peer
	// Learnt: this HAProxy process has learnt the section's stick tables
	// since it started - from the process it replaced, or from a remote
	// peer. Measured on 3.4.7: a process reloaded among remote peers has
	// empty tables until its resync timeout (5 s) passes and a peer
	// teaches it; the process it replaced is gone at once.
	Learnt bool
}

// The section's resync flags (HAProxy's PEERS_F_RESYNC_*_FINISHED).
const (
	peersResyncLocalFinished  = 0x1
	peersResyncRemoteFinished = 0x2
)

// ShowPeersSection runs "show peers <section>": its peers, and whether
// the tables are learnt.
func (m *Manager) ShowPeersSection(section string) (*PeersSection, error) {
	arg, err := cliToken("peers section", section)
	if err != nil {
		return nil, err
	}
	out, err := m.statsCommand("show peers " + arg)
	if err != nil {
		return nil, err
	}
	peers, err := parseShowPeers(string(out))
	if err != nil {
		return nil, err
	}
	return &PeersSection{Peers: peers, Learnt: peersLearnt(string(out))}, nil
}

// ShowPeers runs "show peers <section>": its peers.
func (m *Manager) ShowPeers(section string) ([]Peer, error) {
	s, err := m.ShowPeersSection(section)
	if err != nil {
		return nil, err
	}
	return s.Peers, nil
}

// peersLearnt reads the section's flags ("flags=0x28900001"): both
// resyncs finished.
func peersLearnt(out string) bool {
	for _, raw := range strings.Split(out, "\n") {
		if peersHeadRE.MatchString(raw) {
			v, err := strconv.ParseUint(strings.TrimPrefix(keyValues(raw)["flags"], "0x"), 16, 64)
			const both = peersResyncLocalFinished | peersResyncRemoteFinished
			return err == nil && v&both == both
		}
	}
	return false
}

// parseShowPeers reads one section's answer. Lines are nested by
// indentation: the section, its peers ("0x...: id=name(remote,active)
// addr=..."), each peer's counters, then "shared tables:" with, per
// table, an id line, a counters line and the table's own line.
func parseShowPeers(out string) ([]Peer, error) {
	var peers []Peer
	var cur *Peer
	var tbl *PeerTable
	section := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(raw, "0x") {
			if !peersHeadRE.MatchString(raw) {
				return nil, fmt.Errorf("show peers: %s", line)
			}
			section = true
			continue
		}
		if !section {
			return nil, fmt.Errorf("show peers: %s", line)
		}
		kv := keyValues(line)
		switch {
		case strings.HasPrefix(raw, "  0x") && kv["id"] != "":
			// A peer.
			m := peerIDRE.FindStringSubmatch(kv["id"])
			if m == nil {
				return nil, fmt.Errorf("show peers: peer %q", kv["id"])
			}
			peers = append(peers, Peer{Name: m[1], Local: m[2] == "local", Active: m[3] == "active", Address: kv["addr"],
				LastStatus: kv["last_status"], AppState: kv["app_state"], LearnState: kv["learn_state"], LastHandshake: kv["last_hdshk"]})
			cur, tbl = &peers[len(peers)-1], nil
		case cur == nil:
			continue
		case strings.HasPrefix(line, "0x") && kv["local_id"] != "":
			// A shared table's head: its counters follow.
			cur.Tables = append(cur.Tables, PeerTable{})
			tbl = &cur.Tables[len(cur.Tables)-1]
		case tbl != nil && strings.HasPrefix(line, "last_acked="):
			tbl.LastAcked, tbl.LastPushed, tbl.LastGet = number(kv["last_acked"]), number(kv["last_pushed"]), number(kv["last_get"])
			tbl.TeachingOrigin, tbl.Update = number(kv["teaching_origin"]), number(kv["update"])
		case tbl != nil && strings.HasPrefix(line, "table:"):
			tbl.Name, tbl.TableUpdate, tbl.TableLocalUpdate = kv["id"], number(kv["update"]), number(kv["localupdate"])
		case kv["new_conn"] != "":
			cur.NewConnections, cur.ProtocolErrors = number(kv["new_conn"]), number(kv["proto_err"])
		}
	}
	if !section {
		return nil, errors.New("show peers: " + strings.TrimSpace(out))
	}
	return peers, nil
}

// keyValues are a line's key=value tokens.
func keyValues(line string) map[string]string {
	out := map[string]string{}
	for _, f := range strings.Fields(line) {
		if k, v, ok := strings.Cut(f, "="); ok {
			out[k] = v
		}
	}
	return out
}

func number(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}
