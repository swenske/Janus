package sysctl

import "time"

// Recommendations - phase 2 (docs/guide/kernel-tuning.md): a parameter's
// Rules read what's known of the machine and suggest a value, with the
// measurement and the rule that led to it. A suggestion is only ever
// shown: the operator takes it, tests it on trial and confirms it like
// any change, within the same bounds. No rule exists yet; this is the
// shape they plug into.

// Metrics is what rules read about the node.
type Metrics struct {
	CPUs     int
	MemTotal int64 // bytes
	// Signals are saturation signals observed over a window, by name
	// ("ListenOverflows"): a rule acts on signals seen in many distinct
	// hours, never on one peak.
	Signals map[string]Signal
}

// Signal is a saturation signal over its observation window.
type Signal struct {
	Window time.Duration
	Hours  int     // distinct hours it was seen in
	Peak   float64 // its highest value
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
// within p's bounds wins. Never anything for a parameter that isn't
// Editable.
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
		if err != nil {
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
