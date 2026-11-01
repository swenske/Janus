package handoff

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// A handoff whose member gives no address to copy a family's connections
// to doesn't start: it would copy nothing, and say nothing.
func TestUnreachableOwner(t *testing.T) {
	now := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	f := &fakeNode{flows: map[string][]Flow{"lb-2": {flow("[2001:db8::7]:1", "[2001:db8:1::151]:443")}}, readErr: map[string]error{}}
	m := f.manager(&now) // its readings give an IPv4 address alone
	_, err := m.Start(context.Background(), "lb-2", []netip.Addr{netip.MustParseAddr("2001:db8:1::151")}, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "lb-2's connections: IPv6 connections, but no IPv6 address of its own on their link") {
		t.Errorf("no IPv6 address to copy to: %v", err)
	}
	if len(m.List()) != 0 || len(f.applied) != 0 {
		t.Error("started anyway")
	}
}

// A refusal and a hold come into the tables and go once over, alone or
// beside a handoff.
func TestGuards(t *testing.T) {
	now := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	f := &fakeNode{flows: map[string][]Flow{"lb-2": {flow("198.51.100.7:1", "172.16.10.151:443")}}, readErr: map[string]error{}}
	m := f.manager(&now)
	addrs := []netip.Addr{netip.MustParseAddr("172.16.10.151")}
	if _, err := m.Guard(true, addrs, 2*time.Hour); err == nil {
		t.Error("a refusal of 2 hours taken")
	}
	until, err := m.Guard(true, addrs, time.Minute)
	if err != nil || !until.Equal(now.Add(time.Minute)) || !strings.Contains(f.last(), `comment "janus:refuse"`) {
		t.Fatalf("refuse: %v %v\n%s", until, err, f.last())
	}
	if _, err := m.Guard(false, addrs, 2*time.Minute); err != nil || !strings.Contains(f.last(), `comment "janus:hold"`) {
		t.Fatalf("hold: %v\n%s", err, f.last())
	}
	if _, err := m.Start(context.Background(), "lb-2", addrs, time.Hour); err != nil {
		t.Fatal(err)
	}
	refuse, hold := m.GuardsUntil()
	if !refuse.Equal(now.Add(time.Minute)) || !hold.Equal(now.Add(2*time.Minute)) {
		t.Errorf("until: %v %v", refuse, hold)
	}
	// The refusal over first, then the hold; the handoff goes on.
	now = now.Add(61 * time.Second)
	m.tick()
	if d := f.last(); strings.Contains(d, "janus:refuse") || !strings.Contains(d, "janus:hold") || !strings.Contains(d, `comment "lb-2"`) {
		t.Errorf("after a minute:\n%s", d)
	}
	now = now.Add(time.Minute)
	if !m.tick() {
		t.Error("the loop would end with a handoff left")
	}
	if d := f.last(); strings.Contains(d, "janus:hold") || !strings.Contains(d, `comment "lb-2"`) {
		t.Errorf("after two minutes:\n%s", d)
	}
	// Ended at once.
	if _, err := m.Guard(true, addrs, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.EndGuards(); err != nil || strings.Contains(f.last(), "janus:refuse") {
		t.Errorf("ended: %v\n%s", err, f.last())
	}
	if r, h := m.GuardsUntil(); !r.IsZero() || !h.IsZero() {
		t.Errorf("guards left: %v %v", r, h)
	}
}
