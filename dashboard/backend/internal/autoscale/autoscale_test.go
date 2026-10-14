package autoscale

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

var policy = Policy{
	Nominal:       Size{2, 1024},
	Max:           Size{4, 4096},
	MemoryStepMiB: 128,
}

// load is a node's load as a function of time.
type load func(at time.Time) Sample

// run samples l every SampleEvery over d from start, and returns the
// samples - each one taken at the size the machine has (st.Size), as a
// real machine resized by each decision would report.
func samplesFor(start time.Time, d time.Duration, cpus, mem int, l load) []Sample {
	var out []Sample
	for at := start; !at.After(start.Add(d)); at = at.Add(SampleEvery) {
		s := l(at)
		s.At, s.CPUs, s.MemTotalMiB = at, cpus, mem
		out = append(out, s)
	}
	return out
}

func steady(idle, busy float64, availMiB int) load {
	return func(time.Time) Sample { return Sample{IdlePercent: idle, BusyPercent: busy, MemAvailableMiB: availMiB, MaxConn: 1000} }
}

func TestDefaultsValidate(t *testing.T) {
	if err := (Thresholds{}).Validate(); err != nil {
		t.Fatalf("the defaults: %v", err)
	}
	for _, bad := range []Thresholds{
		{CPUUpIdlePercent: 60}, // above the down threshold (50)
		{MemoryUpAvailablePercent: 45},
		{CPUDownIdlePercent: 20, CPUUpIdlePercent: 10, CPUUpBusyPercent: 75}, // after a step down 80 % busy > 75
		{ReturnAfter: time.Minute},
		{MaxStepsPerHour: 100},
		{CPUUpBusyPercent: 100},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestCPUUp(t *testing.T) {
	st := State{Size: policy.Nominal}
	// HAProxy idle 20 % for 5 minutes: a vCPU more.
	s := samplesFor(t0, 5*time.Minute, 2, 1024, steady(20, 60, 600))
	d := Decide(policy, st, s, t0.Add(5*time.Minute))
	if d.Action != Up || d.Resource != "cpu" || d.To != (Size{3, 1024}) || !strings.Contains(d.Reason, "HAProxy idle 20 %") {
		t.Fatalf("decision %+v", d)
	}
	// For 3 minutes only: nothing yet.
	s = samplesFor(t0, 3*time.Minute, 2, 1024, steady(20, 60, 600))
	if d := Decide(policy, st, s, t0.Add(3*time.Minute)); d.Action != None {
		t.Errorf("a step up after 3 minutes: %+v", d)
	}
	// The CPUs busy (HAProxy unknown): up too.
	s = samplesFor(t0, 5*time.Minute, 2, 1024, steady(-1, 85, 600))
	if d := Decide(policy, st, s, t0.Add(5*time.Minute)); d.Action != Up || !strings.Contains(d.Reason, "CPUs 85 % busy") {
		t.Errorf("busy CPUs: %+v", d)
	}
	// Threads fixed by the configuration: why not, no step.
	p := policy
	p.ThreadsPinned = "nbthread"
	s = samplesFor(t0, 5*time.Minute, 2, 1024, steady(20, 60, 600))
	if d := Decide(p, st, s, t0.Add(5*time.Minute)); d.Action != None || !strings.Contains(d.Waiting, "nbthread") {
		t.Errorf("pinned threads: %+v", d)
	}
	// At the maximum.
	st4 := State{Size: Size{4, 1024}}
	s = samplesFor(t0, 5*time.Minute, 4, 1024, steady(5, 95, 600))
	if d := Decide(policy, st4, s, t0.Add(5*time.Minute)); d.Action != None || !strings.Contains(d.Waiting, "maximum (4)") {
		t.Errorf("at the maximum: %+v", d)
	}
}

func TestMemoryFirst(t *testing.T) {
	st := State{Size: policy.Nominal}
	// HAProxy busy and memory short: memory first.
	s := samplesFor(t0, 5*time.Minute, 2, 1024, steady(10, 90, 150))
	d := Decide(policy, st, s, t0.Add(5*time.Minute))
	if d.Action != Up || d.Resource != "memory" || d.To != (Size{2, 1280}) {
		t.Fatalf("decision %+v", d)
	}
	// Memory steps: a quarter more, in steps, within the maximum.
	if got := memoryUp(1024, 128, 4096); got != 1280 {
		t.Errorf("memoryUp(1024) = %d", got)
	}
	if got := memoryUp(1024, 512, 4096); got != 1536 {
		t.Errorf("memoryUp(1024, Proxmox's 512) = %d", got)
	}
	if got := memoryUp(3900, 128, 4096); got != 4096 {
		t.Errorf("memoryUp near the maximum = %d", got)
	}
	if got := memoryDown(2048, 128, 1024); got != 1536 {
		t.Errorf("memoryDown(2048) = %d", got)
	}
	if got := memoryDown(1100, 128, 1024); got != 1024 {
		t.Errorf("memoryDown near nominal = %d", got)
	}
}

func TestOOMKill(t *testing.T) {
	st := State{Size: policy.Nominal, OOMKills: 2, OOMCounted: true}
	s := samplesFor(t0, time.Minute, 2, 1024, steady(80, 10, 600))
	s[len(s)-1].OOMKills = 3
	d := Decide(policy, st, s, t0.Add(time.Minute))
	if d.Action != Up || d.Resource != "memory" || !strings.Contains(d.Reason, "killed a process") {
		t.Fatalf("an OOM kill: %+v", d)
	}
	// Even right after a step up: no cooldown for it.
	st.LastUp = t0.Add(30 * time.Second)
	if d := Decide(policy, st, s, t0.Add(time.Minute)); d.Action != Up {
		t.Errorf("an OOM kill cooling down: %+v", d)
	}
	// Seen: not again.
	st = st.Apply(d, t0.Add(time.Minute))
	if d := Decide(policy, st, s, t0.Add(time.Minute+SampleEvery)); d.Resource == "memory" && strings.Contains(d.Reason, "killed") {
		t.Errorf("the same kill twice: %+v", d)
	}
	// Kills before the first decision: history, not a reason.
	fresh := State{Size: policy.Nominal}
	old := samplesFor(t0, time.Minute, 2, 1024, steady(80, 10, 600))
	for i := range old {
		old[i].OOMKills = 7
	}
	if d := Decide(policy, fresh, old, t0.Add(time.Minute)); d.Action != None {
		t.Errorf("old kills: %+v", d)
	}
}

func TestCooldownAndHourlyLimit(t *testing.T) {
	st := State{Size: Size{3, 1024}, LastUp: t0.Add(2 * time.Minute), Steps: []time.Time{t0.Add(2 * time.Minute)}}
	s := samplesFor(t0, 5*time.Minute, 3, 1024, steady(10, 90, 600))
	d := Decide(policy, st, s, t0.Add(5*time.Minute))
	if d.Action != None || !strings.Contains(d.Waiting, "cooldown") {
		t.Errorf("cooling down: %+v", d)
	}
	st = State{Size: Size{3, 1024}}
	for i := range 6 {
		st.Steps = append(st.Steps, t0.Add(-time.Duration(50-i)*time.Minute))
	}
	if d := Decide(policy, st, s, t0.Add(5*time.Minute)); d.Action != None || !strings.Contains(d.Waiting, "6 steps") {
		t.Errorf("six steps this hour: %+v", d)
	}
}

func TestBackToNominal(t *testing.T) {
	// 3 vCPUs, quiet for 30 minutes: HAProxy idle 85 % - with 2 it
	// would be 77.5 %, above 50: one less.
	st := State{Size: Size{3, 2048}}
	s := samplesFor(t0, 30*time.Minute, 3, 2048, steady(85, 10, 1500))
	d := Decide(policy, st, s, t0.Add(30*time.Minute))
	if d.Action != Down || d.Resource != "cpu" || d.To != (Size{2, 2048}) {
		t.Fatalf("decision %+v", d)
	}
	// Quiet for 20 minutes only: nothing.
	s20 := samplesFor(t0, 20*time.Minute, 3, 2048, steady(85, 10, 1500))
	if d := Decide(policy, st, s20, t0.Add(20*time.Minute)); d.Action != None {
		t.Errorf("quiet 20 minutes: %+v", d)
	}
	// Idle 60 %: 40 % with one fewer - stays.
	busy := samplesFor(t0, 30*time.Minute, 3, 2048, steady(60, 30, 1500))
	if d := Decide(policy, st, busy, t0.Add(30*time.Minute)); d.Resource == "cpu" {
		t.Errorf("would shrink into its up threshold: %+v", d)
	}
	// vCPUs at nominal: memory next - 1500 of 2048 available, 1036 of
	// 1536 with a fifth less: 67 %.
	st = State{Size: Size{2, 2048}}
	s = samplesFor(t0, 30*time.Minute, 2, 2048, steady(85, 10, 1500))
	d = Decide(policy, st, s, t0.Add(30*time.Minute))
	if d.Action != Down || d.Resource != "memory" || d.To != (Size{2, 1536}) {
		t.Fatalf("memory down: %+v", d)
	}
	// A step within the window: its samples are another size's - wait.
	st.Steps = []time.Time{t0.Add(10 * time.Minute)}
	if d := Decide(policy, st, s, t0.Add(30*time.Minute)); d.Action != None {
		t.Errorf("a step 20 minutes ago: %+v", d)
	}
	// At nominal: nothing to give back.
	st = State{Size: policy.Nominal}
	s = samplesFor(t0, 30*time.Minute, 2, 1024, steady(95, 2, 900))
	if d := Decide(policy, st, s, t0.Add(30*time.Minute)); d.Action != None || d.Waiting != "" {
		t.Errorf("at nominal: %+v", d)
	}
}

// TestObservationSimulates: the machine stays at its nominal size, the
// policy's size is simulated - and stops growing where it would have
// been enough.
func TestObservationSimulates(t *testing.T) {
	st := State{Size: policy.Nominal}
	// The real machine, 2 vCPUs, HAProxy idle 10 % for an hour.
	real := samplesFor(t0, time.Hour, 2, 1024, steady(10, 85, 600))
	at := t0.Add(5 * time.Minute)
	var sizes []int
	for ; !at.After(t0.Add(time.Hour)); at = at.Add(SampleEvery) {
		var seen []Sample
		for _, s := range real {
			if !s.At.After(at) {
				seen = append(seen, s)
			}
		}
		d := Decide(policy, st, seen, at)
		if d.Action == Up {
			sizes = append(sizes, d.To.VCPUs)
		}
		st = st.Apply(d, at)
	}
	// 90 % busy over 2 vCPUs: 3 give 40 % idle - above 30: 3 is
	// enough, it stops there.
	if len(sizes) != 1 || sizes[0] != 3 || st.Size.VCPUs != 3 {
		t.Errorf("simulated steps %v, ended at %+v", sizes, st.Size)
	}
}

func TestNotEnoughSamples(t *testing.T) {
	st := State{Size: policy.Nominal}
	if d := Decide(policy, st, nil, t0); d.Waiting == "" {
		t.Error("no samples: no reason given")
	}
	// Half the samples of 5 minutes (a node that answers one time in
	// two): not enough to decide.
	var s []Sample
	for i, x := range samplesFor(t0, 5*time.Minute, 2, 1024, steady(10, 90, 600)) {
		if i%2 == 0 {
			s = append(s, x)
		}
	}
	if d := Decide(policy, st, s, t0.Add(5*time.Minute)); d.Action != None {
		t.Errorf("a step on half the samples: %+v", d)
	}
}

func TestWarnings(t *testing.T) {
	s := samplesFor(t0, time.Minute, 2, 1024, steady(80, 10, 600))
	s[len(s)-1].CurrConns = 900
	d := Decide(policy, State{Size: policy.Nominal}, s, t0.Add(time.Minute))
	if len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "maxconn") {
		t.Errorf("warnings %v", d.Warnings)
	}
}

func TestApply(t *testing.T) {
	st := State{Size: policy.Nominal, Steps: []time.Time{t0.Add(-2 * time.Hour)}}
	st = st.Apply(Decision{Action: Up, To: Size{3, 1024}, counted: true, OOMKills: 4}, t0)
	if st.Size != (Size{3, 1024}) || !st.LastUp.Equal(t0) || len(st.Steps) != 1 || st.OOMKills != 4 || !st.OOMCounted {
		t.Errorf("after a step up: %+v", st)
	}
	st = st.Apply(Decision{}, t0.Add(time.Minute)) // no sample: the count stays
	if st.OOMKills != 4 {
		t.Errorf("a decision without samples moved the count: %+v", st)
	}
}
