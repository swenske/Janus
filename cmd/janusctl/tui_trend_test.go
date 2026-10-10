package main

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/ring"
)

// withHistory gives s n rounds two seconds apart up to tuiNow, sess(i)
// sessions per second at round i (the other metrics follow it); the
// rounds in [gapFrom, gapTo) didn't answer, so the next one has no
// span and no rate - as derive makes them.
func withHistory(s *sampler, n int, sess func(i int) float64, gapFrom, gapTo int) {
	s.history = ring.New[point](tuiHistory)
	const every = 2 * time.Second
	fresh := true
	for i := 0; i < n; i++ {
		if i >= gapFrom && i < gapTo {
			fresh = true
			continue
		}
		v := sess(i)
		p := point{at: tuiNow.Add(-time.Duration(n-1-i) * every), span: every, cpu: 5 + v/5, load1: v / 200, memUsed: 1<<30 + uint64(v)<<22, memTotal: 4 << 30,
			net: map[string]netRate{"lo": {rx: 1e9, tx: 1e9}, "eth0": {rx: v * 2000, tx: v * 8000}, "eth0.20": {rx: 1e9, tx: 1e9}},
			hap: hapPoint{ok: true, version: "3.2.9", conns: uint32(v * 3), sessRate: uint32(v), reqRate: v * 1.5}}
		if fresh {
			p.span, p.cpu, p.hap.reqRate, fresh = 0, math.NaN(), math.NaN(), false
			p.net = map[string]netRate{"eth0": {rx: math.NaN(), tx: math.NaN()}}
		}
		s.history.Append(p)
		s.last = p
	}
}

// trendFleet is four nodes: a busy master, one that doesn't answer,
// one not asked yet, and a quiet backup that was away for 40 s.
func trendFleet(t *testing.T, a *app) {
	t.Helper()
	busy := testSampler(t, a, "lgslbpub01")
	withHistory(busy, 160, func(i int) float64 { return 150 + 120*math.Sin(float64(i)/12) + float64(i) }, -1, -1)
	down := newSampler(tuiTarget{name: "lgslbpub02", address: "172.16.1.152:9505", dial: func() (*grpc.ClientConn, error) {
		return nil, errors.New("dial tcp 172.16.1.152:9505: connect: connection refused")
	}}, a.wake, &a.interval, &a.paused)
	if _, err := down.connection(); err == nil {
		t.Fatal("the dial must fail")
	}
	down.rounds = 1
	fresh := newSampler(tuiTarget{name: "lgslbpub03", address: "172.16.1.153:9505"}, a.wake, &a.interval, &a.paused)
	quiet := testSampler(t, a, "lgslbpub04")
	quiet.target.address = "172.16.1.154:9505"
	quiet.slow.network.TrialPending = false
	quiet.slow.vrrp = &janusv1alpha1.VRRPStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_RUNNING, Instances: []*janusv1alpha1.VRRPInstance{
		{Name: "VI_1", Role: "BACKUP", Interface: "eth0", VirtualRouterId: 51, Priority: 100, EffectivePriority: 100}}}
	withHistory(quiet, 160, func(i int) float64 { return 6 + 4*math.Sin(float64(i)/7) }, 40, 60)
	a.samplers = []*sampler{busy, down, fresh, quiet}
	a.fleet = newFleetScreen()
	a.screen = a.fleet
}

func TestFleetTrendGolden(t *testing.T) {
	a := testApp(t)
	trendFleet(t, a)
	// Wide: every column, a long trend; log scale - the quiet node shows.
	a.key("y")
	golden(t, "tui-fleet-trend-160x24.txt", a.render(160, 24))
}

func TestTrendSlots(t *testing.T) {
	const every = 2 * time.Second
	rounds := func(vals ...float64) []trendPoint { // two seconds apart, the last at tuiNow
		out := make([]trendPoint, len(vals))
		for i, v := range vals {
			out[i] = trendPoint{at: tuiNow.Add(-time.Duration(len(vals)-1-i) * every), span: every, v: v}
		}
		return out
	}
	str := func(vs []float64) string { return strings.ReplaceAll(fmt.Sprint(vs), "NaN", "-") }

	// A slot a round: each value in its own slot, falling or rising.
	if got := str(trendSlots(rounds(3, 2, 1), tuiNow, 6*time.Second, 3, 0)); got != "[3 2 1]" {
		t.Errorf("a slot a round = %s, want [3 2 1]", got)
	}
	// Slots finer than the interval: a round covers every slot of its
	// span - a step, no holes.
	if got := str(trendSlots(rounds(3, 2, 1), tuiNow, 6*time.Second, 6, 0)); got != "[3 3 2 2 1 1]" {
		t.Errorf("fine slots = %s", got)
	}
	// Coarser slots: the largest value of the rounds in each.
	if got := str(trendSlots(rounds(1, 5, 2, 1, 0, 7), tuiNow, 12*time.Second, 2, 0)); got != "[5 7]" {
		t.Errorf("coarse slots = %s, want [5 7]", got)
	}
	// Before the history: nothing known.
	if got := str(trendSlots(rounds(4), tuiNow, 8*time.Second, 4, 0)); got != "[- - - 4]" {
		t.Errorf("a short history = %s", got)
	}
	// The newest round holds until the next is due...
	pts := rounds(1, 2)
	if got := str(trendSlots(pts, tuiNow.Add(2*time.Second), 8*time.Second, 4, 3*time.Second)); got != "[- 1 2 2]" {
		t.Errorf("held = %s, want [- 1 2 2]", got)
	}
	// ...not longer (3 s here): a node that stopped answering leaves a gap.
	if got := str(trendSlots(pts, tuiNow.Add(8*time.Second), 12*time.Second, 6, 3*time.Second)); got != "[1 2 2 2 - -]" {
		t.Errorf("stopped = %s, want [1 2 2 2 - -]", got)
	}
	// A round after an outage has no span: the outage stays empty, and a
	// NaN (its rate) draws nothing.
	pts = []trendPoint{{at: tuiNow.Add(-10 * time.Second), span: every, v: 3}, {at: tuiNow, v: math.NaN()}, {at: tuiNow, v: 4}}
	if got := str(trendSlots(pts, tuiNow, 12*time.Second, 6, 0)); got != "[3 - - - - 4]" {
		t.Errorf("after an outage = %s, want [3 - - - - 4]", got)
	}
	if got := trendSlots(rounds(1), tuiNow, time.Minute, 0, 0); len(got) != 0 {
		t.Errorf("no slots = %v", got)
	}
}

func TestFleetTrendLayout(t *testing.T) {
	s := newFleetScreen()
	short := []string{"lgslbpub01", "lgslbpub02"}
	long := []string{"a-very-long-node-name-indeed-and-more", "b"}
	for _, c := range []struct {
		w       int
		names   []string
		dropped []int
		nodeW   int
	}{
		{w: 158, names: short, nodeW: 10},
		{w: 118, names: short, dropped: []int{fleetAddress}, nodeW: 10},
		{w: 98, names: short, dropped: []int{fleetAddress, fleetVersion}, nodeW: 10},
		{w: 78, names: short, dropped: fleetDropOrder, nodeW: 10},
		{w: 78, names: long, dropped: fleetDropOrder, nodeW: 10}, // narrowed from 24
		{w: 300, names: long, nodeW: fleetNodeMax},
	} {
		cols, shown, trendW := s.layout(c.w, c.names)
		if trendW < trendMinWidth || shown[len(shown)-1] != fleetTrend || cols[len(cols)-1].Width != trendW {
			t.Errorf("%d cells: trend %d wide, columns %v", c.w, trendW, shown)
			continue
		}
		used := len(cols) - 1
		for _, col := range cols {
			used += col.Width
		}
		if used != c.w {
			t.Errorf("%d cells: the columns take %d", c.w, used)
		}
		if cols[0].Width != c.nodeW {
			t.Errorf("%d cells, %q: NODE %d wide, want %d", c.w, c.names[0], cols[0].Width, c.nodeW)
		}
		for _, d := range c.dropped {
			for _, col := range shown {
				if col == d {
					t.Errorf("%d cells: %s should make room", c.w, fleetColumns[d].Title)
				}
			}
		}
		if len(shown) != len(fleetColumns)-len(c.dropped) {
			t.Errorf("%d cells: %d columns shown, %d dropped", c.w, len(shown), len(c.dropped))
		}
	}
	// No room even so: every column, no trend - as before the trend.
	if cols, shown, trendW := s.layout(60, long); trendW != 0 || len(shown) != fleetTrend || cols[0].Width != 0 {
		t.Errorf("too narrow: trend %d, %d columns, NODE %d", trendW, len(shown), cols[0].Width)
	}
	s.trendOff = true
	if _, shown, trendW := s.layout(300, short); trendW != 0 || len(shown) != fleetTrend {
		t.Errorf("trend off: trend %d, columns %v", trendW, shown)
	}
}

func TestFleetTrendKeys(t *testing.T) {
	a := testApp(t)
	trendFleet(t, a)
	header := func() string { return a.render(120, 40).Lines()[2] }
	if h := header(); !strings.Contains(h, "SESS/S 5m max 384") || strings.Contains(h, "ADDRESS") {
		t.Errorf("by default: sessions over 5 minutes, linear, ADDRESS making room: %q", h)
	}
	// The window.
	a.key("w")
	if h := header(); !strings.Contains(h, "SESS/S 15m") || a.status.text != "trend over the last 15 minutes" {
		t.Errorf("w: %q, said %q", h, a.status.text)
	}
	a.key("w")
	a.key("w")
	if a.status.text != "trend over the last minute" {
		t.Errorf("w to 1m said %q", a.status.text)
	}
	a.key("w")
	if a.fleet.window != trendDefaultWindow {
		t.Errorf("w goes round: window %d", a.fleet.window)
	}
	// The scale: linear, log, linear again - 0-100 % only for percentages.
	a.key("y")
	if h := header(); !strings.Contains(h, "SESS/S 5m log max 384") || a.status.text != "trend scale: logarithmic, the same for every node" {
		t.Errorf("y: %q, said %q", h, a.status.text)
	}
	a.key("y")
	if a.fleet.scale != scaleLinear {
		t.Errorf("no 0-100 %% for sessions: scale %v", a.fleet.scale)
	}
	// The metrics, in order, then none.
	want := []string{"REQ/S 5m max 577", "CONNS 5m max 1.2k", "NET 5m max 3.7MiB/s", "CPU 5m max 82%", "MEM 5m max 62%"}
	for _, w := range want {
		a.key("m")
		if h := header(); !strings.Contains(h, w) {
			t.Errorf("m: %q lacks %q", h, w)
		}
		if got, tone := a.fleet.table.Tone(0, len(a.fleet.shown)-1), trendMetrics[a.fleet.metric].tone; got != tone {
			t.Errorf("%s: the trend drawn in %+v, want %+v", w, got, tone)
		}
	}
	a.key("y")
	a.key("y")
	if h := header(); !strings.Contains(h, "MEM 5m 0-100%") {
		t.Errorf("memory's 0-100 %% scale: %q", h)
	}
	a.key("m")
	if h := header(); strings.Contains(h, "SESS/S 5m") || !strings.Contains(h, "ADDRESS") || a.status.text != "trend hidden - m shows it again" {
		t.Errorf("m past the last metric hides the trend: %q, said %q", h, a.status.text)
	}
	a.key("m")
	if h := header(); !strings.Contains(h, "SESS/S 5m max 384") || a.fleet.scale != scaleLinear {
		t.Errorf("m again: sessions, back to a scale they have: %q, scale %v", h, a.fleet.scale)
	}
	// Sorting by the trend: the latest value, largest first; an
	// unreachable node knows none.
	for a.fleet.sort != fleetTrend {
		a.key("s")
	}
	if a.status.text != "nodes by sessions per second" {
		t.Errorf("s on the trend says %q", a.status.text)
	}
	a.render(120, 40)
	if a.samplers[a.fleet.order[0]].target.name != "lgslbpub01" || a.samplers[a.fleet.order[1]].target.name != "lgslbpub04" {
		t.Errorf("by sessions: %v", a.fleet.order)
	}
	a.key("?")
	if !strings.Contains(strings.Join(a.render(120, 40).Lines(), "\n"), "fleet trend: metric, time window, scale") {
		t.Error("the help lists the trend's keys")
	}
	a.key("?")
	// Paused, the axis stands still.
	a.key("p")
	later := tuiNow.Add(time.Minute)
	a.now = func() time.Time { return later }
	if !a.trendEnd().Equal(tuiNow) {
		t.Errorf("paused: the trend ends %v, want %v", a.trendEnd(), tuiNow)
	}
	a.key("p")
	if !a.trendEnd().Equal(later) {
		t.Errorf("resumed: the trend ends %v", a.trendEnd())
	}
}

func TestFmtCount(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 7: "7", 3.5: "3.5", 412: "412", 1234: "1.2k", 45_600: "46k", 1_300_000: "1.3M", math.NaN(): "-"} {
		if got := fmtCount(v); got != want {
			t.Errorf("fmtCount(%v) = %q, want %q", v, got, want)
		}
	}
	if got := trendTitle(trendMetrics[0], 5*time.Minute, scaleLog, 412, 12); got != "SESS/S 5m" {
		t.Errorf("a narrow header keeps the metric and the window: %q", got)
	}
	if got := trendTitle(trendMetrics[0], 5*time.Minute, scaleLog, 412, 18); got != "SESS/S 5m max 412" {
		t.Errorf("then the largest value: %q", got)
	}
}
