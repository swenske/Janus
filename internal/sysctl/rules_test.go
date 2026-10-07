package sysctl

import (
	"net/netip"
	"strings"
	"testing"
)

// janusLive is a node at Janus's defaults, with the kernel's values for
// the Dynamic parameters on a 1 GiB machine.
func janusLive() map[string]string {
	live := map[string]string{}
	for _, p := range EditableParams() {
		live[p.Name] = p.Default
	}
	live["net.ipv4.tcp_rmem"] = "4096 131072 4194304"
	live["net.ipv4.tcp_wmem"] = "4096 16384 4194304"
	live["fs.file-max"] = "98304"
	live["net.netfilter.nf_conntrack_max"] = "8192"
	return live
}

const gib = 1 << 30

func suggest(t *testing.T, name string, m Metrics) (Recommendation, bool) {
	t.Helper()
	if m.Live == nil {
		m.Live = janusLive()
	}
	return Lookup(name).Recommend(m)
}

func TestRules(t *testing.T) {
	listeners := []Listener{{Addr: netip.MustParseAddrPort("0.0.0.0:443"), Backlog: 60000}, {Addr: netip.MustParseAddrPort("0.0.0.0:80"), Backlog: 60000}}
	for _, tc := range []struct {
		name    string
		param   string
		m       Metrics
		want    string // "" for no suggestion
		ruleID  string
		measure string // in a measurement's value
	}{
		{
			name: "accept queues overflowing in 3 hours: twice somaxconn", param: "net.core.somaxconn",
			m:    Metrics{Signals: map[string]SignalState{"accept_overflows": {Hours: 3, Total: 1520}}, MemTotal: 64 * gib, Listeners: listeners},
			want: "65535", ruleID: "somaxconn.overflows", measure: "1,520, in 3 different hours; HAProxy's listeners at net.core.somaxconn: 2: 0.0.0.0:443 (backlog 60000)",
		},
		{
			name: "in 2 hours only: nothing observed counts - the memory rule's turn", param: "net.core.somaxconn",
			m: Metrics{Signals: map[string]SignalState{"accept_overflows": {Hours: 2, Total: 99999}}, MemTotal: 64 * gib, Listeners: listeners},
		},
		{
			name: "overflows, but HAProxy's listeners asked for less than somaxconn: not the kernel's limit", param: "net.core.somaxconn",
			m: Metrics{Signals: map[string]SignalState{"accept_overflows": {Hours: 9, Total: 99}}, MemTotal: 64 * gib, Listeners: []Listener{{Addr: netip.MustParseAddrPort("0.0.0.0:443"), Backlog: 1000}}},
		},
		{
			name: "1 GiB, two listeners: an eighth of it at 4 KiB a connection, per listener", param: "net.core.somaxconn",
			m:    Metrics{MemTotal: gib - 40<<20, Listeners: listeners},
			want: "8192", ruleID: "somaxconn.memory", measure: "2: 0.0.0.0:443 (backlog 60000)",
		},
		{
			name: "256 MiB, many listeners: 4096 at least", param: "net.core.somaxconn",
			m:    Metrics{MemTotal: 256 << 20, Listeners: append(append([]Listener{}, listeners...), listeners...)},
			want: "4096", ruleID: "somaxconn.memory",
		},
		{
			name: "8 GiB: nothing lower than 60000", param: "net.core.somaxconn",
			m: Metrics{MemTotal: 8 * gib, Listeners: listeners},
		},
		{
			name: "no HAProxy listener: no memory rule", param: "net.core.somaxconn",
			m: Metrics{MemTotal: gib},
		},
		{
			name: "source ports half used: from above the highest listener to 65023", param: "net.ipv4.ip_local_port_range",
			m: Metrics{
				Signals:        map[string]SignalState{"source_ports": {Hours: 4, Peak: 0.62, PeakAbs: 28000, Detail: "10.0.0.5:8080"}},
				ListeningPorts: []int64{22, 443, 9505, 10056},
				Live: func() map[string]string {
					l := janusLive()
					l["net.ipv4.ip_local_port_range"] = "32768 60999"
					return l
				}(),
			},
			want: "10240 65023", ruleID: "port_range.usage", measure: "62% (28,000 sockets) to 10.0.0.5:8080 at the highest; from 50% in 4 different hours",
		},
		{
			name: "a reserved listener inside doesn't move the low end", param: "net.ipv4.ip_local_port_range",
			m: Metrics{
				Signals:        map[string]SignalState{"source_ports": {Hours: 4, Peak: 0.62}},
				ListeningPorts: []int64{9505, 10056, 30000},
				Live: func() map[string]string {
					l := janusLive()
					l["net.ipv4.ip_local_port_range"] = "32768 60999"
					l["net.ipv4.ip_local_reserved_ports"] = "30000"
					return l
				}(),
			},
			want: "10240 65023",
		},
		{
			name: "already the widest: nothing", param: "net.ipv4.ip_local_port_range",
			m: Metrics{Signals: map[string]SignalState{"source_ports": {Hours: 9, Peak: 0.9}}, ListeningPorts: []int64{9505, 10056}},
		},
		{
			name: "outgoing rate: tw_reuse 1, from 0", param: "net.ipv4.tcp_tw_reuse",
			m: Metrics{
				Signals: map[string]SignalState{"outgoing_rate": {Hours: 3, Peak: 812}},
				Live:    func() map[string]string { l := janusLive(); l["net.ipv4.tcp_tw_reuse"] = "0"; return l }(),
			},
			want: "1", ruleID: "tw_reuse.rate", measure: "812/s at the highest; from 300/s in 3 different hours",
		},
		{
			name: "outgoing rate, already 1: nothing", param: "net.ipv4.tcp_tw_reuse",
			m: Metrics{Signals: map[string]SignalState{"outgoing_rate": {Hours: 3, Peak: 812}}},
		},
		{
			name: "SYN cookies: synack_retries 2", param: "net.ipv4.tcp_synack_retries",
			m:    Metrics{Signals: map[string]SignalState{"syn_cookies": {Hours: 5, Total: 30}}},
			want: "2", ruleID: "synack_retries.cookies",
		},
		{
			name: "SYN cookies, already 1: never raised", param: "net.ipv4.tcp_synack_retries",
			m: Metrics{
				Signals: map[string]SignalState{"syn_cookies": {Hours: 5, Total: 30}},
				Live:    func() map[string]string { l := janusLive(); l["net.ipv4.tcp_synack_retries"] = "1"; return l }(),
			},
		},
		{
			name: "input backlog drops: twice", param: "net.core.netdev_max_backlog",
			m:    Metrics{Signals: map[string]SignalState{"backlog_drops": {Hours: 3, Total: 4}}},
			want: "20000", ruleID: "netdev_max_backlog.drops",
		},
		{
			name: "TCP memory pressure: HAProxy's smaller receive buffers", param: "net.ipv4.tcp_rmem",
			m:    Metrics{Signals: map[string]SignalState{"tcp_memory": {Hours: 3, Total: 12}}},
			want: "4096 16060 262144", ruleID: "tcp_rmem.memory",
		},
		{
			name: "... and send buffers", param: "net.ipv4.tcp_wmem",
			m:    Metrics{Signals: map[string]SignalState{"tcp_memory": {Hours: 3, Total: 12}}},
			want: "4096 16384 262144", ruleID: "tcp_wmem.memory",
		},
		{
			name: "open files at 80%: twice the peak, by 1024", param: "fs.file-max",
			m:    Metrics{Signals: map[string]SignalState{"open_files": {Hours: 3, Peak: 0.85, PeakAbs: 83600}}},
			want: "167936", ruleID: "file_max.usage", measure: "85% (83,600 files) at the highest",
		},
		{
			name: "conntrack at 75%: twice the peak, no less than the memory rule", param: "net.netfilter.nf_conntrack_max",
			m:    Metrics{Signals: map[string]SignalState{"conntrack_usage": {Hours: 3, Peak: 0.9, PeakAbs: 7400}}, MemTotal: gib, Conntrack: true},
			want: "104448", ruleID: "conntrack_max.usage", measure: "90% (7,400 entries) at the highest",
		},
		{
			name: "conntrack at 75% of a big table: twice the peak", param: "net.netfilter.nf_conntrack_max",
			m: Metrics{
				Signals: map[string]SignalState{"conntrack_usage": {Hours: 3, Peak: 0.8, PeakAbs: 210000}}, MemTotal: 16 * gib, Conntrack: true,
				Live: func() map[string]string { l := janusLive(); l["net.netfilter.nf_conntrack_max"] = "262144"; return l }(),
			},
			want: "420864", ruleID: "conntrack_max.usage",
		},
		{
			name: "conntrack dropping: twice its size, capped by memory", param: "net.netfilter.nf_conntrack_max",
			m:    Metrics{Signals: map[string]SignalState{"conntrack_drops": {Hours: 3, Total: 9}}, MemTotal: 64 << 20, Conntrack: true},
			want: "16384", ruleID: "conntrack_max.usage", measure: "9, in 3 different hours",
		},
		{
			name: "conntrack in use, nothing observed: a thirty-second of the memory", param: "net.netfilter.nf_conntrack_max",
			m:    Metrics{MemTotal: gib, Conntrack: true},
			want: "104448", ruleID: "conntrack_max.memory",
		},
		{
			name: "conntrack in use on 64 GiB: 262144 at most", param: "net.netfilter.nf_conntrack_max",
			m:    Metrics{MemTotal: 64 * gib, Conntrack: true},
			want: "262144", ruleID: "conntrack_max.memory",
		},
		{
			name: "conntrack unused: nothing", param: "net.netfilter.nf_conntrack_max",
			m: Metrics{MemTotal: gib},
		},
		{
			name: "the keepalives: no rule", param: "net.ipv4.tcp_keepalive_time",
			m: Metrics{MemTotal: gib, Signals: map[string]SignalState{"outgoing_rate": {Hours: 99}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, ok := suggest(t, tc.param, tc.m)
			if tc.want == "" {
				if ok {
					t.Fatalf("suggested %q (%s)", rec.Value, rec.RuleID)
				}
				return
			}
			if !ok || rec.Value != tc.want || rec.RuleID != tc.ruleID && tc.ruleID != "" {
				t.Fatalf("suggested %q (%s, %v), want %q (%s)", rec.Value, rec.RuleID, ok, tc.want, tc.ruleID)
			}
			if rec.Rule == "" || len(rec.Sources) == 0 || len(rec.Measured) == 0 {
				t.Errorf("a suggestion without its rule, sources or measurements: %+v", rec)
			}
			if tc.measure != "" && !strings.Contains(measuredText(rec), tc.measure) {
				t.Errorf("measurements %q, want %q in them", measuredText(rec), tc.measure)
			}
		})
	}
}

func measuredText(r Recommendation) string {
	var parts []string
	for _, m := range r.Measured {
		parts = append(parts, m.Name+": "+m.Value)
	}
	return strings.Join(parts, "; ")
}

// TestEveryRuleIsDocumented: unique IDs, a text and sources each, and
// only on Editable parameters.
func TestEveryRuleIsDocumented(t *testing.T) {
	ids := map[string]bool{}
	for _, p := range Catalog {
		if p.Class != Editable && len(p.Rules) > 0 {
			t.Errorf("%s isn't Editable but has rules", p.Name)
		}
		for _, r := range p.Rules {
			if ids[r.ID] {
				t.Errorf("rule %s twice", r.ID)
			}
			ids[r.ID] = true
			if r.Text == "" || len(r.Sources) == 0 || r.Eval == nil {
				t.Errorf("rule %s: text, sources and Eval needed", r.ID)
			}
		}
	}
	if len(ids) != 11 {
		t.Errorf("%d rules, want 11", len(ids))
	}
	for _, s := range Signals {
		if s.Title == "" || s.Measure == "" || s.Seen == "" || s.Kind == Gauge && s.Threshold == 0 {
			t.Errorf("signal %s isn't fully described", s.ID)
		}
	}
}
