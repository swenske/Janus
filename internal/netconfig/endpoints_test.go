package netconfig

import (
	"slices"
	"testing"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

func TestConfirmEndpoints(t *testing.T) {
	static := func(addrs ...string) *janusv1alpha1.NetworkConfig {
		return &janusv1alpha1.NetworkConfig{Interfaces: []*janusv1alpha1.NetworkInterface{{
			Name: "lan", Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: addrs,
		}}}
	}
	for _, tc := range []struct {
		name     string
		endpoint string
		cfg      *janusv1alpha1.NetworkConfig
		reported []string
		lost     bool
		want     []string
	}{
		{"kept, reported", "192.0.2.10:9505", static("192.0.2.10/24"), []string{"192.0.2.10/24"}, false,
			[]string{"192.0.2.10:9505"}},
		{"moved, the report arrived", "192.0.2.10:9505", static("192.0.2.20/24"), []string{"192.0.2.20/24"}, false,
			[]string{"192.0.2.10:9505", "192.0.2.20:9505"}},
		// A cluster member's move: the stream lost with the old address,
		// the new one first - the old one cost 4 s each round.
		{"moved, lost: the old address last", "192.0.2.10:9505", static("192.0.2.20/24", "2001:db8::20/64"), nil, true,
			[]string{"192.0.2.20:9505", "[2001:db8::20]:9505", "192.0.2.10:9505"}},
		// Lost although the configuration keeps it (a slow apply): it
		// stays first - the node's record must not move to another of
		// its addresses.
		{"lost, kept statically", "192.0.2.10:9505", static("192.0.2.20/24", "192.0.2.10/24"), nil, true,
			[]string{"192.0.2.10:9505", "192.0.2.20:9505"}},
		{"lost, nothing else to try", "192.0.2.10:9505", &janusv1alpha1.NetworkConfig{}, nil, true,
			[]string{"192.0.2.10:9505"}},
		{"no port", "192.0.2.10", static("192.0.2.20/24"), nil, true,
			[]string{"192.0.2.20:9505", "192.0.2.10"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConfirmEndpoints(tc.endpoint, tc.cfg, tc.reported, tc.lost); !slices.Equal(got, tc.want) {
				t.Errorf("ConfirmEndpoints = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestKeepsStatically(t *testing.T) {
	cfg := &janusv1alpha1.NetworkConfig{Interfaces: []*janusv1alpha1.NetworkInterface{
		{Name: "lan", Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP},
		{Name: "front", Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: []string{"192.0.2.20/24", "2001:db8::20/64"}},
	}}
	for ep, want := range map[string]bool{
		"192.0.2.20:9505":     true,
		"[2001:db8::20]:9505": true,
		"192.0.2.20":          true,
		"192.0.2.10:9505":     false,
		"node.example:9505":   false,
	} {
		if got := KeepsStatically(cfg, ep); got != want {
			t.Errorf("KeepsStatically(%s) = %v, want %v", ep, got, want)
		}
	}
}

func TestDropsEndpoint(t *testing.T) {
	static := func(addrs ...string) *janusv1alpha1.NetworkInterface {
		return &janusv1alpha1.NetworkInterface{Name: "lan", Mode: janusv1alpha1.AddressingMode_ADDRESSING_MODE_STATIC, Addresses: addrs}
	}
	cfg := func(ifaces ...*janusv1alpha1.NetworkInterface) *janusv1alpha1.NetworkConfig {
		return &janusv1alpha1.NetworkConfig{Interfaces: ifaces}
	}
	for _, tc := range []struct {
		name     string
		cfg      *janusv1alpha1.NetworkConfig
		endpoint string
		want     bool
	}{
		{"a member's move", cfg(static("192.0.2.33/24")), "192.0.2.34:9505", true},
		{"kept", cfg(static("192.0.2.34/24", "192.0.2.33/24")), "192.0.2.34:9505", false},
		{"another interface by DHCP may keep it", cfg(static("192.0.2.33/24"), &janusv1alpha1.NetworkInterface{Name: "mgmt"}), "192.0.2.34:9505", false},
		{"no interface: the default", cfg(), "192.0.2.34:9505", false},
		{"a port forward", cfg(static("192.0.2.33/24")), "node.example:9505", false},
	} {
		if got := DropsEndpoint(tc.cfg, tc.endpoint); got != tc.want {
			t.Errorf("%s: DropsEndpoint = %v, want %v", tc.name, got, tc.want)
		}
	}
}
