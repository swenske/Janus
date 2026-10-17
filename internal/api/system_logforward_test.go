package api

import (
	"errors"
	"net/netip"
	"testing"
)

// A target's interface is the route to its connection's peer, from its
// local address; none before a connection, or when no route says.
func TestTargetInterface(t *testing.T) {
	defer func(f func(dst, src netip.Addr) (string, error)) { egressInterface = f }(egressInterface)
	var asked [2]netip.Addr
	egressInterface = func(dst, src netip.Addr) (string, error) {
		asked = [2]netip.Addr{dst, src}
		if dst == netip.MustParseAddr("192.0.2.1") {
			return "", errors.New("network is unreachable")
		}
		return "eth1", nil
	}
	if got := targetInterface("10.0.31.21:40312", "10.0.31.20:514"); got != "eth1" || asked != [2]netip.Addr{netip.MustParseAddr("10.0.31.20"), netip.MustParseAddr("10.0.31.21")} {
		t.Errorf("got %q, asked %v", got, asked)
	}
	if got := targetInterface("[fd00::5]:40312", "[fd00::1]:9200"); got != "eth1" || asked[0] != netip.MustParseAddr("fd00::1") {
		t.Errorf("IPv6: got %q, asked %v", got, asked)
	}
	if got := targetInterface("", "192.0.2.1:514"); got != "" {
		t.Errorf("no route: %q", got)
	}
	asked = [2]netip.Addr{}
	if got := targetInterface("", ""); got != "" || asked[0].IsValid() {
		t.Errorf("never connected: %q, asked %v", got, asked)
	}
}
