package netmgr

import (
	"net/netip"
	"testing"
)

// The loopback is on every machine, and asking the kernel a route needs
// no privilege.
func TestEgressInterface(t *testing.T) {
	lo := netip.MustParseAddr("127.0.0.1")
	for _, c := range []struct {
		name     string
		dst, src netip.Addr
	}{
		{"no source", lo, netip.Addr{}},
		{"the connection's source", lo, lo},
		{"a source gone since", lo, netip.MustParseAddr("192.0.2.77")},
		{"IPv4 in IPv6", netip.MustParseAddr("::ffff:127.0.0.1"), netip.Addr{}},
	} {
		if got, err := EgressInterface(c.dst, c.src); err != nil || got != "lo" {
			t.Errorf("%s: %q, %v - want lo", c.name, got, err)
		}
	}
	if _, err := EgressInterface(netip.Addr{}, lo); err == nil {
		t.Error("no destination: no error")
	}
}
