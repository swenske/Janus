package main

import (
	"net/netip"
	"testing"

	"github.com/swenske/Janus/internal/consul"
)

// An exporter is registered where it really listens: auto on the main
// IPv4 address, an IP on it, every address on the node's.
func TestEndpointOf(t *testing.T) {
	main := mainIPv4([]netip.Addr{netip.MustParseAddr("2001:db8::5"), netip.MustParseAddr("192.0.2.5")})
	if main != "192.0.2.5" {
		t.Fatalf("main IPv4 %q", main)
	}
	for _, c := range []struct {
		address string
		port    uint32
		want    consul.Endpoint
	}{
		{"auto", 0, consul.Endpoint{Host: "192.0.2.5", Port: 10056}},
		{"10.1.0.7", 9200, consul.Endpoint{Host: "10.1.0.7", Port: 9200}},
		{"", 10056, consul.Endpoint{Host: "", Port: 10056}},
	} {
		if got := endpointOf(c.address, c.port, 10056, main); *got != c.want {
			t.Errorf("endpointOf(%q, %d) = %+v, want %+v", c.address, c.port, *got, c.want)
		}
	}
	if got := endpointOf("auto", 9100, 9100, ""); got.Host != "" {
		t.Errorf("auto without a main address: %+v (the node's)", got)
	}
}
