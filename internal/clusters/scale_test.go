package clusters

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// samples are a reading every ScaleSampleEvery over d up to now.
func samples(now time.Time, d time.Duration, s ScaleSample) []ScaleSample {
	var out []ScaleSample
	for at := now.Add(-d); !at.After(now); at = at.Add(ScaleSampleEvery) {
		x := s
		x.At = at
		out = append(out, x)
	}
	return out
}

func TestScaleDecide(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	p := Autoscale{Mode: ScaleOn, MinMembers: 2, MaxMembers: 4}
	busy := ScaleSample{Members: 2, IdlePercent: 20, BusyPercent: 60, StealPercent: 1, ConnPercent: 10}
	quiet := ScaleSample{Members: 3, IdlePercent: 95, BusyPercent: 10, StealPercent: 0, ConnPercent: 5}

	d := Decide(p, ScaleState{Members: 2}, samples(now, 6*time.Minute, busy), now)
	if d.Action != ScaleOut || d.To != 3 || !strings.Contains(d.Reason, "HAProxy idle 20 % < 30 %") {
		t.Errorf("busy: %+v", d)
	}
	// Too few samples: nothing yet.
	if d := Decide(p, ScaleState{Members: 2}, samples(now, time.Minute, busy), now); d.Action != ScaleNone {
		t.Errorf("one minute: %+v", d)
	}
	// Judged at the count it simulates: three members carry it.
	if d := Decide(p, ScaleState{Members: 3}, samples(now, 6*time.Minute, ScaleSample{Members: 2, IdlePercent: 45, BusyPercent: 80, ConnPercent: 10}), now); d.Action != ScaleNone || d.Waiting != "" {
		t.Errorf("projected to 3: %+v", d)
	}
	// At the ceiling, cooling down, a host short of CPU: said, not done.
	if d := Decide(p, ScaleState{Members: 4}, samples(now, 6*time.Minute, ScaleSample{Members: 4, IdlePercent: 10, BusyPercent: 90}), now); d.Action != ScaleNone || !strings.Contains(d.Waiting, "max_members (4)") {
		t.Errorf("at max: %+v", d)
	}
	if d := Decide(p, ScaleState{Members: 2, LastOut: now.Add(-3 * time.Minute)}, samples(now, 6*time.Minute, busy), now); d.Action != ScaleNone || !strings.Contains(d.Waiting, "cooling down") {
		t.Errorf("cooldown: %+v", d)
	}
	stolen := busy
	stolen.StealPercent = 25
	if d := Decide(p, ScaleState{Members: 2}, samples(now, 6*time.Minute, stolen), now); d.Action != ScaleNone || len(d.Warnings) != 1 {
		t.Errorf("steal: %+v", d)
	}
	if d := Decide(p, ScaleState{Members: 2, Steps: []time.Time{now.Add(-50 * time.Minute), now.Add(-40 * time.Minute)}}, samples(now, 6*time.Minute, busy), now); d.Action != ScaleNone || !strings.Contains(d.Waiting, "2 steps in the last hour") {
		t.Errorf("steps per hour: %+v", d)
	}

	// In: quiet throughout, judged at one fewer.
	d = Decide(p, ScaleState{Members: 3}, samples(now, 31*time.Minute, quiet), now)
	if d.Action != ScaleIn || d.To != 2 {
		t.Errorf("quiet: %+v", d)
	}
	// Never below min_members; not when one fewer would be too busy.
	if d := Decide(p, ScaleState{Members: 2}, samples(now, 31*time.Minute, ScaleSample{Members: 2, IdlePercent: 95, BusyPercent: 5}), now); d.Action != ScaleNone {
		t.Errorf("at min: %+v", d)
	}
	if d := Decide(p, ScaleState{Members: 3}, samples(now, 31*time.Minute, ScaleSample{Members: 3, IdlePercent: 95, BusyPercent: 30}), now); d.Action != ScaleNone {
		t.Errorf("busy at 2 (45 %%): %+v", d)
	}
	if d := Decide(p, ScaleState{Members: 3, LastOut: now.Add(-20 * time.Minute)}, samples(now, 31*time.Minute, quiet), now); d.Action != ScaleNone || !strings.Contains(d.Waiting, "cooling down") {
		t.Errorf("in, cooling down: %+v", d)
	}

	st := ScaleState{Members: 2, Steps: []time.Time{now.Add(-2 * time.Hour)}}.Apply(ScaleDecision{Action: ScaleOut, From: 2, To: 3}, now)
	if st.Members != 3 || !st.LastOut.Equal(now) || len(st.Steps) != 1 {
		t.Errorf("applied: %+v", st)
	}
}

func TestAutoscaleValidate(t *testing.T) {
	ok := Autoscale{Mode: ScaleObserve, MinMembers: 2, MaxMembers: 4}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		a    Autoscale
		want string
	}{
		{Autoscale{Mode: "maybe", MinMembers: 2, MaxMembers: 4}, "mode"},
		{Autoscale{Mode: ScaleOn, MinMembers: 1, MaxMembers: 4}, "min_members"},
		{Autoscale{Mode: ScaleOn, MinMembers: 3, MaxMembers: 2}, "max_members"},
		{Autoscale{Mode: ScaleOn, MinMembers: 2, MaxMembers: 4, ScaleThresholds: ScaleThresholds{InIdlePercent: 25}}, "would leave as soon as it came"},
		{Autoscale{Mode: ScaleOn, MinMembers: 2, MaxMembers: 4, ScaleThresholds: ScaleThresholds{InBusyPercent: 80}}, "in_busy_percent"},
	} {
		if err := c.a.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.a, err, c.want)
		}
	}
}

// The policy's durations travel in seconds, as a machine's do; what
// v2026.10.24-5 and -6 stored in nanoseconds is still read.
func TestAutoscaleJSON(t *testing.T) {
	a := Autoscale{Mode: ScaleOn, MinMembers: 2, MaxMembers: 4, ScaleThresholds: ScaleThresholds{OutAfter: 3 * time.Minute, InCooldown: time.Hour, OutIdlePercent: 25}}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"out_after_seconds":180`) || !strings.Contains(s, `"in_cooldown_seconds":3600`) || strings.Contains(s, `"out_after":`) {
		t.Errorf("marshalled: %s", s)
	}
	var back Autoscale
	if err := json.Unmarshal(b, &back); err != nil || back != a {
		t.Errorf("round trip: %+v %v", back, err)
	}
	var old Autoscale
	if err := json.Unmarshal([]byte(`{"mode":"observe","min_members":2,"max_members":3,"out_after":120000000000}`), &old); err != nil || old.OutAfter != 2*time.Minute || old.Mode != ScaleObserve {
		t.Errorf("nanoseconds of before: %+v %v", old, err)
	}
	c := Cluster{Autoscale: &a}
	if b, _ := json.Marshal(c); !strings.Contains(string(b), `"autoscale":{"mode":"on"`) {
		t.Errorf("in a cluster: %s", b)
	}
}

// The bounds are said in seconds, as the fields are (v2026.10.25's lab:
// "1m0s to 1h0m0s").
func TestAutoscaleBoundsInSeconds(t *testing.T) {
	a := Autoscale{Mode: ScaleObserve, MinMembers: 2, MaxMembers: 4}
	a.OutAfter = 30 * time.Second
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "out_after_seconds: 60 to 3600 (seconds)") {
		t.Errorf("out_after 30 s: %v", err)
	}
}
