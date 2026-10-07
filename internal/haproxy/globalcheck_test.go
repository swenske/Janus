package haproxy

import (
	"os"
	"strings"
	"testing"
)

func nodeTestPolicy() *GlobalPolicy {
	return NodePolicy("/var/empty", "/run/janus/haproxy-admin.sock")
}

// TestGlobalPolicyAcceptsTheNodeConfigs: the bootstrap configuration and
// the documented example pass as they are.
func TestGlobalPolicyAcceptsTheNodeConfigs(t *testing.T) {
	for _, f := range []string{"../../rootfs/base/etc/haproxy/haproxy.cfg", "../../examples/haproxy/web.cfg"} {
		cfg, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := nodeTestPolicy().Check(cfg); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// TestGlobalPolicy: what it accepts (indentation, comments, order,
// extra tuning, no global at all on a host) and what it refuses, each
// with a message naming the line.
func TestGlobalPolicy(t *testing.T) {
	const good = `global
  log stdout format raw local0
  chroot /var/empty
  stats socket /run/janus/haproxy-admin.sock mode 660 level admin
  stats timeout 30s
  uid 1000
  gid 1000
  ulimit-n 524288
  tune.bufsize 32768

defaults
    mode http
`
	tests := []struct {
		name, cfg, want string
	}{
		{"a production configuration", good, ""},
		{"the parameters in any order, tabs and comments",
			"global # the node's\n\tstats socket /run/janus/haproxy-admin.sock level admin mode 660 # janusd's\n\tchroot   /var/empty\n\tuid 1000\n\tgid 1000\n", ""},
		{"no indentation at all", "global\nchroot /var/empty\nuid 1000\ngid 1000\nstats socket /run/janus/haproxy-admin.sock mode 660 level admin\nfrontend f\nbind *:80\n", ""},
		{"no global section", "defaults\n  mode http\n", `"chroot /var/empty" is required`},
		{"no uid", strings.Replace(good, "  uid 1000\n", "", 1), `"uid 1000" is required`},
		{"another uid", strings.Replace(good, "uid 1000", "uid 0", 1), `"uid 0" must be "uid 1000"`},
		{"no gid", strings.Replace(good, "  gid 1000\n", "", 1), `"gid 1000" is required`},
		{"no chroot", strings.Replace(good, "  chroot /var/empty\n", "", 1), `"chroot /var/empty" is required`},
		{"another chroot", strings.Replace(good, "chroot /var/empty", "chroot /tmp", 1), `"chroot /tmp" must be "chroot /var/empty"`},
		{"no stats socket", strings.Replace(good, "  stats socket /run/janus/haproxy-admin.sock mode 660 level admin\n", "", 1), `"stats socket /run/janus/haproxy-admin.sock mode 660 level admin" is required`},
		{"a second stats socket on the network", strings.Replace(good, "  stats timeout 30s\n", "  stats socket ipv4@0.0.0.0:9999 level admin\n", 1), `only janusd's socket`},
		{"janusd's socket at another level", strings.Replace(good, "mode 660 level admin", "mode 666 level admin", 1), `it must be "stats socket /run/janus/haproxy-admin.sock mode 660 level admin"`},
		{"janusd's socket with more", strings.Replace(good, "level admin", "level admin expose-fd listeners", 1), `it must be`},
		{"daemon", strings.Replace(good, "  uid 1000\n", "  uid 1000\n  daemon\n", 1), `"daemon" isn't allowed`},
		{"master-worker", strings.Replace(good, "  uid 1000\n", "  uid 1000\n  master-worker\n", 1), `"master-worker" isn't allowed`},
		{"external-check", strings.Replace(good, "  uid 1000\n", "  uid 1000\n  external-check\n", 1), `"external-check" isn't allowed`},
		{"insecure-fork-wanted", strings.Replace(good, "  uid 1000\n", "  uid 1000\n  insecure-fork-wanted\n", 1), `"insecure-fork-wanted" isn't allowed`},
		{"set-dumpable", strings.Replace(good, "  uid 1000\n", "  uid 1000\n  set-dumpable\n", 1), `"set-dumpable" isn't allowed`},
		{"setenv", strings.Replace(good, "  uid 1000\n", "  uid 1000\n  setenv JANUS_ACME_THUMBPRINT x\n", 1), `"setenv" isn't allowed`},
		{"a program section", good + "\nprogram cmd\n  command /bin/true\n", `program section isn't allowed`},
		{"a forbidden keyword in a comment", strings.Replace(good, "  uid 1000\n", "  uid 1000\n  # daemon\n", 1), ""},
		{"the keywords outside global", good + "frontend f\n  bind *:80\n  # a frontend may say what it likes\n  daemon\n", ""},
	}
	for _, tc := range tests {
		err := nodeTestPolicy().Check([]byte(tc.cfg))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && err == nil:
			t.Errorf("%s: accepted", tc.name)
		case tc.want != "" && !strings.Contains(err.Error(), tc.want):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	var none *GlobalPolicy
	if err := none.Check([]byte("defaults\n")); err != nil {
		t.Errorf("no policy: %v", err)
	}
}
