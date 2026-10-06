package sysctl

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeProbes is the node's state as a test wants it.
type fakeProbes struct {
	listening []int64
	open      int64
	conntrack int64
	mem       int64
}

func (f fakeProbes) ListeningTCPPorts() ([]int64, error) { return f.listening, nil }
func (f fakeProbes) OpenFiles() (int64, error)           { return f.open, nil }
func (f fakeProbes) ConntrackEntries() (int64, error)    { return f.conntrack, nil }
func (f fakeProbes) MemTotal() (int64, error)            { return f.mem, nil }

var quietProbes = fakeProbes{open: 1000, mem: 16 << 30}

// kernelDefaults are the values a fresh kernel has, before Janus's
// baseline: the CIS keys Janus fixes are non-compliant here.
var kernelDefaults = map[string]string{
	"fs.protected_hardlinks":                     "0",
	"fs.protected_symlinks":                      "0",
	"kernel.yama.ptrace_scope":                   "1",
	"fs.suid_dumpable":                           "0",
	"kernel.dmesg_restrict":                      "0",
	"kernel.kptr_restrict":                       "0",
	"kernel.randomize_va_space":                  "2",
	"net.ipv4.ip_forward":                        "0",
	"net.ipv4.conf.all.forwarding":               "0",
	"net.ipv4.conf.default.forwarding":           "0",
	"net.ipv4.conf.all.send_redirects":           "1",
	"net.ipv4.conf.default.send_redirects":       "1",
	"net.ipv4.icmp_ignore_bogus_error_responses": "1",
	"net.ipv4.icmp_echo_ignore_broadcasts":       "1",
	"net.ipv4.conf.all.accept_redirects":         "1",
	"net.ipv4.conf.default.accept_redirects":     "1",
	"net.ipv4.conf.all.secure_redirects":         "1",
	"net.ipv4.conf.default.secure_redirects":     "1",
	"net.ipv4.conf.all.rp_filter":                "0",
	"net.ipv4.conf.default.rp_filter":            "0",
	"net.ipv4.conf.all.accept_source_route":      "0",
	"net.ipv4.conf.default.accept_source_route":  "1",
	"net.ipv4.conf.all.log_martians":             "0",
	"net.ipv4.conf.default.log_martians":         "0",
	"net.ipv4.tcp_syncookies":                    "1",
	"net.ipv6.conf.all.forwarding":               "0",
	"net.ipv6.conf.default.forwarding":           "0",
	"net.ipv6.conf.all.accept_redirects":         "1",
	"net.ipv6.conf.default.accept_redirects":     "1",
	"net.ipv6.conf.all.accept_source_route":      "0",
	"net.ipv6.conf.default.accept_source_route":  "0",
	"net.ipv6.conf.all.accept_ra":                "1",
	"net.ipv6.conf.default.accept_ra":            "1",
	"net.ipv4.conf.all.promote_secondaries":      "0",
	"net.ipv4.conf.default.promote_secondaries":  "0",

	"net.core.somaxconn":                        "4096",
	"net.ipv4.ip_local_port_range":              "32768\t60999",
	"net.ipv4.ip_local_reserved_ports":          "",
	"net.ipv4.tcp_tw_reuse":                     "2",
	"net.ipv4.tcp_fin_timeout":                  "60",
	"net.ipv4.tcp_synack_retries":               "5",
	"net.ipv4.ip_nonlocal_bind":                 "0",
	"net.ipv6.ip_nonlocal_bind":                 "0",
	"net.core.netdev_max_backlog":               "1000",
	"net.ipv4.tcp_rmem":                         "4096\t131072\t6291456",
	"net.ipv4.tcp_wmem":                         "4096\t16384\t4194304",
	"fs.file-max":                               "95000",
	"net.ipv4.tcp_keepalive_time":               "7200",
	"net.ipv4.tcp_keepalive_intvl":              "75",
	"net.ipv4.tcp_keepalive_probes":             "9",
	"net.ipv4.tcp_fastopen":                     "1",
	"net.netfilter.nf_conntrack_max":            "4096",
	"net.ipv4.tcp_max_syn_backlog":              "1024",
	"fs.nr_open":                                "1048576",
	"net.ipv4.tcp_congestion_control":           "cubic",
	"net.ipv4.tcp_available_congestion_control": "reno cubic",
	"kernel.core_pattern":                       "core",
}

// fakeNode points Root, Dir and RunDir at a fresh tree with a kernel's
// default values and two interfaces.
func fakeNode(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	oldRoot, oldDir, oldRun := Root, Dir, RunDir
	Root, Dir, RunDir = filepath.Join(base, "proc-sys"), filepath.Join(base, "config"), filepath.Join(base, "run")
	t.Cleanup(func() { Root, Dir, RunDir = oldRoot, oldDir, oldRun })
	for key, v := range kernelDefaults {
		mustWrite(t, keyPath(Root, key), v+"\n")
	}
	for _, iface := range []string{"eth0", "lo"} {
		for _, leaf := range []string{"send_redirects", "accept_redirects", "secure_redirects"} {
			mustWrite(t, filepath.Join(Root, "net/ipv4/conf", iface, leaf), "1\n")
		}
		for _, leaf := range []string{"accept_redirects", "accept_ra"} {
			mustWrite(t, filepath.Join(Root, "net/ipv6/conf", iface, leaf), "1\n")
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// booted is fakeNode after the boot steps init runs.
func booted(t *testing.T) {
	t.Helper()
	fakeNode(t)
	if err := CaptureBootDefaults(); err != nil {
		t.Fatal(err)
	}
	ApplyBaseline(func(string, ...any) {})
	ApplySaved(func(string, ...any) {})
	if a := EnforceCIS(func(string, ...any) {}); !a.OK() {
		t.Fatalf("not compliant after boot: %v", a.Failures())
	}
}

func liveValue(t *testing.T, name string) string {
	t.Helper()
	p := Lookup(name)
	v, err := readValue(p.Path(Root))
	if err != nil {
		t.Fatal(err)
	}
	return Normalize(p.Kind, v)
}

func TestCatalogInvariants(t *testing.T) {
	cisKeys := map[string]bool{}
	for _, c := range CISControls {
		if cisKeys[c.Key] {
			t.Errorf("CIS key %s twice", c.Key)
		}
		cisKeys[c.Key] = true
		if !slices.Contains(c.Want, c.Value) {
			t.Errorf("CIS %s: Janus writes %s, which isn't compliant", c.ID(), c.Value)
		}
	}
	if len(CISControls) != 33 {
		t.Errorf("%d CIS controls, want the benchmark's 33 kernel-parameter keys", len(CISControls))
	}
	seen := map[string]bool{}
	for _, p := range Catalog {
		if seen[p.Name] {
			t.Errorf("%s twice", p.Name)
		}
		seen[p.Name] = true
		if cisKeys[p.Name] {
			t.Errorf("%s is a CIS key: it can't be in the catalog", p.Name)
		}
		// net.ipv4.conf.* and net.ipv6.conf.* are the CIS keys' families
		// (and ip_forward is conf.all.forwarding): nothing editable lives
		// there, so no write can alias a CIS key.
		for _, prefix := range []string{"net.ipv4.conf.", "net.ipv6.conf.", "net.ipv4.ip_forward"} {
			if strings.HasPrefix(p.Name, prefix) {
				t.Errorf("%s is under %s, where the CIS keys are", p.Name, prefix)
			}
		}
		if p.Summary == "" || len(p.Sources) == 0 {
			t.Errorf("%s: no summary or no source", p.Name)
		}
		for _, s := range p.Sources {
			if !strings.HasPrefix(s.URL, "https://") || s.Title == "" {
				t.Errorf("%s: source %+v", p.Name, s)
			}
		}
		switch p.Class {
		case Editable:
			if p.Effect == "" || p.Risk == "" || p.Applies == 0 {
				t.Errorf("%s: an editable parameter needs its effect, risk and when it applies", p.Name)
			}
			switch p.Kind {
			case KindEnum:
				if len(p.Allowed) == 0 {
					t.Errorf("%s: an enum without values", p.Name)
				}
			case KindPorts:
				if len(p.Bounds) != 1 || p.MaxItems == 0 {
					t.Errorf("%s: ports need one bound and MaxItems", p.Name)
				}
			case KindInt, KindPair, KindTriple:
				if len(p.Bounds) != components(p.Kind) {
					t.Errorf("%s: %d bounds for %d components", p.Name, len(p.Bounds), components(p.Kind))
				}
			default:
				t.Errorf("%s: kind %d can't be edited", p.Name, p.Kind)
			}
			if p.Dynamic {
				if p.Default != "" {
					t.Errorf("%s: dynamic with a static default", p.Name)
				}
			} else if v, _, err := p.Parse(p.Default); err != nil || v != p.Default {
				t.Errorf("%s: default %q: %q %v", p.Name, p.Default, v, err)
			}
		case ReadOnly, Forbidden:
			if p.Why == "" {
				t.Errorf("%s: why can't it change?", p.Name)
			}
		default:
			t.Errorf("%s: no class", p.Name)
		}
	}
	if n := len(EditableParams()); n != 17 {
		t.Errorf("%d editable parameters, the validated selection has 17", n)
	}
}

func TestBaselineIsCISCompliant(t *testing.T) {
	fakeNode(t)
	if a := AuditCIS(Root); a.OK() {
		t.Fatal("a kernel's defaults shouldn't pass the benchmark")
	} else if a.Compliant() >= len(a.Results) {
		t.Fatal("Compliant() disagrees with OK()")
	}
	var log []string
	ApplyBaseline(func(f string, args ...any) { log = append(log, fmt.Sprintf(f, args...)) })
	a := AuditCIS(Root)
	if !a.OK() {
		t.Fatalf("not compliant after the baseline: %v", a.Failures())
	}
	if got := liveValue(t, "net.core.somaxconn"); got != "60000" {
		t.Errorf("somaxconn %s after the baseline", got)
	}
	if got := liveValue(t, "net.ipv4.tcp_rmem"); got != "4096 131072 6291456" {
		t.Errorf("tcp_rmem %s: a dynamic parameter must keep the kernel's value", got)
	}
	// Each interface too: the kernel uses its own value.
	for _, path := range []string{"net/ipv6/conf/eth0/accept_ra", "net/ipv6/conf/lo/accept_redirects", "net/ipv4/conf/eth0/secure_redirects"} {
		if v, _ := readValue(filepath.Join(Root, path)); v != "0" {
			t.Errorf("%s = %q after the baseline", path, v)
		}
	}
	if !slices.Contains(log, "sysctl "+keyPath(Root, "net.ipv4.tcp_syncookies")+"=1") {
		t.Errorf("each write is logged: %q", log)
	}
	// An interface that slips back is reported per interface.
	mustWrite(t, filepath.Join(Root, "net/ipv6/conf/eth0/accept_ra"), "1\n")
	a = AuditCIS(Root)
	if a.OK() || !strings.Contains(strings.Join(a.Failures(), " "), "eth0: 1") {
		t.Fatalf("eth0's accept_ra at 1 must fail 3.3.2.7: %v", a.Failures())
	}
	if a := EnforceCIS(func(string, ...any) {}); !a.OK() {
		t.Fatalf("EnforceCIS didn't fix it: %v", a.Failures())
	}
}

// TestBaselineWritesForwardingFirst: turning forwarding off resets
// conf.all.accept_redirects to 1 in the kernel, so it's written first.
func TestBaselineWritesForwardingFirst(t *testing.T) {
	index := map[string]int{}
	for i, s := range Baseline() {
		index[s.Key] = i
	}
	for _, fwd := range []string{"net.ipv4.ip_forward", "net.ipv4.conf.all.forwarding", "net.ipv4.conf.default.forwarding"} {
		for _, redirect := range []string{"net.ipv4.conf.all.accept_redirects", "net.ipv4.conf.default.accept_redirects"} {
			if index[fwd] > index[redirect] {
				t.Errorf("%s is written after %s", fwd, redirect)
			}
		}
	}
	for _, redirect := range []string{"net.ipv6.conf.all.accept_redirects", "net.ipv6.conf.all.accept_ra"} {
		if index["net.ipv6.conf.all.forwarding"] > index[redirect] {
			t.Errorf("IPv6 forwarding is written after %s", redirect)
		}
	}
}

// candidates are values of p to try: its bounds, its default, each
// allowed value, and values drawn within its bounds.
func candidates(p *Param, rnd *rand.Rand) []string {
	var out []string
	if !p.Dynamic {
		out = append(out, p.Default)
	}
	draw := func(b Bound) int64 { return b.Min + rnd.Int63n(b.Max-b.Min+1) }
	switch p.Kind {
	case KindEnum:
		for _, v := range p.Allowed {
			out = append(out, fmt.Sprint(v))
		}
	case KindInt:
		b := p.Bounds[0]
		out = append(out, fmt.Sprint(b.Min), fmt.Sprint(b.Max))
		for range 8 {
			out = append(out, fmt.Sprint(draw(b)))
		}
	case KindPair:
		lo, hi := p.Bounds[0], p.Bounds[1]
		out = append(out, fmt.Sprintf("%d %d", lo.Min, lo.Min+minPorts-1), fmt.Sprintf("%d %d", lo.Max, hi.Max), fmt.Sprintf("%d %d", lo.Min, hi.Max))
		for range 8 {
			a := draw(lo)
			out = append(out, fmt.Sprintf("%d %d", a, a+minPorts-1+rnd.Int63n(hi.Max-(a+minPorts-1)+1)))
		}
	case KindTriple:
		out = append(out, "4096 4096 65536", "65536 4194304 67108864", "4096 16060 262144", "4096 16384 262144")
		for range 8 {
			v := []int64{draw(p.Bounds[0]), draw(p.Bounds[1]), draw(p.Bounds[2])}
			slices.Sort(v)
			if v[1] > p.Bounds[1].Max { // keep the default within its own bounds
				v[1] = p.Bounds[1].Max
			}
			out = append(out, fmt.Sprintf("%d %d %d", v[0], v[1], v[2]))
		}
	case KindPorts:
		var many []string
		for i := range 32 {
			many = append(many, fmt.Sprint(20000+2*i))
		}
		out = append(out, "", "1024", "65535", "1024-2047,30000,65000-65535", strings.Join(many, ","))
	}
	return out
}

// snapshotCIS reads every CIS file, interfaces included.
func snapshotCIS(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, c := range CISControls {
		v, err := readValue(keyPath(Root, c.Key))
		if err != nil {
			t.Fatal(err)
		}
		out[c.Key] = v
		ifaces, err := interfaceDirs(Root, c.Interfaces)
		if err != nil {
			t.Fatal(err)
		}
		for _, iface := range ifaces {
			path := filepath.Join(Root, "net", c.Interfaces, "conf", iface, leaf(c.Key))
			out[path], _ = readValue(path)
		}
	}
	return out
}

// TestNoAllowedValueBreaksCIS is the benchmark's non-regression test:
// every editable parameter, set through the real write path to its
// bounds, its default, each allowed value and values drawn in between,
// leaves every CIS control compliant and no CIS file touched.
func TestNoAllowedValueBreaksCIS(t *testing.T) {
	booted(t)
	before := snapshotCIS(t)
	m := NewManager(true, quietProbes, nil)
	rnd := rand.New(rand.NewSource(1))
	tried := 0
	for _, p := range EditableParams() {
		for _, v := range candidates(p, rnd) {
			if _, err := m.ApplyTrial(Request{Changes: []Change{{Name: p.Name, Value: v}}, Actor: Actor{Name: "test"}}); err != nil {
				t.Errorf("%s=%q refused: %v", p.Name, v, err)
				continue
			}
			tried++
			if a := AuditCIS(Root); !a.OK() {
				t.Errorf("%s=%q breaks the benchmark: %v", p.Name, v, a.Failures())
			}
			if after := snapshotCIS(t); !mapsEqual(before, after) {
				t.Errorf("%s=%q touched a CIS file", p.Name, v)
			}
			if _, err := m.Cancel(Actor{Name: "test"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if tried < 100 {
		t.Errorf("only %d values tried", tried)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestRefusesAnythingOutsideTheWhitelist(t *testing.T) {
	booted(t)
	m := NewManager(true, quietProbes, nil)
	names := []string{
		"kernel.core_pattern", "net.ipv4.tcp_max_syn_backlog", "fs.nr_open", "vm.swappiness",
		"net.core.somaxconn ", "net/core/somaxconn", "net.core..somaxconn", "../net.core.somaxconn",
		"net.ipv4.conf.eth0.accept_redirects", "NET.CORE.SOMAXCONN", "",
	}
	for _, c := range CISControls {
		names = append(names, c.Key)
	}
	for _, name := range names {
		_, err := m.ApplyTrial(Request{Changes: []Change{{Name: name, Value: "1"}}})
		var inv *InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("%q: %v, want a refusal", name, err)
			continue
		}
		if c, ok := cisByKey(name); ok && !strings.Contains(err.Error(), "control "+c.ID()) {
			t.Errorf("%q: %v - a CIS key's refusal names its control", name, err)
		}
		if m.Trial() != nil {
			t.Fatalf("%q: something went on trial", name)
		}
	}
	if !strings.Contains(fmt.Sprint(m.ApplyTrial(Request{Changes: []Change{{Name: "fs.nr_open", Value: "1"}}})), "read-only") {
		t.Error("a read-only parameter's refusal says so")
	}
}

func TestParseChecksFormAndBounds(t *testing.T) {
	for _, c := range []struct {
		name, value, want, err string
	}{
		{"net.core.somaxconn", "60000", "60000", ""},
		{"net.core.somaxconn", " 8192 ", "8192", ""},
		{"net.core.somaxconn", "4095", "", "between 4096 and 65535"},
		{"net.core.somaxconn", "65536", "", "between"},
		{"net.core.somaxconn", "1e5", "", "isn't an integer"},
		{"net.core.somaxconn", "1 2", "", "one integer"},
		{"net.ipv4.tcp_tw_reuse", "3", "", "one of 0, 1, 2"},
		{"net.ipv4.tcp_fastopen", "1027", "", "one of"},
		{"net.ipv4.ip_local_port_range", "10240\t65023", "10240 65023", ""},
		{"net.ipv4.ip_local_port_range", "1023 65023", "", "the low end must be between 1024"},
		{"net.ipv4.ip_local_port_range", "10240", "", "2 integers"},
		{"net.ipv4.tcp_rmem", "4096 131072 6291456", "4096 131072 6291456", ""},
		{"net.ipv4.tcp_rmem", "4096 8388608 16777216", "", "the default must be between"},
		{"net.ipv4.ip_local_reserved_ports", "9100, 8080-8090,8085", "8080-8090,9100", ""},
		{"net.ipv4.ip_local_reserved_ports", "", "", ""},
		{"net.ipv4.ip_local_reserved_ports", "80", "", "port 80 isn't between"},
		{"net.ipv4.ip_local_reserved_ports", "9000-8000", "", "isn't a port or a range"},
		{"net.ipv4.ip_local_reserved_ports", "a", "", "isn't a port"},
	} {
		got, _, err := Lookup(c.name).Parse(c.value)
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s=%q: %v", c.name, c.value, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s=%q: %v, want %q", c.name, c.value, err, c.err)
		case c.err == "" && got != c.want:
			t.Errorf("%s=%q normalized to %q, want %q", c.name, c.value, got, c.want)
		}
	}
	var many []string
	for i := range 33 {
		many = append(many, fmt.Sprint(20000+2*i))
	}
	if _, _, err := Lookup("net.ipv4.ip_local_reserved_ports").Parse(strings.Join(many, ",")); err == nil {
		t.Error("33 reserved items accepted")
	}
}

func TestContextChecks(t *testing.T) {
	booted(t)
	listening := fakeProbes{listening: []int64{9505, 32400}, open: 100000, conntrack: 40000, mem: 1 << 30}
	m := NewManager(true, listening, nil)
	try := func(changes ...Change) error {
		_, err := m.Validate(Request{Changes: changes})
		return err
	}
	if err := try(Change{Name: "net.ipv4.ip_local_port_range", Value: "10240 65023"}); err == nil || !strings.Contains(err.Error(), "listens on 32400") {
		t.Errorf("a listening port inside the range: %v", err)
	}
	if err := try(Change{Name: "net.ipv4.ip_local_port_range", Value: "10240 65023"}, Change{Name: "net.ipv4.ip_local_reserved_ports", Value: "32400"}); err != nil {
		t.Errorf("reserved in the same request: %v", err)
	}
	if err := try(Change{Name: "net.ipv4.ip_local_port_range", Value: "10240 14335"}); err != nil {
		t.Errorf("a range below the listening port: %v", err)
	}
	if err := try(Change{Name: "net.ipv4.ip_local_port_range", Value: "10240 12000"}); err == nil {
		t.Error("a range of fewer than 4096 ports accepted")
	}
	if err := try(Change{Name: "fs.file-max", Value: "110000"}); err == nil || !strings.Contains(err.Error(), "125000") {
		t.Errorf("file-max under 125%% of the open files: %v", err)
	}
	if err := try(Change{Name: "net.netfilter.nf_conntrack_max", Value: "1000000"}); err == nil || !strings.Contains(err.Error(), "with this memory") {
		t.Errorf("conntrack beyond an eighth of the memory: %v", err)
	}
	if err := try(Change{Name: "net.netfilter.nf_conntrack_max", Value: "40000"}); err == nil || !strings.Contains(err.Error(), "tracked") {
		t.Errorf("conntrack under its entries: %v", err)
	}
	if err := try(Change{Name: "net.ipv4.tcp_wmem", Value: "65536 4096 262144"}); err == nil || !strings.Contains(err.Error(), "order") {
		t.Errorf("buffers out of order: %v", err)
	}
	// The live range has the listening port inside: a warning.
	s := m.Snapshot()
	for _, ps := range s.Params {
		if ps.Param.Name == "net.ipv4.ip_local_port_range" && (len(ps.Warnings) != 1 || !strings.Contains(ps.Warnings[0], "32400")) {
			t.Errorf("warnings %q", ps.Warnings)
		}
	}
}

func TestTrialRevertsUnlessConfirmed(t *testing.T) {
	booted(t)
	m := NewManager(true, quietProbes, nil)
	tr, err := m.ApplyTrial(Request{Changes: []Change{{Name: "net.core.somaxconn", Value: "30000"}}, Actor: Actor{Name: "alice", Roles: []string{"os:admin"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := liveValue(t, "net.core.somaxconn"); got != "30000" {
		t.Fatalf("on trial: %s", got)
	}
	if d := time.Until(tr.RevertAt); d < DefaultTrial-time.Minute || d > DefaultTrial {
		t.Errorf("reverts in %s", d)
	}
	// A second apply adds to the trial; the revert goes back to before
	// the first.
	if _, err := m.ApplyTrial(Request{Changes: []Change{{Name: "net.core.somaxconn", Value: "20000"}, {Name: "net.ipv4.tcp_fin_timeout", Value: "45"}}}); err != nil {
		t.Fatal(err)
	}
	if tr := m.Trial(); len(tr.Changes) != 2 || tr.Changes[0].Old != "60000" || tr.Changes[0].New != "20000" {
		t.Fatalf("trial %+v", tr)
	}
	m.revert(m.trial) // the timer firing
	if m.Trial() != nil {
		t.Fatal("still on trial after the revert")
	}
	if a, b := liveValue(t, "net.core.somaxconn"), liveValue(t, "net.ipv4.tcp_fin_timeout"); a != "60000" || b != "30" {
		t.Fatalf("after the revert: somaxconn %s, fin_timeout %s", a, b)
	}
	if _, _, err := LoadSaved(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a reverted trial saved something: %v", err)
	}
	h, err := History(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 || h[0].Action != ActionRevert || h[0].Actor.Name != "timeout" || h[2].Action != ActionTrial || h[2].Actor.String() != "alice (os:admin)" {
		t.Fatalf("history %+v", h)
	}
	if h[2].Changes[0] != (ChangeRecord{Name: "net.core.somaxconn", Old: "60000", New: "30000"}) {
		t.Errorf("trial record %+v", h[2].Changes)
	}
	if _, err := m.Cancel(Actor{}); !errors.Is(err, ErrNoTrial) {
		t.Errorf("cancel with nothing on trial: %v", err)
	}
}

func TestConfirmSaves(t *testing.T) {
	booted(t)
	m := NewManager(true, quietProbes, nil)
	tr, err := m.ApplyTrial(Request{Changes: []Change{
		{Name: "net.core.somaxconn", Value: "30000"},
		{Name: "net.ipv4.ip_local_reserved_ports", Value: "32400"},
		{Name: "net.ipv4.tcp_tw_reuse", Value: "1"}, // its default: not saved
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Confirm(tr.Started, Actor{Name: "alice"}); !errors.Is(err, ErrOldConnection) {
		t.Fatalf("confirm over a connection older than the trial: %v", err)
	}
	if _, err := m.Confirm(tr.Started.Add(time.Second), Actor{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(Dir, savedName))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "net.core.somaxconn = 30000\n") || !strings.Contains(body, "net.ipv4.ip_local_reserved_ports = 32400\n") || strings.Contains(body, "tcp_tw_reuse") {
		t.Fatalf("saved file:\n%s", body)
	}
	if m.Trial() != nil {
		t.Fatal("still on trial")
	}

	// A reboot: the kernel's defaults, then init's steps.
	for key, v := range kernelDefaults {
		mustWrite(t, keyPath(Root, key), v+"\n")
	}
	ApplyBaseline(func(string, ...any) {})
	if refused := ApplySaved(func(string, ...any) {}); len(refused) > 0 {
		t.Fatalf("refused %+v", refused)
	}
	if got := liveValue(t, "net.core.somaxconn"); got != "30000" {
		t.Fatalf("after a reboot: %s", got)
	}

	// Back to the default: the line goes.
	tr, err = m.ApplyTrial(Request{Changes: []Change{{Name: "net.core.somaxconn", Reset: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Confirm(tr.Started.Add(time.Second), Actor{}); err != nil {
		t.Fatal(err)
	}
	if s := Saved(); len(s) != 1 || s["net.ipv4.ip_local_reserved_ports"] != "32400" {
		t.Fatalf("saved %v", s)
	}
}

func TestResetAllAndDynamicDefaults(t *testing.T) {
	booted(t)
	m := NewManager(true, quietProbes, nil)
	tr, err := m.ApplyTrial(Request{Changes: []Change{
		{Name: "net.ipv4.tcp_rmem", Value: "4096 16060 262144"},
		{Name: "fs.file-max", Value: "500000"},
		{Name: "net.ipv4.tcp_fastopen", Value: "3"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Confirm(tr.Started.Add(time.Second), Actor{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplyTrial(Request{ResetAll: true, Changes: []Change{{Name: "fs.file-max", Value: "1"}}}); err == nil {
		t.Error("a reset of everything with another change accepted")
	}
	tr, err = m.ApplyTrial(Request{ResetAll: true})
	if err != nil {
		t.Fatal(err)
	}
	// Only what differs goes on trial.
	if len(tr.Changes) != 3 {
		t.Errorf("a reset of everything put %d parameters on trial, 3 differ: %+v", len(tr.Changes), tr.Changes)
	}
	// The dynamic defaults are the kernel's values at boot - the
	// conntrack table's 4096 is outside an operator's bounds, and fine.
	for name, want := range map[string]string{"net.ipv4.tcp_rmem": "4096 131072 6291456", "fs.file-max": "95000", "net.ipv4.tcp_fastopen": "1", "net.netfilter.nf_conntrack_max": "4096"} {
		if got := liveValue(t, name); got != want {
			t.Errorf("%s = %s after a reset, want %s", name, got, want)
		}
	}
	if _, err := m.Confirm(tr.Started.Add(time.Second), Actor{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSaved(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("everything at its default, yet a saved file: %v", err)
	}
	if _, err := m.ApplyTrial(Request{ResetAll: true}); err == nil || !strings.Contains(err.Error(), "already at its default") {
		t.Errorf("a reset with everything at its default: %v", err)
	}
}

func TestBootRefusesTamperedLines(t *testing.T) {
	booted(t)
	mustWrite(t, filepath.Join(Dir, savedName), `# tampered with
net.core.somaxconn = 30000
net.ipv4.tcp_syncookies = 0
net.ipv4.conf.all.accept_redirects = 1
kernel.core_pattern = |/bin/evil
net.ipv4.tcp_fin_timeout = 5
not a line
net.ipv4.tcp_max_syn_backlog = 60000
`)
	var log []string
	ApplyBaseline(func(string, ...any) {})
	refused := ApplySaved(func(f string, args ...any) { log = append(log, fmt.Sprintf(f, args...)) })
	if len(refused) != 6 {
		t.Fatalf("refused %+v", refused)
	}
	a := EnforceCIS(func(string, ...any) {})
	if !a.OK() {
		t.Fatalf("a tampered file broke the benchmark: %v", a.Failures())
	}
	if got := liveValue(t, "net.core.somaxconn"); got != "30000" {
		t.Errorf("the valid line: %s", got)
	}
	if v, _ := readValue(keyPath(Root, "kernel.core_pattern")); v != "core" {
		t.Errorf("core_pattern written: %q", v)
	}
	if got := liveValue(t, "net.ipv4.tcp_fin_timeout"); got != "30" {
		t.Errorf("an out-of-bounds value applied: %s", got)
	}
	joined := strings.Join(log, "\n")
	for _, want := range []string{"line 3: net.ipv4.tcp_syncookies: locked: " + Benchmark + " control 3.3.1.18", "line 5: kernel.core_pattern: forbidden", "line 7: not a \"name = value\" line"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log lacks %q:\n%s", want, joined)
		}
	}
	h, _ := History(1)
	if len(h) != 1 || h[0].Action != ActionBootRefused || len(h[0].Changes) != 6 {
		t.Fatalf("history %+v", h)
	}
}

func TestReloadsHAProxyWhenAsked(t *testing.T) {
	booted(t)
	reloads := 0
	var fail error
	m := NewManager(true, quietProbes, func() error { reloads++; return fail })
	if _, err := m.ApplyTrial(Request{Changes: []Change{{Name: "net.ipv4.tcp_fin_timeout", Value: "40"}}, ReloadHAProxy: true}); err != nil {
		t.Fatal(err)
	}
	if reloads != 0 {
		t.Fatal("reloaded for a parameter HAProxy doesn't read at its listeners")
	}
	if _, err := m.ApplyTrial(Request{Changes: []Change{{Name: "net.core.somaxconn", Value: "30000"}}, ReloadHAProxy: true}); err != nil {
		t.Fatal(err)
	}
	if reloads != 1 || !m.Trial().Reloaded {
		t.Fatalf("%d reloads", reloads)
	}
	if _, err := m.Cancel(Actor{}); err != nil {
		t.Fatal(err)
	}
	if reloads != 2 {
		t.Fatalf("not reloaded after the cancel: %d", reloads)
	}
	// A reload that fails undoes the change.
	fail = errors.New("haproxy: cannot bind socket")
	if _, err := m.ApplyTrial(Request{Changes: []Change{{Name: "net.ipv4.ip_nonlocal_bind", Value: "0"}}, ReloadHAProxy: true}); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("%v", err)
	}
	if got := liveValue(t, "net.ipv4.ip_nonlocal_bind"); got != "1" {
		t.Fatalf("still %s after the failed reload", got)
	}
}

func TestReadOnlyOffANode(t *testing.T) {
	booted(t)
	m := NewManager(false, quietProbes, nil)
	if _, err := m.ApplyTrial(Request{Changes: []Change{{Name: "net.core.somaxconn", Value: "30000"}}}); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("%v", err)
	}
	if s := m.Snapshot(); s.Managed || len(s.Params) != len(Catalog) || !s.CIS.OK() {
		t.Fatalf("snapshot %+v", s)
	}
}

func TestTrialTimeoutBounds(t *testing.T) {
	booted(t)
	m := NewManager(true, quietProbes, nil)
	for _, d := range []time.Duration{time.Second, 2 * time.Hour} {
		if _, err := m.ApplyTrial(Request{Changes: []Change{{Name: "net.core.somaxconn", Value: "30000"}}, Timeout: d}); err == nil {
			t.Errorf("a %s trial accepted", d)
		}
	}
}

func TestBootPutsUnconfirmedValuesBack(t *testing.T) {
	booted(t)
	m := NewManager(true, quietProbes, nil)
	if _, err := m.ApplyTrial(Request{Changes: []Change{{Name: "net.core.somaxconn", Value: "30000"}}}); err != nil {
		t.Fatal(err)
	}
	// janusd restarts: a new manager, the trial lost.
	if a := NewManager(true, quietProbes, nil).Boot(); !a.OK() {
		t.Fatal(a.Failures())
	}
	if got := liveValue(t, "net.core.somaxconn"); got != "60000" {
		t.Fatalf("after a janusd restart: %s", got)
	}
	m.trial.timer.Stop()
}

func TestHistoryKeepsTheNewest(t *testing.T) {
	fakeNode(t)
	for i := range historyMax + 5 {
		if err := appendHistory(Entry{Actor: Actor{Name: fmt.Sprint(i)}, Action: ActionTrial}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := History(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != historyMax || h[0].Actor.Name != fmt.Sprint(historyMax+4) || h[len(h)-1].Actor.Name != "5" {
		t.Fatalf("%d entries, newest %s, oldest %s", len(h), h[0].Actor.Name, h[len(h)-1].Actor.Name)
	}
	if h, _ := History(3); len(h) != 3 {
		t.Fatalf("limit: %d", len(h))
	}
}

func TestListeningPorts(t *testing.T) {
	const tcp = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:2521 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 100 0 0 10 0
   2: 0F02000A:2521 0202000A:C350 01 00000000:00000000 02:0000A0BB 00000000     0        0 3 4 0000000000000000 20 4 30 10 -1
`
	if got := listeningPorts(tcp); !slices.Equal(got, []int64{9505, 8080}) {
		t.Fatalf("%v", got)
	}
}

func TestRecommendRespectsBounds(t *testing.T) {
	p := *Lookup("net.core.somaxconn")
	p.Rules = []Rule{
		{ID: "too-high", Eval: func(Metrics) (Recommendation, bool) { return Recommendation{Value: "999999"}, true }},
		{ID: "ram-tier", Text: "16384 from 1 to 4 GiB", Eval: func(m Metrics) (Recommendation, bool) {
			return Recommendation{Value: "16384", Measured: []Measurement{{Name: "MemTotal", Value: fmt.Sprint(m.MemTotal)}}}, m.MemTotal >= 1<<30
		}},
	}
	rec, ok := p.Recommend(Metrics{MemTotal: 2 << 30})
	if !ok || rec.Value != "16384" || rec.RuleID != "ram-tier" || rec.Rule == "" {
		t.Fatalf("%+v %v", rec, ok)
	}
	ro := *Lookup("fs.nr_open")
	ro.Rules = p.Rules
	if _, ok := ro.Recommend(Metrics{MemTotal: 2 << 30}); ok {
		t.Fatal("a recommendation for a read-only parameter")
	}
}

// TestAuditFailsWhenInterfacesCantBeListed: per-interface controls are
// compliant only once every interface was checked - a real enforcing
// boot once denied the listing, and the audit passed by default.
func TestAuditFailsWhenInterfacesCantBeListed(t *testing.T) {
	booted(t)
	conf := filepath.Join(Root, "net/ipv6/conf")
	if err := os.Chmod(conf, 0o311); err != nil { // traversable, not listable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(conf, 0o755) })
	if _, err := os.ReadDir(conf); err == nil {
		t.Skip("running as root: the listing isn't denied")
	}
	a := AuditCIS(Root)
	if a.OK() || !strings.Contains(strings.Join(a.Failures(), " "), "the interfaces can't be listed") {
		t.Fatalf("an unlistable interface directory passed: %v", a.Failures())
	}
}
