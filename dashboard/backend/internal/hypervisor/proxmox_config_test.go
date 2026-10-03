package hypervisor

import "testing"

func TestProxmoxNetworks(t *testing.T) {
	c := &ProxmoxConfig{Networks: []string{"vmbr0", "vmbr0.10", "vmbr1.100-109", "vmbr2.*"}}
	for network, want := range map[string]bool{
		"vmbr0":         true, // untagged, named
		"vmbr0.10":      true,
		"vmbr0.11":      false,
		"vmbr1":         false, // a range doesn't give the untagged bridge
		"vmbr1.100":     true,
		"vmbr1.109":     true,
		"vmbr1.110":     false,
		"vmbr2.1":       true,
		"vmbr2.4094":    true,
		"vmbr2":         false, // * is every VLAN, not untagged traffic
		"vmbr3":         false,
		"vmbr1.100-109": false, // an interface names one VLAN
		"vmbr2.*":       false,
	} {
		if got := c.AllowsNetwork(network); got != want {
			t.Errorf("AllowsNetwork(%q) = %v, want %v", network, got, want)
		}
	}
	for _, bad := range []string{"vmbr0.0", "vmbr0.4095", "vmbr0.20-10", "vmbr0.1-300", "vmbr0.10-*", "vmbr0.*-5", "0vmbr", "vmbr0.10.20"} {
		if _, _, _, err := ParseProxmoxAllowed(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if b, f, l, err := ParseProxmoxAllowed("vmbr0.1-256"); err != nil || b != "vmbr0" || f != 1 || l != 256 {
		t.Errorf("vmbr0.1-256: %s %d %d %v", b, f, l, err)
	}
}
