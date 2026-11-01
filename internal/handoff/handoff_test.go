package handoff

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func flow(remote, local string) Flow {
	return Flow{Remote: netip.MustParseAddrPort(remote), Local: netip.MustParseAddrPort(local)}
}

func TestPackUnpack(t *testing.T) {
	in := []Flow{
		flow("198.51.100.7:51234", "172.16.10.151:443"),
		flow("[2001:db8::7]:40000", "[2a01:e0a:86c:7b55::151]:22067"),
		flow("203.0.113.9:1", "172.16.10.151:65535"),
	}
	v4, v6 := Pack(in)
	if len(v4) != 24 || len(v6) != 36 {
		t.Fatalf("sizes: %d %d", len(v4), len(v6))
	}
	got4, err := Unpack(v4, false)
	if err != nil {
		t.Fatal(err)
	}
	got6, err := Unpack(v6, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Flow{in[0], in[2], in[1]}; !slices.Equal(append(got4, got6...), want) {
		t.Errorf("round trip: %v", append(got4, got6...))
	}
	if _, err := Unpack(v4[:13], false); err == nil {
		t.Error("13 bytes taken for IPv4 connections")
	}
	// An IPv4-mapped address is IPv4.
	mapped := Flow{Remote: netip.AddrPortFrom(netip.MustParseAddr("::ffff:198.51.100.7"), 1), Local: netip.AddrPortFrom(netip.MustParseAddr("::ffff:172.16.10.151"), 443)}
	if v4, v6 := Pack([]Flow{mapped}); len(v4) != 12 || len(v6) != 0 {
		t.Errorf("a mapped address: %d %d", len(v4), len(v6))
	}
}

func TestDocument(t *testing.T) {
	if got := Document(nil, Guards{}); got != "destroy table ip janus_handoff\ndestroy table ip6 janus_handoff\n" {
		t.Errorf("no owner: %q", got)
	}
	// Guards alone: the refusal before conntrack, the hold after it.
	g := Document(nil, Guards{Refuse: []netip.Addr{netip.MustParseAddr("172.16.10.151")}, Hold: []netip.Addr{netip.MustParseAddr("172.16.10.151"), netip.MustParseAddr("2001:db8:1::151")}})
	for _, want := range []string{
		"table ip janus_handoff {\n\tchain prerouting {\n\t\ttype filter hook prerouting priority raw; policy accept;\n\t\tmeta l4proto != tcp accept\n\t\tip daddr { 172.16.10.151 } tcp flags & (syn | ack) == syn counter drop comment \"janus:refuse\"\n\t}\n",
		"\tchain hold {\n\t\ttype filter hook prerouting priority mangle; policy accept;\n\t\tip daddr { 172.16.10.151 } tcp flags & (syn | rst) == 0 ct state { new, invalid } counter drop comment \"janus:hold\"\n\t}\n}\n",
		"table ip6 janus_handoff {\n\tchain hold {",
		"ip6 daddr { 2001:db8:1::151 } tcp flags & (syn | rst) == 0",
	} {
		if !strings.Contains(g, want) {
			t.Errorf("missing %q in\n%s", want, g)
		}
	}
	if strings.Contains(g, "ip6 daddr { 2001:db8:1::151 } tcp flags & (syn | ack)") {
		t.Errorf("an IPv6 refusal no one asked for:\n%s", g)
	}
	doc := Document([]Owner{
		{Name: "lb-2", Reach4: netip.MustParseAddr("10.0.0.2"), Dev4: "eth0", Flows: []Flow{flow("198.51.100.8:2", "172.16.10.151:443"), flow("198.51.100.7:1", "172.16.10.151:443")}},
		{Name: "lb-1", Reach4: netip.MustParseAddr("10.0.0.1"), Reach6: netip.MustParseAddr("fe80::1%eth0"), Dev4: "eth0", Dev6: "eth0",
			Flows: []Flow{flow("[2001:db8::7]:40000", "[2001:db8:1::151]:443")}},
		// Nowhere to copy its IPv6 to: its rule left out.
		{Name: "lb-3", Reach4: netip.MustParseAddr("10.0.0.3"), Dev4: "eth0", Flows: []Flow{flow("[2001:db8::9]:9", "[2001:db8:1::151]:443")}},
	}, Guards{})
	for _, want := range []string{
		"destroy table ip janus_handoff\ndestroy table ip6 janus_handoff\n",
		// Owners by name: lb-1 is o0, lb-2 o1; lb-1 has no IPv4 flow.
		"\tset o1 {\n\t\ttype ipv4_addr . inet_service . ipv4_addr . inet_service\n\t\telements = {\n\t\t\t198.51.100.7 . 1 . 172.16.10.151 . 443,\n\t\t\t198.51.100.8 . 2 . 172.16.10.151 . 443\n\t\t}\n\t}\n",
		"ip saddr . tcp sport . ip daddr . tcp dport @o1 counter dup to 10.0.0.2 device \"eth0\" drop comment \"lb-2\"\n",
		"table ip6 janus_handoff {\n\tset o0 {",
		"ip6 saddr . tcp sport . ip6 daddr . tcp dport @o0 counter dup to fe80::1 device \"eth0\" drop comment \"lb-1\"\n",
		"type filter hook prerouting priority raw; policy accept;\n\t\tmeta l4proto != tcp accept\n",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing %q in\n%s", want, doc)
		}
	}
	if strings.Contains(doc, "lb-3\"") || strings.Contains(doc, "@o0 counter dup to 10.0.0.1") {
		t.Errorf("a rule without connections or reach:\n%s", doc)
	}
}

func TestParseCounters(t *testing.T) {
	v4 := []byte(`{"nftables":[{"metainfo":{"version":"1.1.7"}},{"table":{"family":"ip","name":"janus_handoff"}},
		{"rule":{"family":"ip","table":"janus_handoff","chain":"prerouting","expr":[{"match":{}}]}},
		{"rule":{"family":"ip","table":"janus_handoff","chain":"prerouting","comment":"lb-1","expr":[{"match":{}},{"counter":{"packets":489,"bytes":25666}},{"dup":{}},{"drop":null}]}}]}`)
	v6 := []byte(`{"nftables":[{"rule":{"comment":"lb-1","expr":[{"counter":{"packets":11,"bytes":1}}]}},{"rule":{"comment":"lb-2","expr":[{"counter":{"packets":3,"bytes":1}}]}}]}`)
	got, err := ParseCounters(v4, v6)
	if err != nil || got["lb-1"] != 500 || got["lb-2"] != 3 {
		t.Errorf("%v %v", got, err)
	}
}

// fakeNode is what a Manager drives: the members' connections it reads,
// the documents it applies.
type fakeNode struct {
	mu      sync.Mutex
	flows   map[string][]Flow
	readErr map[string]error
	applied []string
	failApp bool
	reads   int
}

func (f *fakeNode) manager(now *time.Time) *Manager {
	return &Manager{
		Read: func(_ context.Context, owner string, addrs []netip.Addr) (Reading, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.reads++
			if err := f.readErr[owner]; err != nil {
				return Reading{}, err
			}
			return Reading{Flows: slices.Clone(f.flows[owner]), Reach4: netip.MustParseAddr("10.0.0.1")}, nil
		},
		Device: func(netip.Addr) (string, error) { return "eth0", nil },
		Apply: func(doc string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.failApp {
				return errors.New("no dup here")
			}
			f.applied = append(f.applied, doc)
			return nil
		},
		Now:    func() time.Time { return *now },
		manual: true,
	}
}

func (f *fakeNode) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.applied) == 0 {
		return ""
	}
	return f.applied[len(f.applied)-1]
}

func TestManager(t *testing.T) {
	now := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	f := &fakeNode{flows: map[string][]Flow{"lb-1": {flow("198.51.100.7:1", "172.16.10.151:443")}}, readErr: map[string]error{}}
	m := f.manager(&now)
	addrs := []netip.Addr{netip.MustParseAddr("172.16.10.151")}
	ctx := context.Background()

	// A first reading that fails starts nothing.
	f.readErr["lb-1"] = errors.New("connection refused")
	if _, err := m.Start(ctx, "lb-1", addrs, time.Minute); err == nil || !strings.Contains(err.Error(), "reading lb-1's connections: connection refused") {
		t.Fatalf("a failed first reading: %v", err)
	}
	if len(m.List()) != 0 || len(f.applied) != 0 {
		t.Fatal("a handoff started anyway")
	}
	delete(f.readErr, "lb-1")

	info, err := m.Start(ctx, "lb-1", addrs, time.Minute)
	if err != nil || info.Flows != 1 || !info.Until.Equal(now.Add(time.Minute)) {
		t.Fatalf("start: %+v %v", info, err)
	}
	if !strings.Contains(f.last(), "198.51.100.7 . 1 . 172.16.10.151 . 443") || m.Document() != f.last() {
		t.Errorf("applied:\n%s", f.last())
	}

	// Read again when due: a new connection is copied too.
	f.mu.Lock()
	f.flows["lb-1"] = append(f.flows["lb-1"], flow("198.51.100.8:2", "172.16.10.151:443"))
	f.mu.Unlock()
	now = now.Add(FastEvery)
	if !m.tick() {
		t.Fatal("the loop ended with a handoff left")
	}
	if !strings.Contains(f.last(), "198.51.100.8 . 2") || m.List()[0].Flows != 2 {
		t.Errorf("after a reading: %+v\n%s", m.List(), f.last())
	}
	// Unchanged: nothing applied again.
	n := len(f.applied)
	now = now.Add(FastEvery)
	m.tick()
	if len(f.applied) != n {
		t.Error("the same connections applied again")
	}
	// A reading that fails keeps the copies (its member rebooting).
	f.readErr["lb-1"] = errors.New("unavailable")
	now = now.Add(FastEvery)
	m.tick()
	if l := m.List(); len(l) != 1 || l[0].Flows != 2 || l[0].Error != "unavailable" {
		t.Errorf("a failed reading: %+v", l)
	}
	delete(f.readErr, "lb-1")

	// Asked again: read at once, the end moved.
	info, err = m.Start(ctx, "lb-1", addrs, 2*time.Minute)
	if err != nil || !info.Until.Equal(now.Add(2*time.Minute)) || info.Error != "" {
		t.Errorf("again: %+v %v", info, err)
	}
	// Expired: the tables go, the loop would end.
	now = now.Add(3 * time.Minute)
	if m.tick() {
		t.Error("the loop goes on with no handoff")
	}
	if len(m.List()) != 0 || f.last() != Document(nil, Guards{}) {
		t.Errorf("expired: %+v\n%s", m.List(), f.last())
	}

	// A document the kernel refuses: no handoff, the previous document
	// kept for the firewall.
	f.failApp = true
	if _, err := m.Start(ctx, "lb-1", addrs, time.Minute); err == nil || !strings.Contains(err.Error(), "no dup here") {
		t.Errorf("refused: %v", err)
	}
	if len(m.List()) != 0 || m.Document() != Document(nil, Guards{}) {
		t.Errorf("after a refusal: %+v %q", m.List(), m.Document())
	}
	f.failApp = false

	// Stop.
	if _, err := m.Start(ctx, "lb-1", addrs, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop("lb-1"); err != nil || len(m.List()) != 0 || f.last() != Document(nil, Guards{}) {
		t.Errorf("stop: %v %+v", err, m.List())
	}
	if _, err := m.Start(ctx, "lb-1", addrs, 25*time.Hour); err == nil {
		t.Error("25 hours taken")
	}
	_ = m.Close()
}
