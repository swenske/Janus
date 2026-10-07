package sysctl

import (
	"net/netip"
	"time"
)

// Recommendations (docs/guide/kernel-tuning.md#suggestions): a
// parameter's Rules read what's known of the machine - its memory,
// HAProxy's listeners, the signals the node observed (signals.go) - and
// suggest a value, with the measurements and the rule that led to it. A
// suggestion is only ever shown: the operator takes it, tests it on trial
// and confirms it like any change, within the same bounds and checks.

// Metrics is what rules read about the node.
type Metrics struct {
	CPUs     int
	MemTotal int64 // bytes
	// Live is every Editable parameter's live value, normalized.
	Live map[string]string
	// Signals are the signals observed over Window, by ID - none when
	// nothing observes (a janusd that doesn't run a Janus node).
	Signals map[string]SignalState
	// Listeners are HAProxy's listening sockets; ListeningPorts every
	// TCP port the node listens on.
	Listeners      []Listener
	ListeningPorts []int64
	// Conntrack: the kernel tracks connections - the nftables
	// extension's rules use it.
	Conntrack bool
}

// SignalState is a signal over Window.
type SignalState struct {
	Hours int // distinct hours it was seen in
	// Total is a Counter's increase over the window; Peak its biggest
	// hour, or a Gauge's highest value - and PeakAbs what that was in
	// absolute (open files, entries, sockets), Detail what peaked (a
	// destination).
	Total    float64
	Peak     float64
	PeakAbs  int64
	Detail   string
	LastSeen time.Time // the last hour it was seen in
}

// Listener is a listening socket of HAProxy's.
type Listener struct {
	Addr netip.AddrPort
	// Backlog is the one the kernel gave it: what HAProxy asked for, at
	// most net.core.somaxconn.
	Backlog uint32
}

// Observation is what the node observed, for SysctlList.
type Observation struct {
	Since   time.Time // the oldest hour kept
	Signals map[string]SignalState
}

// Measurement is a piece of data a suggestion rests on.
type Measurement struct {
	Name   string
	Value  string
	Window string // "" for a fact about the machine (its memory)
}

// Recommendation is a suggested value and why.
type Recommendation struct {
	Value    string
	RuleID   string
	Rule     string // the rule, in words: tiers or formula
	Measured []Measurement
	Sources  []Source
}

// Rule suggests a value from Metrics, or nothing.
type Rule struct {
	ID      string
	Text    string
	Sources []Source
	Eval    func(Metrics) (Recommendation, bool)
}

// Recommend runs p's rules in order: the first that suggests a value
// within p's bounds, other than the live one, wins. Never anything for a
// parameter that isn't Editable.
func (p *Param) Recommend(m Metrics) (Recommendation, bool) {
	if p.Class != Editable {
		return Recommendation{}, false
	}
	for _, r := range p.Rules {
		rec, ok := r.Eval(m)
		if !ok {
			continue
		}
		v, _, err := p.Parse(rec.Value)
		if err != nil || v == m.Live[p.Name] {
			continue
		}
		rec.Value = v
		if rec.RuleID == "" {
			rec.RuleID = r.ID
		}
		if rec.Rule == "" {
			rec.Rule = r.Text
		}
		if len(rec.Sources) == 0 {
			rec.Sources = r.Sources
		}
		return rec, true
	}
	return Recommendation{}, false
}
