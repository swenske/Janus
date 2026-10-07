package haproxy

import (
	"fmt"
	"strings"
)

// GlobalPolicy is what a node's haproxy.cfg must say, and may not say,
// in its global section - the guarantees docs/haproxy-config.md
// documents as the node's: HAProxy drops its privileges into an empty
// chroot, janusd's runtime API is the only administration socket, and
// HAProxy neither daemonizes away from janusd's supervision nor runs
// programs of its own. `haproxy -c` accepts a configuration without
// any of that; the node refuses it before writing it. Nil (janusd on a
// host, in CI) enforces nothing.
type GlobalPolicy struct {
	// Chroot is the directory a "chroot" line must name.
	Chroot string
	// UID and GID are what the "uid" and "gid" lines must say.
	UID, GID string
	// StatsSocket is the path of the only "stats socket" allowed, and
	// required: janusd's, "mode 660 level admin".
	StatsSocket string
}

// NodePolicy is the policy of a node whose janusd prepares chrootDir and
// talks to HAProxy over statsSocket: rootfs/base/etc/haproxy/haproxy.cfg's
// global section, what every node boots with.
func NodePolicy(chrootDir, statsSocket string) *GlobalPolicy {
	return &GlobalPolicy{Chroot: chrootDir, UID: "1000", GID: "1000", StatsSocket: statsSocket}
}

// sectionKeywords start a section: HAProxy pays no attention to
// indentation, so a global line is one between "global" and the next
// of these.
var sectionKeywords = map[string]bool{
	"global": true, "defaults": true, "frontend": true, "backend": true, "listen": true,
	"peers": true, "resolvers": true, "mailers": true, "userlist": true, "cache": true,
	"program": true, "http-errors": true, "ring": true, "log-forward": true,
	"crt-store": true, "fcgi-app": true, "traces": true, "acme": true,
}

// forbiddenGlobal are the global keywords a node refuses, each with why.
var forbiddenGlobal = map[string]string{
	"daemon":               "janusd supervises HAProxy in the foreground",
	"master-worker":        "janusd supervises HAProxy itself",
	"external-check":       "HAProxy runs no programs on a node",
	"insecure-fork-wanted": "HAProxy runs no programs on a node",
	"set-dumpable":         "HAProxy's memory holds keys",
	"setenv":               "HAProxy's environment is janusd's",
	"presetenv":            "HAProxy's environment is janusd's",
	"resetenv":             "HAProxy's environment is janusd's",
	"unsetenv":             "HAProxy's environment is janusd's",
}

// Check reports why cfg's global section isn't a node's - the first
// reason found, worded for the operator - or nil.
func (p *GlobalPolicy) Check(cfg []byte) error {
	if p == nil {
		return nil
	}
	wantChroot := "chroot " + p.Chroot
	wantUID, wantGID := "uid "+p.UID, "gid "+p.GID
	seen := map[string]bool{}
	inGlobal := false
	for _, raw := range strings.Split(string(cfg), "\n") {
		fields := strings.Fields(uncomment(raw))
		if len(fields) == 0 {
			continue
		}
		if sectionKeywords[fields[0]] {
			if fields[0] == "program" {
				return fmt.Errorf("a program section isn't allowed on a node: HAProxy runs no programs here")
			}
			inGlobal = fields[0] == "global"
			continue
		}
		if !inGlobal {
			continue
		}
		if why, ok := forbiddenGlobal[fields[0]]; ok {
			return fmt.Errorf("global: %q isn't allowed on a node: %s", fields[0], why)
		}
		line := strings.Join(fields, " ")
		switch {
		case fields[0] == "chroot":
			if line != wantChroot {
				return fmt.Errorf("global: %q must be %q on a node, the empty directory janusd prepares", line, wantChroot)
			}
			seen["chroot"] = true
		case fields[0] == "uid":
			if line != wantUID {
				return fmt.Errorf("global: %q must be %q on a node", line, wantUID)
			}
			seen["uid"] = true
		case fields[0] == "gid":
			if line != wantGID {
				return fmt.Errorf("global: %q must be %q on a node", line, wantGID)
			}
			seen["gid"] = true
		case len(fields) >= 2 && fields[0] == "stats" && fields[1] == "socket":
			if err := p.checkStatsSocket(fields[2:]); err != nil {
				return fmt.Errorf("global: %q: %v", line, err)
			}
			seen["stats socket"] = true
		}
	}
	for _, want := range []struct{ key, line, why string }{
		{"chroot", wantChroot, "HAProxy runs in the empty directory janusd prepares"},
		{"uid", wantUID, "HAProxy drops its privileges"},
		{"gid", wantGID, "HAProxy drops its privileges"},
		{"stats socket", "stats socket " + p.StatsSocket + " mode 660 level admin", "janusd's runtime API goes through it"},
	} {
		if !seen[want.key] {
			return fmt.Errorf("global: %q is required on a node: %s (docs/haproxy-config.md)", want.line, want.why)
		}
	}
	return nil
}

// checkStatsSocket checks what follows "stats socket": janusd's path,
// then "mode 660" and "level admin" in any order and nothing else - no
// second socket, no network address, no other level.
func (p *GlobalPolicy) checkStatsSocket(args []string) error {
	if len(args) == 0 || args[0] != p.StatsSocket {
		return fmt.Errorf("only janusd's socket, %s, is allowed on a node", p.StatsSocket)
	}
	got := map[string]string{}
	for i := 1; i+1 < len(args); i += 2 {
		got[args[i]] = args[i+1]
	}
	if len(args)%2 != 1 || len(got) != 2 || got["mode"] != "660" || got["level"] != "admin" {
		return fmt.Errorf("it must be %q", "stats socket "+p.StatsSocket+" mode 660 level admin")
	}
	return nil
}

// uncomment drops a line's comment: from the first "#" not escaped as
// "\#", the way HAProxy reads it.
func uncomment(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] == '#' && (i == 0 || line[i-1] != '\\') {
			return line[:i]
		}
	}
	return line
}
