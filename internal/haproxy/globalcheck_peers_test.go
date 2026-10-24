package haproxy

import (
	"strings"
	"testing"
)

// The peers section janusd generates (internal/cluster) is the node's:
// a configuration neither defines it, nor names HAProxy's local peer,
// nor binds the port its listener takes.
func TestPolicyPeers(t *testing.T) {
	p := NodePolicy("/var/empty", "/run/janus/haproxy-admin.sock")
	p.PeersSection = "janus"
	p.PeersPort = func() int { return 10057 }
	global := "global\n    chroot /var/empty\n    uid 1000\n    gid 1000\n    stats socket /run/janus/haproxy-admin.sock mode 660 level admin\n"
	for _, tc := range []struct{ name, cfg, want string }{
		{"a table on the generated section", global + "frontend fe\n    bind :443\n    stick-table type ip size 10k peers janus\n", ""},
		{"another peers section", global + "peers mine\n    bind :10058\n    server a\n", ""},
		{"the generated section", global + "peers janus\n    server a\n", "janusd's"},
		{"localpeer", global + "    localpeer lb-1\n", "localpeer"},
		{"the peers port", global + "frontend fe\n    bind 10.0.20.11:10057\n", "peers listener"},
		{"the peers port among others", global + "frontend fe\n    bind :443,:10057 ssl\n", "peers listener"},
		{"a range over the peers port", global + "listen l\n    bind 0.0.0.0:10050-10060\n", "peers listener"},
		{"a range beside it", global + "listen l\n    bind 0.0.0.0:10050-10056\n", ""},
		{"another port", global + "frontend fe\n    bind [::]:10056\n", ""},
		{"a unix socket", global + "frontend fe\n    bind /run/x.sock\n", ""},
	} {
		err := p.Check([]byte(tc.cfg))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
	// Without a port to keep: any bind line passes.
	p.PeersPort = nil
	if err := p.Check([]byte(global + "frontend fe\n    bind :10057\n")); err != nil {
		t.Errorf("no port kept: %v", err)
	}
}
