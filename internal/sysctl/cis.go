package sysctl

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// The CIS benchmark Janus's baseline follows, restricted to the controls
// on kernel parameters (sections 1.5 and 3.3). Debian 13's is the newest
// CIS Linux benchmark; these controls only check /proc/sys values, so
// they hold for any distribution. The values below are transcribed from
// ComplianceAsCode's controls/cis_debian13.yml (version 1.0.0).
const (
	Benchmark = "CIS Debian Linux 13 Benchmark v1.0.0"
	Profile   = "Level 2 - Server"
)

// CISControl is one control of the benchmark: a key the node sets and
// never lets anyone change.
type CISControl struct {
	IDs   []string // 1.5.3 and 1.5.10 check the same key
	Level int      // the benchmark's server profile: 1 or 2
	Key   string
	Value string   // what Janus writes
	Want  []string // the compliant values
	// Interfaces: the key's leaf must also hold on every interface
	// ("ipv4", "ipv6") - the kernel uses each interface's own value, and
	// IPv6 doesn't copy all/default onto the interfaces that already
	// exist (the boot DHCP's comes up before init runs).
	Interfaces string
}

// ID is the control's identifier as the benchmark gives it.
func (c CISControl) ID() string { return strings.Join(c.IDs, ", ") }

func cis(level int, key, value string, ids ...string) CISControl {
	return CISControl{IDs: ids, Level: level, Key: key, Value: value, Want: []string{value}}
}

// CISControls, in the order they are written: forwarding before the
// redirect keys, since turning forwarding off resets
// conf.all.accept_redirects to 1 (inet_forward_change).
var CISControls = []CISControl{
	cis(1, "fs.protected_hardlinks", "1", "1.5.1"),
	cis(2, "fs.protected_symlinks", "1", "1.5.2"),
	{IDs: []string{"1.5.3", "1.5.10"}, Level: 1, Key: "kernel.yama.ptrace_scope", Value: "2", Want: []string{"1", "2", "3"}},
	cis(1, "fs.suid_dumpable", "0", "1.5.4"),
	cis(1, "kernel.dmesg_restrict", "1", "1.5.5"),
	{IDs: []string{"1.5.8"}, Level: 1, Key: "kernel.kptr_restrict", Value: "2", Want: []string{"1", "2"}},
	cis(1, "kernel.randomize_va_space", "2", "1.5.9"),

	cis(2, "net.ipv4.ip_forward", "0", "3.3.1.1"),
	cis(1, "net.ipv4.conf.all.forwarding", "0", "3.3.1.2"),
	cis(1, "net.ipv4.conf.default.forwarding", "0", "3.3.1.3"),
	withInterfaces("ipv4", cis(1, "net.ipv4.conf.all.send_redirects", "0", "3.3.1.4")),
	cis(1, "net.ipv4.conf.default.send_redirects", "0", "3.3.1.5"),
	cis(1, "net.ipv4.icmp_ignore_bogus_error_responses", "1", "3.3.1.6"),
	cis(1, "net.ipv4.icmp_echo_ignore_broadcasts", "1", "3.3.1.7"),
	withInterfaces("ipv4", cis(1, "net.ipv4.conf.all.accept_redirects", "0", "3.3.1.8")),
	cis(1, "net.ipv4.conf.default.accept_redirects", "0", "3.3.1.9"),
	withInterfaces("ipv4", cis(1, "net.ipv4.conf.all.secure_redirects", "0", "3.3.1.10")),
	cis(1, "net.ipv4.conf.default.secure_redirects", "0", "3.3.1.11"),
	cis(1, "net.ipv4.conf.all.rp_filter", "1", "3.3.1.12"),
	cis(1, "net.ipv4.conf.default.rp_filter", "1", "3.3.1.13"),
	cis(1, "net.ipv4.conf.all.accept_source_route", "0", "3.3.1.14"),
	cis(1, "net.ipv4.conf.default.accept_source_route", "0", "3.3.1.15"),
	cis(1, "net.ipv4.conf.all.log_martians", "1", "3.3.1.16"),
	cis(1, "net.ipv4.conf.default.log_martians", "1", "3.3.1.17"),
	cis(1, "net.ipv4.tcp_syncookies", "1", "3.3.1.18"),

	cis(1, "net.ipv6.conf.all.forwarding", "0", "3.3.2.1"),
	cis(1, "net.ipv6.conf.default.forwarding", "0", "3.3.2.2"),
	withInterfaces("ipv6", cis(1, "net.ipv6.conf.all.accept_redirects", "0", "3.3.2.3")),
	cis(1, "net.ipv6.conf.default.accept_redirects", "0", "3.3.2.4"),
	cis(1, "net.ipv6.conf.all.accept_source_route", "0", "3.3.2.5"),
	cis(1, "net.ipv6.conf.default.accept_source_route", "0", "3.3.2.6"),
	withInterfaces("ipv6", cis(1, "net.ipv6.conf.all.accept_ra", "0", "3.3.2.7")),
	cis(1, "net.ipv6.conf.default.accept_ra", "0", "3.3.2.8"),
}

func withInterfaces(family string, c CISControl) CISControl {
	c.Interfaces = family
	return c
}

// cisByKey finds the control covering key.
func cisByKey(key string) (CISControl, bool) {
	for _, c := range CISControls {
		if c.Key == key {
			return c, true
		}
	}
	return CISControl{}, false
}

// ControlResult is one control as found on the node.
type ControlResult struct {
	Control   CISControl
	Value     string // "" when the key is missing
	Compliant bool
	Problems  []string // what isn't compliant: the key, or an interface
}

// Audit is the benchmark's state on the node.
type Audit struct {
	Results []ControlResult
}

// Compliant counts the compliant controls.
func (a Audit) Compliant() int {
	n := 0
	for _, r := range a.Results {
		if r.Compliant {
			n++
		}
	}
	return n
}

// OK reports whether every control is compliant.
func (a Audit) OK() bool { return a.Compliant() == len(a.Results) }

// Failures describes the non-compliant controls, for logs and errors.
func (a Audit) Failures() []string {
	var out []string
	for _, r := range a.Results {
		if !r.Compliant {
			out = append(out, fmt.Sprintf("CIS %s %s: %s", r.Control.ID(), r.Control.Key, strings.Join(r.Problems, "; ")))
		}
	}
	return out
}

// AuditCIS reads every control's key under root (/proc/sys) - and, for
// the ones that apply per interface, every interface's value.
func AuditCIS(root string) Audit {
	var a Audit
	for _, c := range CISControls {
		r := ControlResult{Control: c, Compliant: true}
		v, err := readValue(keyPath(root, c.Key))
		switch {
		case err != nil:
			r.Compliant = false
			r.Problems = append(r.Problems, missingReason(err))
		case !slices.Contains(c.Want, v):
			r.Compliant = false
			r.Value = v
			r.Problems = append(r.Problems, fmt.Sprintf("%s, want %s", v, strings.Join(c.Want, " or ")))
		default:
			r.Value = v
		}
		ifaces, err := interfaceDirs(root, c.Interfaces)
		if err != nil {
			// The interfaces can't be checked: not compliant, rather
			// than compliant by default.
			r.Compliant = false
			r.Problems = append(r.Problems, "the interfaces can't be listed: "+err.Error())
		}
		for _, iface := range ifaces {
			path := filepath.Join(root, "net", c.Interfaces, "conf", iface, leaf(c.Key))
			iv, err := readValue(path)
			if err != nil {
				continue // the interface went away
			}
			if !slices.Contains(c.Want, iv) {
				r.Compliant = false
				r.Problems = append(r.Problems, fmt.Sprintf("%s: %s, want %s", iface, iv, strings.Join(c.Want, " or ")))
			}
		}
		a.Results = append(a.Results, r)
	}
	return a
}

func missingReason(err error) string {
	if os.IsNotExist(err) {
		return "absent from this kernel"
	}
	return err.Error()
}

// interfaceDirs lists the interfaces under net/<family>/conf, all and
// default excepted; nothing for family "", nor for a family the kernel
// hasn't.
func interfaceDirs(root, family string) ([]string, error) {
	if family == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(filepath.Join(root, "net", family, "conf"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "all" && e.Name() != "default" {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// leaf is a key's last component: "accept_ra" of "net.ipv6.conf.all.accept_ra".
func leaf(key string) string {
	return key[strings.LastIndex(key, ".")+1:]
}

// keyPath is key's file under root. Only for keys of this package's own
// tables: an interface name may hold a dot, which these never do.
func keyPath(root, key string) string {
	return root + "/" + strings.ReplaceAll(key, ".", "/")
}
