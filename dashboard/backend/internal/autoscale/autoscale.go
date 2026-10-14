// Package autoscale decides when a scalable machine's vCPUs and memory
// change (docs/guide/auto-scaling.md): from samples of its node's load -
// HAProxy's idle time, the CPUs' busy time, the memory available, OOM
// kills - against a policy's thresholds. Pure: no clock, no I/O; the
// Controller's loop (dashboard/backend: autoscale.go) samples, decides
// and acts.
//
// Every measurement is projected to the size the decision is about:
// what the load would be with that many vCPUs, that much memory. A
// machine resized moments ago is judged at its new size; and in
// observation mode, where nothing is resized, the size the policy would
// have given is simulated - it stops growing where the real load says
// it would have been enough.
//
// HAProxy's documentation gives no number for any of this: the defaults
// below are Janus's starting points, the operator's to change.
package autoscale

import (
	"fmt"
	"math"
	"time"
)

// Size is a machine's vCPUs and memory.
type Size struct {
	VCPUs     int `json:"vcpus"`
	MemoryMiB int `json:"memory_mib"`
}

func (s Size) String() string { return fmt.Sprintf("%d vCPU, %d MiB", s.VCPUs, s.MemoryMiB) }

// Thresholds is when a machine grows and shrinks. Zero values take the
// defaults (WithDefaults).
type Thresholds struct {
	// A vCPU more when, over CPUUpAfter, HAProxy's idle time averages
	// below CPUUpIdlePercent, or the CPUs are busier than
	// CPUUpBusyPercent on average.
	CPUUpIdlePercent int           `json:"cpu_up_idle_percent,omitempty"`
	CPUUpBusyPercent int           `json:"cpu_up_busy_percent,omitempty"`
	CPUUpAfter       time.Duration `json:"cpu_up_after,omitempty"`
	// A vCPU less when, throughout ReturnAfter, HAProxy would stay idle
	// CPUDownIdlePercent of the time with one fewer - and the CPUs that
	// much free.
	CPUDownIdlePercent int `json:"cpu_down_idle_percent,omitempty"`
	// More memory when what's available stays below
	// MemoryUpAvailablePercent for MemoryUpAfter - at once on an OOM
	// kill.
	MemoryUpAvailablePercent int           `json:"memory_up_available_percent,omitempty"`
	MemoryUpAfter            time.Duration `json:"memory_up_after,omitempty"`
	// Less when, throughout ReturnAfter, MemoryDownAvailablePercent would
	// stay available with less.
	MemoryDownAvailablePercent int `json:"memory_down_available_percent,omitempty"`
	// ReturnAfter: how long the load must stay low before the machine
	// takes a step back towards its nominal size.
	ReturnAfter time.Duration `json:"return_after,omitempty"`
	// Cooldown: the least time between two steps up, for the last one to
	// show in the load.
	Cooldown time.Duration `json:"cooldown,omitempty"`
	// MaxStepsPerHour bounds every step, up or down.
	MaxStepsPerHour int `json:"max_steps_per_hour,omitempty"`
}

// Defaults are Thresholds' zero values' values.
var Defaults = Thresholds{
	CPUUpIdlePercent:           30,
	CPUUpBusyPercent:           75,
	CPUUpAfter:                 5 * time.Minute,
	CPUDownIdlePercent:         50,
	MemoryUpAvailablePercent:   20,
	MemoryUpAfter:              2 * time.Minute,
	MemoryDownAvailablePercent: 40,
	ReturnAfter:                30 * time.Minute,
	Cooldown:                   5 * time.Minute,
	MaxStepsPerHour:            6,
}

// WithDefaults is t with its zero values set to Defaults'.
func (t Thresholds) WithDefaults() Thresholds {
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
	set(&t.CPUUpIdlePercent, Defaults.CPUUpIdlePercent)
	set(&t.CPUUpBusyPercent, Defaults.CPUUpBusyPercent)
	setD(&t.CPUUpAfter, Defaults.CPUUpAfter)
	set(&t.CPUDownIdlePercent, Defaults.CPUDownIdlePercent)
	set(&t.MemoryUpAvailablePercent, Defaults.MemoryUpAvailablePercent)
	setD(&t.MemoryUpAfter, Defaults.MemoryUpAfter)
	set(&t.MemoryDownAvailablePercent, Defaults.MemoryDownAvailablePercent)
	setD(&t.ReturnAfter, Defaults.ReturnAfter)
	setD(&t.Cooldown, Defaults.Cooldown)
	set(&t.MaxStepsPerHour, Defaults.MaxStepsPerHour)
	return t
}

// Validate says what's wrong with t (its defaults set) - the
// hysteresis above all: a machine that grows must not shrink back at
// once.
func (t Thresholds) Validate() error {
	t = t.WithDefaults()
	pct := func(name string, v int) error {
		if v < 1 || v > 99 {
			return fmt.Errorf("%s: 1 to 99 %%", name)
		}
		return nil
	}
	for _, c := range []struct {
		name string
		v    int
	}{
		{"cpu_up_idle_percent", t.CPUUpIdlePercent}, {"cpu_up_busy_percent", t.CPUUpBusyPercent},
		{"cpu_down_idle_percent", t.CPUDownIdlePercent}, {"memory_up_available_percent", t.MemoryUpAvailablePercent},
		{"memory_down_available_percent", t.MemoryDownAvailablePercent},
	} {
		if err := pct(c.name, c.v); err != nil {
			return err
		}
	}
	if t.CPUDownIdlePercent <= t.CPUUpIdlePercent {
		return fmt.Errorf("cpu_down_idle_percent (%d) must be above cpu_up_idle_percent (%d), or a machine would shrink as soon as it grew", t.CPUDownIdlePercent, t.CPUUpIdlePercent)
	}
	if 100-t.CPUDownIdlePercent >= t.CPUUpBusyPercent {
		return fmt.Errorf("with cpu_down_idle_percent %d, the CPUs may be %d %% busy after a step down - cpu_up_busy_percent (%d) must be above that", t.CPUDownIdlePercent, 100-t.CPUDownIdlePercent, t.CPUUpBusyPercent)
	}
	if t.MemoryDownAvailablePercent <= t.MemoryUpAvailablePercent {
		return fmt.Errorf("memory_down_available_percent (%d) must be above memory_up_available_percent (%d)", t.MemoryDownAvailablePercent, t.MemoryUpAvailablePercent)
	}
	for _, d := range []struct {
		name     string
		v        time.Duration
		min, max time.Duration
	}{
		{"cpu_up_after", t.CPUUpAfter, time.Minute, time.Hour},
		{"memory_up_after", t.MemoryUpAfter, 30 * time.Second, time.Hour},
		{"return_after", t.ReturnAfter, time.Minute, 24 * time.Hour},
		{"cooldown", t.Cooldown, time.Minute, 6 * time.Hour},
	} {
		if d.v < d.min || d.v > d.max {
			return fmt.Errorf("%s: %s to %s", d.name, d.min, d.max)
		}
	}
	if t.MaxStepsPerHour < 1 || t.MaxStepsPerHour > 60 {
		return fmt.Errorf("max_steps_per_hour: 1 to 60")
	}
	return nil
}

// Policy is what Decide works with.
type Policy struct {
	Nominal, Max Size
	// MemoryStepMiB is the hypervisor's: memory changes by multiples of
	// it (0: it can't change while the machine runs).
	MemoryStepMiB int
	// ThreadsPinned is the keyword of HAProxy's configuration that fixes
	// its threads ("nbthread", "cpu-map"), "" when none does: vCPUs then
	// can't help.
	ThreadsPinned string
	Thresholds
}

// Sample is one reading of a node, every few seconds.
type Sample struct {
	At time.Time
	// CPUs online when it was taken.
	CPUs int
	// IdlePercent is HAProxy's (show info's Idle_pct); BusyPercent the
	// CPUs' since the previous sample, StealPercent the time they waited
	// for the host meanwhile (not busy). Negative: unknown.
	IdlePercent  float64
	BusyPercent  float64
	StealPercent float64
	// Memory, in MiB: the machine's, and what's available.
	MemTotalMiB, MemAvailableMiB int
	// OOMKills since the node booted.
	OOMKills uint64
	// HAProxy's connections and its limit.
	CurrConns, MaxConn uint64
}

// State is what Decide remembers between two calls: the size it judges
// - what the machine has, or what observation simulates - and its last
// steps.
type State struct {
	Size   Size        `json:"size"`
	LastUp time.Time   `json:"last_up,omitempty"`
	Steps  []time.Time `json:"steps,omitempty"` // within the last hour
	// OOMKills is the node's count of OOM kills the last decision saw:
	// kills above it are new (OOMCounted: there was one).
	OOMKills   uint64 `json:"oom_kills,omitempty"`
	OOMCounted bool   `json:"oom_counted,omitempty"`
}

// MaxStealPercent is the steal time above which a vCPU more isn't asked
// for: the vCPUs already wait that long for the host's CPUs, and one more
// would wait as well - a host short of CPU, not a machine.
const MaxStealPercent = 10

// Action is what a decision does.
type Action string

const (
	None Action = ""
	Up   Action = "up"
	Down Action = "down"
)

// Decision is Decide's answer.
type Decision struct {
	Action Action
	// Resource is "cpu" or "memory".
	Resource string
	From, To Size
	// Reason says what led to it, for the journal: the measurement and
	// its window.
	Reason string
	// Waiting says why nothing happens though the load asks for it (at
	// the ceiling, cooling down...), "" when nothing asks.
	Waiting string
	// OOMKills: the node's count of OOM kills this decision saw
	// (State.OOMKills next).
	OOMKills uint64
	// Warnings are what the load shows that no step can fix.
	Warnings []string

	counted bool // OOMKills is a sample's
}

// SampleEvery is how often the Controller reads a node: windows need
// four-fifths of their samples.
const SampleEvery = 15 * time.Second

// window is the samples of the last d, when there are enough of them -
// four-fifths of what SampleEvery gives.
func window(samples []Sample, now time.Time, d time.Duration) ([]Sample, bool) {
	from := now.Add(-d)
	i := len(samples)
	for i > 0 && !samples[i-1].At.Before(from) {
		i--
	}
	w := samples[i:]
	need := int(float64(d/SampleEvery) * 0.8)
	return w, len(w) >= max(need, 1)
}

// idleAt is HAProxy's idle time with n vCPUs, from a sample taken with
// s.CPUs: its busy time spread over n.
func idleAt(s Sample, n int) float64 {
	if s.IdlePercent < 0 || s.CPUs < 1 || n < 1 {
		return -1
	}
	return clamp(100 - (100-s.IdlePercent)*float64(s.CPUs)/float64(n))
}

// busyAt is the CPUs' busy time with n vCPUs.
func busyAt(s Sample, n int) float64 {
	if s.BusyPercent < 0 || s.CPUs < 1 || n < 1 {
		return -1
	}
	return clamp(s.BusyPercent * float64(s.CPUs) / float64(n))
}

// availableAt is the share of memory available with memMiB: the memory
// added or taken comes with it or goes from it.
func availableAt(s Sample, memMiB int) float64 {
	if s.MemTotalMiB <= 0 || memMiB <= 0 {
		return -1
	}
	return clamp(float64(s.MemAvailableMiB+memMiB-s.MemTotalMiB) / float64(memMiB) * 100)
}

func clamp(v float64) float64 { return math.Max(0, math.Min(100, v)) }

func average(w []Sample, f func(Sample) float64) (float64, bool) {
	sum, n := 0.0, 0
	for _, s := range w {
		if v := f(s); v >= 0 {
			sum += v
			n++
		}
	}
	if n == 0 || n < len(w)*4/5 {
		return 0, false
	}
	return sum / float64(n), true
}

// everyone reports whether f holds for every sample of w it knows.
func everyone(w []Sample, f func(Sample) (float64, bool)) bool {
	known := 0
	for _, s := range w {
		v, ok := f(s)
		if v < 0 {
			continue
		}
		known++
		if !ok {
			return false
		}
	}
	return known > 0 && known >= len(w)*4/5
}

// memoryUp is the memory a step up gives from cur: a quarter more, in
// whole steps, within max.
func memoryUp(cur, step, maxMiB int) int {
	add := roundUp(cur/4, step)
	return min(cur+max(add, step), maxMiB)
}

// memoryDown is the memory a step down leaves from cur: a fifth less, in
// whole steps, not below nominal.
func memoryDown(cur, step, nominal int) int {
	take := roundUp(cur/5, step)
	return max(cur-max(take, step), nominal)
}

func roundUp(v, step int) int {
	if step <= 0 {
		return v
	}
	return (v + step - 1) / step * step
}

// Decide says what the machine should do now: one step - memory before
// vCPUs on the way up (HAProxy reloads for a vCPU, two processes for a
// while), vCPUs before memory on the way down - or nothing, and why.
func Decide(p Policy, st State, samples []Sample, now time.Time) Decision {
	t := p.WithDefaults()
	cur := st.Size
	d := Decision{From: cur, To: cur}
	if len(samples) == 0 {
		d.Waiting = "no sample of the node yet"
		return d
	}
	last := samples[len(samples)-1]

	recent := 0
	for _, s := range st.Steps {
		if now.Sub(s) < time.Hour {
			recent++
		}
	}
	limited := recent >= t.MaxStepsPerHour
	cooling := !st.LastUp.IsZero() && now.Sub(st.LastUp) < t.Cooldown

	if last.MaxConn > 0 && float64(last.CurrConns) >= 0.8*float64(last.MaxConn) {
		d.Warnings = append(d.Warnings, fmt.Sprintf("HAProxy holds %d connections of its %d (maxconn): a limit of its configuration - more vCPUs or memory won't raise it", last.CurrConns, last.MaxConn))
	}

	// Asks for a step up, and why nothing happens if one can't be taken.
	type ask struct {
		resource string
		to       Size
		reason   string
		urgent   bool // an OOM kill: no cooldown
	}
	var asks []ask
	var waits []string

	// Memory: an OOM kill since the last decision - the first one counts
	// from the oldest sample, a node that rebooted from zero -, or too
	// little available for MemoryUpAfter.
	d.OOMKills, d.counted = last.OOMKills, true
	base := st.OOMKills
	switch {
	case !st.OOMCounted:
		base = samples[0].OOMKills
	case last.OOMKills < base:
		base = 0
	}
	if last.OOMKills > base {
		if a, why := memoryAsk(p, cur); why == "" {
			asks = append(asks, ask{"memory", a, fmt.Sprintf("the kernel killed a process for lack of memory (%d since boot)", last.OOMKills), true})
		} else {
			waits = append(waits, "an OOM kill, but "+why)
		}
	}
	if w, ok := window(samples, now, t.MemoryUpAfter); ok {
		if everyone(w, func(s Sample) (float64, bool) {
			v := availableAt(s, cur.MemoryMiB)
			return v, v < float64(t.MemoryUpAvailablePercent)
		}) {
			if a, why := memoryAsk(p, cur); why == "" {
				avg, _ := average(w, func(s Sample) float64 { return availableAt(s, cur.MemoryMiB) })
				asks = append(asks, ask{"memory", a, fmt.Sprintf("%.0f %% of the memory available on average over %s, below %d %%", avg, t.MemoryUpAfter, t.MemoryUpAvailablePercent), false})
			} else {
				waits = append(waits, fmt.Sprintf("memory below %d %% available, but %s", t.MemoryUpAvailablePercent, why))
			}
		}
	}

	// vCPUs: HAProxy busy, or the CPUs.
	if w, ok := window(samples, now, t.CPUUpAfter); ok {
		idle, idleOK := average(w, func(s Sample) float64 { return idleAt(s, cur.VCPUs) })
		busy, busyOK := average(w, func(s Sample) float64 { return busyAt(s, cur.VCPUs) })
		steal, stealOK := average(w, func(s Sample) float64 { return s.StealPercent })
		var reason string
		switch {
		case idleOK && idle < float64(t.CPUUpIdlePercent):
			reason = fmt.Sprintf("HAProxy idle %.0f %% of the time on average over %s, below %d %%", idle, t.CPUUpAfter, t.CPUUpIdlePercent)
		case busyOK && busy > float64(t.CPUUpBusyPercent):
			reason = fmt.Sprintf("the CPUs %.0f %% busy on average over %s, above %d %%", busy, t.CPUUpAfter, t.CPUUpBusyPercent)
		}
		if reason != "" {
			switch {
			case p.ThreadsPinned != "":
				waits = append(waits, fmt.Sprintf("%s, but HAProxy's configuration fixes its threads (%s): more vCPUs wouldn't be used", reason, p.ThreadsPinned))
			case cur.VCPUs >= p.Max.VCPUs:
				waits = append(waits, fmt.Sprintf("%s, but the vCPUs are at their maximum (%d)", reason, p.Max.VCPUs))
			case stealOK && steal > MaxStealPercent:
				waits = append(waits, fmt.Sprintf("%s, but the host is short of CPU: the vCPUs waited for it %.0f %% of the time (steal, above %d %%) - another vCPU would wait as well", reason, steal, MaxStealPercent))
			case availableAt(last, cur.MemoryMiB) < float64(t.MemoryUpAvailablePercent+5) && len(asks) == 0:
				waits = append(waits, fmt.Sprintf("%s, but too little memory is available for HAProxy's reload", reason))
			default:
				asks = append(asks, ask{"cpu", Size{cur.VCPUs + 1, cur.MemoryMiB}, reason, false})
			}
		}
	}

	if len(asks) > 0 {
		a := asks[0] // memory first
		switch {
		case limited:
			d.Waiting = fmt.Sprintf("%s - but %d steps were taken in the last hour, the most allowed", a.reason, recent)
		case cooling && !a.urgent:
			d.Waiting = fmt.Sprintf("%s - but the last step up was %s ago (cooldown %s)", a.reason, now.Sub(st.LastUp).Round(time.Second), t.Cooldown)
		default:
			d.Action, d.Resource, d.To, d.Reason = Up, a.resource, a.to, a.reason
		}
		return d
	}
	if len(waits) > 0 {
		d.Waiting = waits[0]
		return d
	}

	// Back towards nominal: the load low throughout ReturnAfter, since
	// the last step.
	w, ok := window(samples, now, t.ReturnAfter)
	if !ok || cur == p.Nominal {
		return d
	}
	for _, s := range st.Steps {
		if now.Sub(s) < t.ReturnAfter {
			return d // a step within the window: its samples are another size's
		}
	}
	if cur.VCPUs > p.Nominal.VCPUs {
		n := cur.VCPUs - 1
		if everyone(w, func(s Sample) (float64, bool) {
			v := idleAt(s, n)
			return v, v >= float64(t.CPUDownIdlePercent)
		}) && everyone(w, func(s Sample) (float64, bool) {
			v := busyAt(s, n)
			return v, v <= float64(100-t.CPUDownIdlePercent)
		}) {
			if limited {
				d.Waiting = "the load is low, but the most steps an hour were taken"
				return d
			}
			d.Action, d.Resource, d.To = Down, "cpu", Size{n, cur.MemoryMiB}
			d.Reason = fmt.Sprintf("HAProxy would stay idle %d %% of the time or more with %d vCPUs, all through the last %s", t.CPUDownIdlePercent, n, t.ReturnAfter)
			return d
		}
	}
	if cur.MemoryMiB > p.Nominal.MemoryMiB && p.MemoryStepMiB > 0 {
		m := memoryDown(cur.MemoryMiB, p.MemoryStepMiB, p.Nominal.MemoryMiB)
		if everyone(w, func(s Sample) (float64, bool) {
			v := availableAt(s, m)
			return v, v >= float64(t.MemoryDownAvailablePercent)
		}) {
			if limited {
				d.Waiting = "the load is low, but the most steps an hour were taken"
				return d
			}
			d.Action, d.Resource, d.To = Down, "memory", Size{cur.VCPUs, m}
			d.Reason = fmt.Sprintf("%d %% of the memory or more would stay available with %d MiB, all through the last %s", t.MemoryDownAvailablePercent, m, t.ReturnAfter)
		}
	}
	return d
}

// memoryAsk is the size a memory step up gives, or why there's none.
func memoryAsk(p Policy, cur Size) (Size, string) {
	switch {
	case p.MemoryStepMiB <= 0:
		return cur, "its memory can't change while it runs"
	case cur.MemoryMiB >= p.Max.MemoryMiB:
		return cur, fmt.Sprintf("the memory is at its maximum (%d MiB)", p.Max.MemoryMiB)
	}
	return Size{cur.VCPUs, memoryUp(cur.MemoryMiB, p.MemoryStepMiB, p.Max.MemoryMiB)}, ""
}

// Apply is st once d was carried out (or simulated) at now.
func (st State) Apply(d Decision, now time.Time) State {
	if d.counted {
		st.OOMKills, st.OOMCounted = d.OOMKills, true
	}
	if d.Action == None {
		return st
	}
	st.Size = d.To
	if d.Action == Up {
		st.LastUp = now
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
