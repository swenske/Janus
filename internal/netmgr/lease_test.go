package netmgr

import (
	"reflect"
	"testing"
)

func TestParsePnp(t *testing.T) {
	// /proc/net/pnp as the kernel writes it after ip=dhcp (QEMU's slirp).
	info := parsePnp("#PROTO: DHCP\ndomain example.lan\nnameserver 10.0.2.3\nnameserver 0.0.0.0\nbootserver 10.0.2.2\n")
	if !info.dhcp || info.domain != "example.lan" || info.server != "10.0.2.2" || !reflect.DeepEqual(info.nameservers, []string{"10.0.2.3"}) {
		t.Errorf("parsePnp = %+v", info)
	}
	if parsePnp("#MANUAL\n").dhcp {
		t.Error("a manual configuration isn't a DHCP lease")
	}
}
