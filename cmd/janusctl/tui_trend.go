package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/termui"
)

// The fleet's trend column: one metric over a time window, a sparkline
// per node (a block a slot), every node against the same scale - a node that takes ten
// times the traffic of another draws ten times higher. m, w and y
// change the metric, the window and the scale.

// trendMetric is what the trend column can draw.
type trendMetric struct {
	key    string // in tui.json
	name   string // said in the footer when chosen
	title  string // the column's header
	pct    bool   // a percentage: the 0-100 % scale is offered
	tone   termui.Style
	format func(float64) string
	// pick is the metric at a point; phys tells the node's physical
	// interfaces (a VLAN's traffic is its parent's too).
	pick func(p *point, phys func(string) bool) float64
}

var trendMetrics = []trendMetric{
	{key: "sess", name: "sessions per second", title: "SESS/S", tone: styleProcess, format: fmtCount, pick: func(p *point, _ func(string) bool) float64 {
		if !p.hap.ok {
			return math.NaN()
		}
		return float64(p.hap.sessRate)
	}},
	{key: "req", name: "requests per second", title: "REQ/S", tone: styleProcess, format: fmtCount, pick: func(p *point, _ func(string) bool) float64 {
		if !p.hap.ok {
			return math.NaN()
		}
		return p.hap.reqRate
	}},
	{key: "conns", name: "connections", title: "CONNS", tone: styleProcess, format: fmtCount, pick: func(p *point, _ func(string) bool) float64 {
		if !p.hap.ok {
			return math.NaN()
		}
		return float64(p.hap.conns)
	}},
	{key: "net", name: "network traffic, in + out", title: "NET", tone: termui.Style{FG: termui.ColorDownload}, format: fmtRate, pick: func(p *point, phys func(string) bool) float64 {
		sum, seen := 0.0, false
		for name, r := range p.net {
			if !phys(name) {
				continue
			}
			if math.IsNaN(r.rx) || math.IsNaN(r.tx) {
				return math.NaN()
			}
			sum, seen = sum+r.rx+r.tx, true
		}
		if !seen {
			return math.NaN()
		}
		return sum
	}},
	{key: "cpu", name: "CPU", title: "CPU", pct: true, tone: termui.Style{FG: termui.GradCPU.At(0.5)}, format: fmtPct, pick: func(p *point, _ func(string) bool) float64 { return p.cpu }},
	{key: "mem", name: "memory", title: "MEM", pct: true, tone: termui.Style{FG: termui.GradUsed.At(0.5)}, format: fmtPct, pick: func(p *point, _ func(string) bool) float64 {
		if p.memTotal == 0 {
			return math.NaN()
		}
		return float64(p.memUsed) / float64(p.memTotal) * 100
	}},
}

// styleProcess is HAProxy's traffic: its graph's colours halfway.
var styleProcess = termui.Style{FG: termui.GradProcess.At(0.5)}

// trendWindows are the spans w steps through. The history holds
// tuiHistory rounds: 30 minutes at the default interval.
var trendWindows = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute}

const trendDefaultWindow = 1 // 5 minutes

type trendScale int

const (
	scaleLinear trendScale = iota // 0 to the fleet's largest value in the window
	scaleLog                      // the same, logarithmic: a quiet node still shows beside a busy one
	scaleFull                     // 0 to 100 %, percentages only
)

// trendMetricIndex is the metric whose key is k.
func trendMetricIndex(k string) (int, bool) {
	for i, m := range trendMetrics {
		if m.key == k {
			return i, true
		}
	}
	return 0, false
}

// trendWindowIndex is the window fmtWindow writes as s, -1 for none.
func trendWindowIndex(s string) int {
	for i, w := range trendWindows {
		if fmtWindow(w) == s {
			return i
		}
	}
	return -1
}

// key is the scale in tui.json.
func (sc trendScale) key() string {
	return [...]string{scaleLinear: "linear", scaleLog: "log", scaleFull: "full"}[sc]
}

// scaleNamed is the scale whose key is k.
func scaleNamed(k string) (trendScale, bool) {
	for _, sc := range []trendScale{scaleLinear, scaleLog, scaleFull} {
		if sc.key() == k {
			return sc, true
		}
	}
	return scaleLinear, false
}

func (sc trendScale) String() string {
	switch sc {
	case scaleLog:
		return "logarithmic"
	case scaleFull:
		return "0 to 100 %"
	}
	return "linear"
}

// trendMinWidth is the narrowest trend column worth drawing (a slot a
// cell); the fleet drops other columns to give it this much.
const trendMinWidth = 16

// physicalOf is the test for the interfaces a node's traffic is counted
// on: every one but the loopback and the VLANs (their parent carries
// their traffic too) - by the kinds its network status reports, else
// by name (lo, parent.id).
func physicalOf(v *nodeView) func(string) bool {
	kinds := map[string]string{}
	for _, i := range v.slow.network.GetInterfaces() {
		kinds[i.GetName()] = i.GetKind()
	}
	return func(name string) bool {
		if k, ok := kinds[name]; ok {
			return k != "loopback" && k != "vlan"
		}
		return name != "lo" && !strings.Contains(name, ".")
	}
}

// trendSlots spreads points over n slots of window ending at end, each
// slot the largest value of the points covering it. A point covers the
// span its rates were measured over, and the newest one holds for hold
// more, until the next round is due: a slot finer than the interval is
// no gap, a node that stopped answering is one.
func trendSlots(pts []trendPoint, end time.Time, window time.Duration, n int, hold time.Duration) []float64 {
	out := make([]float64, max(n, 0))
	for i := range out {
		out[i] = math.NaN()
	}
	if n <= 0 || window <= 0 {
		return out
	}
	start := end.Add(-window)
	slot := float64(window) / float64(n)
	pos := func(t time.Time) float64 { return float64(t.Sub(start)) / slot }
	for j, p := range pts {
		if math.IsNaN(p.v) {
			continue
		}
		to := p.at
		if j == len(pts)-1 {
			to = p.at.Add(hold)
		}
		if to.After(end) {
			to = end
		}
		// (from, to]: a point ending on a slot's edge is in the slot
		// before it; one without a span, in the slot its time ends.
		last := int(math.Ceil(pos(to))) - 1
		first := min(int(math.Floor(pos(p.at.Add(-p.span)))), last)
		for i := max(first, 0); i <= min(last, n-1); i++ {
			if math.IsNaN(out[i]) || p.v > out[i] {
				out[i] = p.v
			}
		}
	}
	return out
}

// trendColumn is the trend column for one frame: each node's sparkline
// and latest value, the header saying the metric, the window and the
// scale.
type trendColumn struct {
	title  string
	sparks []string  // by sampler
	latest []float64 // by sampler, NaN unknown: what the column sorts by
}

// trendOf draws metric over window for every node, in width cells
// ending at end.
func trendOf(samplers []*sampler, views []nodeView, metric trendMetric, window time.Duration, sc trendScale, width int, end time.Time, interval time.Duration, sym termui.GraphSymbols) trendColumn {
	t := trendColumn{sparks: make([]string, len(samplers)), latest: make([]float64, len(samplers))}
	slots := make([][]float64, len(samplers))
	top := math.NaN()
	for i, s := range samplers {
		phys := physicalOf(&views[i])
		pick := func(p *point) float64 { return metric.pick(p, phys) }
		t.latest[i] = math.NaN()
		if views[i].reachable && !views[i].last.at.IsZero() {
			t.latest[i] = pick(&views[i].last)
		}
		pts := s.trend(end.Add(-window), pick)
		hold := interval
		if n := len(pts); n > 0 {
			hold = max(pts[n-1].span, interval)
		}
		slots[i] = trendSlots(pts, end, window, width, hold+hold/2)
		for _, v := range slots[i] {
			if !math.IsNaN(v) && (math.IsNaN(top) || v > top) {
				top = v
			}
		}
	}
	if sc == scaleFull && !metric.pct {
		sc = scaleLinear
	}
	maxV := top
	switch {
	case sc == scaleFull:
		maxV = 100
	case math.IsNaN(top) || top <= 0:
		maxV = 1
	}
	for i := range slots {
		values := slots[i]
		scaleMax := maxV
		if sc == scaleLog {
			values = make([]float64, len(slots[i]))
			for j, v := range slots[i] {
				values[j] = math.Log1p(max(v, 0))
				if math.IsNaN(v) {
					values[j] = math.NaN()
				}
			}
			scaleMax = math.Log1p(maxV)
		}
		t.sparks[i] = termui.Spark(values, scaleMax, sym)
	}
	t.title = trendTitle(metric, window, sc, top, width)
	return t
}

// trendTitle is the column's header, as much of "SESS/S 5m log max 412"
// as fits in width.
func trendTitle(metric trendMetric, window time.Duration, sc trendScale, top float64, width int) string {
	head := metric.title + " " + fmtWindow(window)
	var scale string
	switch {
	case sc == scaleFull && metric.pct:
		scale = "0-100%"
	case math.IsNaN(top):
	case sc == scaleLog:
		scale = "log max " + metric.format(top)
	default:
		scale = "max " + metric.format(top)
	}
	for _, s := range []string{head + " " + scale, head + " " + strings.TrimPrefix(scale, "log "), head, metric.title} {
		s = strings.TrimSpace(s)
		if len([]rune(s)) <= width {
			return s
		}
	}
	return termui.Truncate(metric.title, width)
}

// fmtWindow is a window as the header says it: 1m, 5m, 15m, 30m.
func fmtWindow(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return d.String()
}

// fmtCount is a count or a rate in at most five characters: 7, 3.5,
// 412, 1.2k, 45k, 1.3M.
func fmtCount(v float64) string {
	switch {
	case math.IsNaN(v):
		return "-"
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e4:
		return fmt.Sprintf("%.0fk", v/1e3)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	case v < 10 && v != math.Trunc(v):
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.0f", v)
}
