package clusters

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Horizontal auto-scaling (docs/cluster.md section 6): when a cluster
// gains or loses a member, from samples of its members' load against a
// policy's thresholds. Pure: no clock, no I/O; the Controller's loop
// samples, decides and acts.
//
// The load is the members' aggregate - HAProxy's idle time and the CPUs'
// busy time averaged, the connections' share of maxconn the highest -
// projected to the member count the decision is about: what each member
// would carry with that many. A step out is judged at the count it has
// (or, observing, the count it would have had); a step in at one fewer,
// throughout its window. One signal is enough to step out; every signal
// must agree to step in.

// ScaleThresholds is when a cluster gains or loses a member. Zero values
// take ScaleDefaults'.
type ScaleThresholds struct {
	// A member more when, over OutAfter, HAProxy's idle time averages
	// below OutIdlePercent, the CPUs are busier than OutBusyPercent, or
	// the connections above OutConnPercent of maxconn.
	OutIdlePercent int           `json:"out_idle_percent,omitempty"`
	OutBusyPercent int           `json:"out_busy_percent,omitempty"`
	OutConnPercent int           `json:"out_conn_percent,omitempty"`
	OutAfter       time.Duration `json:"out_after,omitempty"`
	// A member less when, throughout InAfter, one fewer would still see
	// HAProxy idle at least InIdlePercent, the CPUs below InBusyPercent
	// and the connections below InConnPercent.
	InIdlePercent int           `json:"in_idle_percent,omitempty"`
	InBusyPercent int           `json:"in_busy_percent,omitempty"`
	InConnPercent int           `json:"in_conn_percent,omitempty"`
	InAfter       time.Duration `json:"in_after,omitempty"`
	// The least time after a step out, after a step in, before the next.
	OutCooldown time.Duration `json:"out_cooldown,omitempty"`
	InCooldown  time.Duration `json:"in_cooldown,omitempty"`
	// MaxStepsPerHour bounds every step.
	MaxStepsPerHour int `json:"max_steps_per_hour,omitempty"`
}

// ScaleDefaults are ScaleThresholds' zero values' values.
var ScaleDefaults = ScaleThresholds{
	OutIdlePercent: 30, OutBusyPercent: 75, OutConnPercent: 70, OutAfter: 5 * time.Minute,
	InIdlePercent: 50, InBusyPercent: 40, InConnPercent: 35, InAfter: 30 * time.Minute,
	OutCooldown: 10 * time.Minute, InCooldown: 30 * time.Minute, MaxStepsPerHour: 2,
}

// WithDefaults is t with its zero values set to ScaleDefaults'.
func (t ScaleThresholds) WithDefaults() ScaleThresholds {
	set := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	setD := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	d := ScaleDefaults
	set(&t.OutIdlePercent, d.OutIdlePercent)
	set(&t.OutBusyPercent, d.OutBusyPercent)
	set(&t.OutConnPercent, d.OutConnPercent)
	setD(&t.OutAfter, d.OutAfter)
	set(&t.InIdlePercent, d.InIdlePercent)
	set(&t.InBusyPercent, d.InBusyPercent)
	set(&t.InConnPercent, d.InConnPercent)
	setD(&t.InAfter, d.InAfter)
	setD(&t.OutCooldown, d.OutCooldown)
	setD(&t.InCooldown, d.InCooldown)
	set(&t.MaxStepsPerHour, d.MaxStepsPerHour)
	return t
}

// Validate says what's wrong with t (its defaults set) - the hysteresis
// first: a member added must not be taken away at once.
func (t ScaleThresholds) Validate() error {
	t = t.WithDefaults()
	for _, c := range []struct {
		name string
		v    int
	}{
		{"out_idle_percent", t.OutIdlePercent}, {"out_busy_percent", t.OutBusyPercent}, {"out_conn_percent", t.OutConnPercent},
		{"in_idle_percent", t.InIdlePercent}, {"in_busy_percent", t.InBusyPercent}, {"in_conn_percent", t.InConnPercent},
	} {
		if c.v < 1 || c.v > 99 {
			return fmt.Errorf("%s: 1 to 99 %%", c.name)
		}
	}
	switch {
	case t.InIdlePercent <= t.OutIdlePercent:
		return fmt.Errorf("in_idle_percent (%d) must be above out_idle_percent (%d), or a member would leave as soon as it came", t.InIdlePercent, t.OutIdlePercent)
	case t.InBusyPercent >= t.OutBusyPercent:
		return fmt.Errorf("in_busy_percent (%d) must be below out_busy_percent (%d)", t.InBusyPercent, t.OutBusyPercent)
	case t.InConnPercent >= t.OutConnPercent:
		return fmt.Errorf("in_conn_percent (%d) must be below out_conn_percent (%d)", t.InConnPercent, t.OutConnPercent)
	}
	for _, d := range []struct {
		name     string
		v        time.Duration
		min, max time.Duration
	}{
		{"out_after_seconds", t.OutAfter, time.Minute, time.Hour},
		{"in_after_seconds", t.InAfter, 5 * time.Minute, 24 * time.Hour},
		{"out_cooldown_seconds", t.OutCooldown, time.Minute, 6 * time.Hour},
		{"in_cooldown_seconds", t.InCooldown, time.Minute, 24 * time.Hour},
	} {
		if d.v < d.min || d.v > d.max {
			return fmt.Errorf("%s: %s to %s", d.name, d.min, d.max)
		}
	}
	if t.MaxStepsPerHour < 1 || t.MaxStepsPerHour > 12 {
		return fmt.Errorf("max_steps_per_hour: 1 to 12")
	}
	return nil
}

// The auto-scaling modes.
const (
	ScaleObserve = "observe" // decides and journals, changes nothing
	ScaleOn      = "on"
	ScaleOff     = "off"
)

// Autoscale is a cluster's horizontal auto-scaling.
type Autoscale struct {
	Mode       string `json:"mode"`
	MinMembers int    `json:"min_members"`
	MaxMembers int    `json:"max_members"`
	// MaxVCPUs and MaxMemoryMiB bound the members' total; 0: no bound.
	MaxVCPUs     int `json:"max_vcpus,omitempty"`
	MaxMemoryMiB int `json:"max_memory_mib,omitempty"`
	ScaleThresholds
	// PausedBy: the members were set from outside while on - by whom;
	// the engine decides but doesn't act until the policy is set again.
	PausedBy string `json:"paused_by,omitempty"`
}

// autoscaleJSON is Autoscale as the API and the store write it: its
// durations in seconds, as a machine's policy has them.
type autoscaleJSON struct {
	Mode               string `json:"mode"`
	MinMembers         int    `json:"min_members"`
	MaxMembers         int    `json:"max_members"`
	MaxVCPUs           int    `json:"max_vcpus,omitempty"`
	MaxMemoryMiB       int    `json:"max_memory_mib,omitempty"`
	OutIdlePercent     int    `json:"out_idle_percent,omitempty"`
	OutBusyPercent     int    `json:"out_busy_percent,omitempty"`
	OutConnPercent     int    `json:"out_conn_percent,omitempty"`
	OutAfterSeconds    int    `json:"out_after_seconds,omitempty"`
	InIdlePercent      int    `json:"in_idle_percent,omitempty"`
	InBusyPercent      int    `json:"in_busy_percent,omitempty"`
	InConnPercent      int    `json:"in_conn_percent,omitempty"`
	InAfterSeconds     int    `json:"in_after_seconds,omitempty"`
	OutCooldownSeconds int    `json:"out_cooldown_seconds,omitempty"`
	InCooldownSeconds  int    `json:"in_cooldown_seconds,omitempty"`
	MaxStepsPerHour    int    `json:"max_steps_per_hour,omitempty"`
	PausedBy           string `json:"paused_by,omitempty"`
	// v2026.10.24-5 and -6 wrote the durations in nanoseconds under
	// these names: still read.
	OutAfter    time.Duration `json:"out_after,omitempty"`
	InAfter     time.Duration `json:"in_after,omitempty"`
	OutCooldown time.Duration `json:"out_cooldown,omitempty"`
	InCooldown  time.Duration `json:"in_cooldown,omitempty"`
}

func (a Autoscale) MarshalJSON() ([]byte, error) {
	sec := func(d time.Duration) int { return int(d / time.Second) }
	t := a.ScaleThresholds
	return json.Marshal(autoscaleJSON{
		Mode: a.Mode, MinMembers: a.MinMembers, MaxMembers: a.MaxMembers, MaxVCPUs: a.MaxVCPUs, MaxMemoryMiB: a.MaxMemoryMiB,
		OutIdlePercent: t.OutIdlePercent, OutBusyPercent: t.OutBusyPercent, OutConnPercent: t.OutConnPercent, OutAfterSeconds: sec(t.OutAfter),
		InIdlePercent: t.InIdlePercent, InBusyPercent: t.InBusyPercent, InConnPercent: t.InConnPercent, InAfterSeconds: sec(t.InAfter),
		OutCooldownSeconds: sec(t.OutCooldown), InCooldownSeconds: sec(t.InCooldown), MaxStepsPerHour: t.MaxStepsPerHour, PausedBy: a.PausedBy,
	})
}

func (a *Autoscale) UnmarshalJSON(b []byte) error {
	var j autoscaleJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	dur := func(seconds int, old time.Duration) time.Duration {
		if seconds != 0 {
			return time.Duration(seconds) * time.Second
		}
		return old
	}
	*a = Autoscale{
		Mode: j.Mode, MinMembers: j.MinMembers, MaxMembers: j.MaxMembers, MaxVCPUs: j.MaxVCPUs, MaxMemoryMiB: j.MaxMemoryMiB, PausedBy: j.PausedBy,
		ScaleThresholds: ScaleThresholds{
			OutIdlePercent: j.OutIdlePercent, OutBusyPercent: j.OutBusyPercent, OutConnPercent: j.OutConnPercent, OutAfter: dur(j.OutAfterSeconds, j.OutAfter),
			InIdlePercent: j.InIdlePercent, InBusyPercent: j.InBusyPercent, InConnPercent: j.InConnPercent, InAfter: dur(j.InAfterSeconds, j.InAfter),
			OutCooldown: dur(j.OutCooldownSeconds, j.OutCooldown), InCooldown: dur(j.InCooldownSeconds, j.InCooldown), MaxStepsPerHour: j.MaxStepsPerHour,
		},
	}
	return nil
}

// Validate says what's wrong with a.
func (a Autoscale) Validate() error {
	switch a.Mode {
	case ScaleObserve, ScaleOn, ScaleOff:
	default:
		return fmt.Errorf("mode %q: observe, on or off", a.Mode)
	}
	if a.MinMembers < 2 {
		return fmt.Errorf("min_members: 2 at least - one member alone is no cluster to scale")
	}
	if a.MaxMembers < a.MinMembers || a.MaxMembers > 16 {
		return fmt.Errorf("max_members: from min_members (%d) to 16", a.MinMembers)
	}
	if a.MaxVCPUs < 0 || a.MaxMemoryMiB < 0 {
		return fmt.Errorf("max_vcpus, max_memory_mib: 0 (none) or more")
	}
	return a.ScaleThresholds.Validate()
}

// ScaleSample is one reading of the members in service.
type ScaleSample struct {
	At time.Time `json:"at"`
	// Members read for it.
	Members int `json:"members"`
	// IdlePercent is HAProxy's idle time, BusyPercent and StealPercent
	// the CPUs', averaged over the members; ConnPercent the highest share
	// of maxconn in use. Negative: unknown.
	IdlePercent  float64 `json:"idle_percent"`
	BusyPercent  float64 `json:"busy_percent"`
	StealPercent float64 `json:"steal_percent"`
	ConnPercent  float64 `json:"conn_percent"`
}

// ScaleState is what Decide remembers between two calls: the member
// count it judges - the cluster's, or what observation simulates - and
// its last steps.
type ScaleState struct {
	Members int         `json:"members"`
	LastOut time.Time   `json:"last_out,omitzero"`
	LastIn  time.Time   `json:"last_in,omitzero"`
	Steps   []time.Time `json:"steps,omitempty"` // within the last hour
}

// ScaleMaxStealPercent: the members' vCPUs wait that long for their
// hosts - a host short of CPU; a member more on it wouldn't help.
const ScaleMaxStealPercent = 10

// The scaling actions.
const (
	ScaleNone = ""
	ScaleOut  = "out"
	ScaleIn   = "in"
)

// ScaleDecision is Decide's answer.
type ScaleDecision struct {
	Action   string
	From, To int
	// Reason: what led to it, for the journal.
	Reason string
	// Waiting: why nothing happens though the load asks for it.
	Waiting  string
	Warnings []string
}

// ScaleSampleEvery is how often the members are read: a window needs
// four-fifths of its samples.
const ScaleSampleEvery = 15 * time.Second

// scaleWindow is the samples of the last d, when there are enough.
func scaleWindow(samples []ScaleSample, now time.Time, d time.Duration) ([]ScaleSample, bool) {
	var w []ScaleSample
	for _, s := range samples {
		if !s.At.Before(now.Add(-d)) && !s.At.After(now) {
			w = append(w, s)
		}
	}
	return w, float64(len(w)) >= 0.8*float64(d/ScaleSampleEvery)
}

// at projects a sample's load to n members: what each would carry.
func (s ScaleSample) at(n int) (idle, busy, conn float64) {
	f := float64(s.Members) / float64(max(n, 1))
	idle, busy, conn = -1, -1, -1
	if s.IdlePercent >= 0 {
		idle = clamp(100 - (100-s.IdlePercent)*f)
	}
	if s.BusyPercent >= 0 {
		busy = clamp(s.BusyPercent * f)
	}
	if s.ConnPercent >= 0 {
		conn = clamp(s.ConnPercent * f)
	}
	return
}

func clamp(v float64) float64 { return math.Max(0, math.Min(100, v)) }

// mean averages f over the samples where it is known.
func mean(w []ScaleSample, f func(ScaleSample) float64) (float64, bool) {
	sum, n := 0.0, 0
	for _, s := range w {
		if v := f(s); v >= 0 {
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// Decide says what cluster should do now: a member more, one less, or
// nothing - and why.
func Decide(p Autoscale, st ScaleState, samples []ScaleSample, now time.Time) ScaleDecision {
	t := p.WithDefaults()
	n := st.Members
	d := ScaleDecision{From: n, To: n}
	recent := 0
	for _, s := range st.Steps {
		if now.Sub(s) < time.Hour {
			recent++
		}
	}

	// Out: one signal, at the count judged.
	if w, ok := scaleWindow(samples, now, t.OutAfter); ok {
		var why []string
		if v, ok := mean(w, func(s ScaleSample) float64 { i, _, _ := s.at(n); return i }); ok && v < float64(t.OutIdlePercent) {
			why = append(why, fmt.Sprintf("HAProxy idle %.0f %% < %d %%", v, t.OutIdlePercent))
		}
		if v, ok := mean(w, func(s ScaleSample) float64 { _, b, _ := s.at(n); return b }); ok && v > float64(t.OutBusyPercent) {
			why = append(why, fmt.Sprintf("CPUs %.0f %% busy > %d %%", v, t.OutBusyPercent))
		}
		if v, ok := mean(w, func(s ScaleSample) float64 { _, _, c := s.at(n); return c }); ok && v > float64(t.OutConnPercent) {
			why = append(why, fmt.Sprintf("connections at %.0f %% of maxconn > %d %%", v, t.OutConnPercent))
		}
		if len(why) > 0 {
			reason := fmt.Sprintf("over %s with %d members: %s", t.OutAfter, n, joinComma(why))
			steal, _ := mean(w, func(s ScaleSample) float64 { return s.StealPercent })
			switch {
			case n >= p.MaxMembers:
				d.Waiting = fmt.Sprintf("%s - at max_members (%d)", reason, p.MaxMembers)
			case steal > ScaleMaxStealPercent:
				d.Waiting = reason
				d.Warnings = append(d.Warnings, fmt.Sprintf("the members' vCPUs wait %.0f %% of the time for their hosts: hosts short of CPU, a member more there wouldn't help", steal))
			case !st.LastOut.IsZero() && now.Sub(st.LastOut) < t.OutCooldown:
				d.Waiting = fmt.Sprintf("%s - cooling down after the last member added", reason)
			case !st.LastIn.IsZero() && now.Sub(st.LastIn) < t.OutCooldown:
				d.Waiting = fmt.Sprintf("%s - cooling down after the last member removed", reason)
			case recent >= t.MaxStepsPerHour:
				d.Waiting = fmt.Sprintf("%s - %d steps in the last hour already", reason, recent)
			default:
				d.Action, d.To, d.Reason = ScaleOut, n+1, reason
			}
			return d
		}
	}

	// In: every signal, at one fewer, throughout.
	if n <= p.MinMembers {
		return d
	}
	w, ok := scaleWindow(samples, now, t.InAfter)
	if !ok {
		return d
	}
	for _, s := range w {
		i, b, c := s.at(n - 1)
		if i < float64(t.InIdlePercent) || b < 0 || b >= float64(t.InBusyPercent) || (c >= 0 && c >= float64(t.InConnPercent)) {
			return d
		}
	}
	reason := fmt.Sprintf("throughout %s, %d members would still see HAProxy idle %d %% or more, the CPUs below %d %%, connections below %d %% of maxconn", t.InAfter, n-1, t.InIdlePercent, t.InBusyPercent, t.InConnPercent)
	switch {
	case now.Sub(st.LastOut) < t.InCooldown || now.Sub(st.LastIn) < t.InCooldown:
		d.Waiting = reason + " - cooling down after the last step"
	case recent >= t.MaxStepsPerHour:
		d.Waiting = fmt.Sprintf("%s - %d steps in the last hour already", reason, recent)
	default:
		d.Action, d.To, d.Reason = ScaleIn, n-1, reason
	}
	return d
}

// Apply is st once d was carried out (or simulated) at now.
func (st ScaleState) Apply(d ScaleDecision, now time.Time) ScaleState {
	if d.Action == ScaleNone {
		return st
	}
	st.Members = d.To
	if d.Action == ScaleOut {
		st.LastOut = now
	} else {
		st.LastIn = now
	}
	var steps []time.Time
	for _, s := range st.Steps {
		if now.Sub(s) < time.Hour {
			steps = append(steps, s)
		}
	}
	st.Steps = append(steps, now)
	return st
}

func joinComma(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
