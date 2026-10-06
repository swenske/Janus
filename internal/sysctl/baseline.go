package sysctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Root is /proc/sys; Dir is where the saved values and the history live
// (STATE's config/, which janusd's -config-dir moves); RunDir holds this
// boot's record of the kernel's own values. Vars, so tests can move them.
var (
	Root   = "/proc/sys"
	Dir    = "/etc/janus/config"
	RunDir = "/run/janus"
)

// Setting is a key and the value written to it.
type Setting struct{ Key, Value string }

// janusSettings are the baseline's keys that are neither CIS nor tunable.
var janusSettings = []Setting{
	// When an interface's primary IPv4 address is removed (janusd moving
	// it to another one in the same subnet), promote a secondary rather
	// than delete them all with it - the kernel's default, which a real
	// reconfiguration hit (internal/netmgr).
	{"net.ipv4.conf.all.promote_secondaries", "1"},
	{"net.ipv4.conf.default.promote_secondaries", "1"},
}

// Baseline is what every boot writes, in this order: the CIS controls
// (CISControls' order), Janus's own settings, then the Editable
// parameters' defaults - not the Dynamic ones, which stay the kernel's.
func Baseline() []Setting {
	var out []Setting
	for _, c := range CISControls {
		out = append(out, Setting{c.Key, c.Value})
	}
	out = append(out, janusSettings...)
	for _, p := range EditableParams() {
		if !p.Dynamic {
			out = append(out, Setting{p.Name, p.Default})
		}
	}
	return out
}

// Logf reports what a boot step did - init's console.
type Logf func(format string, args ...any)

// write puts value into path, newline-terminated as echo would.
func write(path, value string) error {
	return os.WriteFile(path, []byte(value+"\n"), 0o644)
}

// ApplyBaseline writes Baseline, then the per-interface values of the
// CIS controls that need them. Each write is logged - "sysctl
// <path>=<value>", with the error when it failed - and none stops the
// others: a kernel without some feature has no file for it.
func ApplyBaseline(logf Logf) {
	for _, s := range Baseline() {
		logWrite(logf, keyPath(Root, s.Key), s.Value, false)
	}
	writeInterfaces(logf, false)
}

// writeInterfaces sets the per-interface CIS values on every interface.
func writeInterfaces(logf Logf, onlyFailures bool) {
	for _, c := range CISControls {
		ifaces, err := interfaceDirs(Root, c.Interfaces)
		if err != nil {
			logf("sysctl %s on each interface: %v", c.Key, err)
			continue
		}
		for _, iface := range ifaces {
			logWrite(logf, filepath.Join(Root, "net", c.Interfaces, "conf", iface, leaf(c.Key)), c.Value, onlyFailures)
		}
	}
}

// logWrite writes value into path and logs it - only a failure with
// onlyFailures.
func logWrite(logf Logf, path, value string, onlyFailures bool) {
	err := write(path, value)
	switch {
	case err != nil:
		logf("sysctl %s=%s: %v", path, value, err)
	case !onlyFailures:
		logf("sysctl %s=%s", path, value)
	}
}

// EnforceCIS writes every CIS control again, interfaces included, and
// audits them: the last word of a boot, after the saved values, goes to
// the benchmark. Only failures are logged write by write.
func EnforceCIS(logf Logf) Audit {
	for _, c := range CISControls {
		logWrite(logf, keyPath(Root, c.Key), c.Value, true)
	}
	writeInterfaces(logf, true)
	a := AuditCIS(Root)
	if a.OK() {
		logf("sysctl: %s (%s): %d/%d controls compliant", Benchmark, Profile, a.Compliant(), len(a.Results))
	} else {
		for _, f := range a.Failures() {
			logf("sysctl: NOT COMPLIANT: %s", f)
		}
	}
	return a
}

const bootFile = "sysctl-boot.json"

// CaptureBootDefaults records the Dynamic parameters' values before
// anything changes them: the kernel computed them from this machine's
// memory, and they are Janus's defaults for this boot.
func CaptureBootDefaults() error {
	values := map[string]string{}
	for _, p := range EditableParams() {
		if !p.Dynamic {
			continue
		}
		if v, err := readValue(p.Path(Root)); err == nil {
			values[p.Name] = Normalize(p.Kind, v)
		}
	}
	data, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(RunDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(RunDir, bootFile), append(data, '\n'), 0o644)
}

// bootDefaults reads CaptureBootDefaults' record. Without one - janusd
// on a machine init didn't boot - the live values stand in.
func bootDefaults() map[string]string {
	values := map[string]string{}
	if data, err := os.ReadFile(filepath.Join(RunDir, bootFile)); err == nil {
		_ = json.Unmarshal(data, &values)
	}
	for _, p := range EditableParams() {
		if !p.Dynamic || values[p.Name] != "" {
			continue
		}
		if v, err := readValue(p.Path(Root)); err == nil {
			values[p.Name] = Normalize(p.Kind, v)
		}
	}
	return values
}

// Defaults is every Editable parameter's default on this node.
func Defaults() map[string]string {
	boot := bootDefaults()
	out := map[string]string{}
	for _, p := range EditableParams() {
		if p.Dynamic {
			out[p.Name] = boot[p.Name]
		} else {
			out[p.Name] = p.Default
		}
	}
	return out
}

// Refused is a saved line the boot didn't apply.
type Refused struct {
	Line   int
	Name   string
	Value  string
	Reason string
}

// ApplySaved writes the saved values at boot, each checked like a change
// made through the API: a key outside the whitelist, or a value out of
// its bounds, is skipped - logged, and recorded in the history. The
// checks that look at the running node (listening ports...) have nothing
// to look at yet, and were made when the value was confirmed.
func ApplySaved(logf Logf) []Refused {
	lines, malformed, err := LoadSaved()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logf("sysctl: %s: %v - the defaults stay", savedName, err)
		}
		return nil
	}
	var refused []Refused
	for _, m := range malformed {
		refused = append(refused, m)
		logf("sysctl: %s line %d: %s", savedName, m.Line, m.Reason)
	}
	target := Defaults()
	for _, l := range lines {
		target[l.Name] = l.Value
	}
	for _, l := range lines {
		p, reason := editable(l.Name)
		var value string
		if p != nil {
			var nums []int64
			value, nums, err = p.Parse(l.Value)
			if err == nil && p.check != nil {
				err = p.check(nums, &env{target: target})
			}
			if err != nil {
				reason = err.Error()
			}
		}
		if reason != "" {
			refused = append(refused, Refused{Line: l.Line, Name: l.Name, Value: l.Value, Reason: reason})
			logf("sysctl: %s line %d: %s: %s - refused", savedName, l.Line, l.Name, reason)
			continue
		}
		logWrite(logf, p.Path(Root), value, false)
	}
	if len(refused) > 0 {
		e := Entry{Actor: Actor{Name: "boot"}, Action: ActionBootRefused}
		for _, r := range refused {
			e.Changes = append(e.Changes, ChangeRecord{Name: r.Name, New: r.Value})
			e.Detail += fmt.Sprintf("line %d: %s. ", r.Line, r.Reason)
		}
		if err := appendHistory(e); err != nil {
			logf("sysctl: history: %v", err)
		}
	}
	return refused
}

// editable is name's parameter if it may be changed, else why not.
func editable(name string) (*Param, string) {
	if c, ok := cisByKey(name); ok {
		return nil, fmt.Sprintf("locked: %s control %s covers it", Benchmark, c.ID())
	}
	p := Lookup(name)
	switch {
	case p == nil:
		return nil, "not a parameter Janus lets anyone change - see SysctlList for the ones it does"
	case p.Class == ReadOnly:
		return nil, "read-only: " + p.Why
	case p.Class == Forbidden:
		return nil, "forbidden: " + p.Why
	}
	return p, ""
}
