package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/termui"
)

// The fleet screen: one row per node of the context, what the
// Controller's node list shows, and a trend (tui_trend.go) in the
// rest of the width - Enter opens a node.

var fleetColumns = []termui.Column{
	{Title: "NODE"}, {Title: "ADDRESS", Width: 21}, {Title: "STATE", Width: 11}, {Title: "VERSION", Width: 16},
	{Title: "HAPROXY", Width: 7}, {Title: "CONNS", Width: 6, Right: true}, {Title: "SESS/S", Width: 6, Right: true}, {Title: "CPU", Width: 4, Right: true},
	{Title: "MEM", Width: 4, Right: true}, {Title: "LOAD", Width: 5, Right: true}, {Title: "VRRP", Width: 6}, {Title: "TRIAL", Width: 7},
	{Title: "TREND"}, // titled and sized each frame
}

// The columns' indexes.
const (
	fleetNode = iota
	fleetAddress
	fleetState
	fleetVersion
	fleetHAProxy
	fleetConns
	fleetSess
	fleetCPU
	fleetMem
	fleetLoad
	fleetVRRP
	fleetTrial
	fleetTrend
)

// fleetDropOrder is the columns given up, in this order, for the trend
// to get trendMinWidth cells.
var fleetDropOrder = []int{fleetAddress, fleetVersion, fleetLoad, fleetHAProxy}

// fleetNodeMin and fleetNodeMax bound the NODE column beside a trend,
// as wide as the longest name otherwise.
const fleetNodeMin, fleetNodeMax = 8, 24

type fleetScreen struct {
	table termui.Table
	sort  int
	desc  bool
	order []int // the sampler each row shows
	shown []int // the columns drawn, as fleetColumns indexes

	metric   int // in trendMetrics
	trendOff bool
	window   int // in trendWindows
	scale    trendScale
}

func newFleetScreen() *fleetScreen {
	return &fleetScreen{table: termui.Table{Columns: fleetColumns}, window: trendDefaultWindow}
}

func (s *fleetScreen) open(a *app) {
	for _, smp := range a.samplers {
		smp.full.Store(false)
	}
}

func (s *fleetScreen) close(*app) {}

func (s *fleetScreen) hints(*app) []string {
	return []string{"? help", "q quit", "↑↓ move", "Enter open", "s sort", "r reverse", "m metric", "w window", "y scale"}
}

func (s *fleetScreen) key(a *app, k string) {
	switch k {
	case termui.KeyEnter:
		if s.table.Cursor < len(s.order) {
			a.show(newNodeScreen(a.samplers[s.order[s.table.Cursor]], true))
		}
	case "s":
		s.sort = s.nextSort()
		if s.sort == fleetTrend {
			a.say("nodes by "+trendMetrics[s.metric].name, termui.ColorDefault)
		} else {
			a.say("nodes by "+strings.ToLower(fleetColumns[s.sort].Title), termui.ColorDefault)
		}
	case "r":
		s.desc = !s.desc
	case "m":
		switch {
		case s.trendOff:
			s.trendOff, s.metric = false, 0
		case s.metric == len(trendMetrics)-1:
			s.trendOff = true
			a.say("trend hidden - m shows it again", termui.ColorDefault)
			return
		default:
			s.metric++
		}
		if s.scale == scaleFull && !trendMetrics[s.metric].pct {
			s.scale = scaleLinear
		}
		a.say("trend: "+trendMetrics[s.metric].name, termui.ColorDefault)
	case "w":
		s.trendOff = false
		s.window = (s.window + 1) % len(trendWindows)
		if m := int(trendWindows[s.window].Minutes()); m == 1 {
			a.say("trend over the last minute", termui.ColorDefault)
		} else {
			a.say(fmt.Sprintf("trend over the last %d minutes", m), termui.ColorDefault)
		}
	case "y":
		s.trendOff = false
		s.scale++
		if s.scale > scaleFull || s.scale == scaleFull && !trendMetrics[s.metric].pct {
			s.scale = scaleLinear
		}
		a.say("trend scale: "+s.scale.String()+", the same for every node", termui.ColorDefault)
	default:
		s.table.Key(k, 10)
	}
}

// nextSort is the column after the sorted one among those drawn.
func (s *fleetScreen) nextSort() int {
	for i, c := range s.shown {
		if c == s.sort && i+1 < len(s.shown) {
			return s.shown[i+1]
		}
	}
	if len(s.shown) == 0 {
		return (s.sort + 1) % len(fleetColumns)
	}
	return s.shown[0]
}

// layout is the columns drawn in w cells and the trend's width (0: no
// trend): every column when the trend is off; else NODE as wide as the
// names, the trend in the rest - columns given up (fleetDropOrder),
// then NODE narrowed, until it has trendMinWidth cells, or no trend.
func (s *fleetScreen) layout(w int, names []string) ([]termui.Column, []int, int) {
	all := make([]int, fleetTrend)
	for i := range all {
		all[i] = i
	}
	if s.trendOff {
		return fleetColumns[:fleetTrend], all, 0
	}
	nodeW := len(fleetColumns[fleetNode].Title)
	for _, n := range names {
		nodeW = max(nodeW, len([]rune(n)))
	}
	nodeW = min(max(nodeW, fleetNodeMin), fleetNodeMax)
	shown := append([]int(nil), all...)
	room := func() int {
		used := len(shown) // the separators, the trend's included
		for _, c := range shown {
			if c == fleetNode {
				used += nodeW
			} else {
				used += fleetColumns[c].Width
			}
		}
		return w - used
	}
	for _, drop := range fleetDropOrder {
		if room() >= trendMinWidth {
			break
		}
		for i, c := range shown {
			if c == drop {
				shown = append(shown[:i], shown[i+1:]...)
				break
			}
		}
	}
	if short := trendMinWidth - room(); short > 0 {
		nodeW = max(nodeW-short, fleetNodeMin)
	}
	trendW := room()
	if trendW < trendMinWidth {
		return fleetColumns[:fleetTrend], all, 0
	}
	shown = append(shown, fleetTrend)
	cols := make([]termui.Column, len(shown))
	for i, c := range shown {
		cols[i] = fleetColumns[c]
		switch c {
		case fleetNode:
			cols[i].Width = nodeW
		case fleetTrend:
			cols[i].Width = trendW
		}
	}
	return cols, shown, trendW
}

// trendEnd is where the trend's time axis ends: now, or when the
// dashboard was paused - a paused fleet stands still.
func (a *app) trendEnd() time.Time {
	if a.paused.Load() && !a.pausedAt.IsZero() {
		return a.pausedAt
	}
	return a.now()
}

// fleetRow is one node's line, as text and as the values it sorts by.
type fleetRow struct {
	cells []string
	keys  []float64 // numeric columns, NaN where text decides
	tones []termui.Style
	err   string
}

func fleetRowOf(v nodeView) fleetRow {
	r := fleetRow{cells: make([]string, len(fleetColumns)), keys: make([]float64, len(fleetColumns)), tones: make([]termui.Style, len(fleetColumns))}
	for i := range r.keys {
		r.keys[i] = math.NaN()
	}
	r.cells[fleetNode], r.cells[fleetAddress] = v.name, v.address
	switch {
	case !v.sampled:
		r.cells[fleetState], r.tones[fleetState] = "connecting", styleMuted
	case v.reachable:
		r.cells[fleetState], r.tones[fleetState] = "ok", styleOK
	default:
		r.cells[fleetState], r.tones[fleetState], r.err = "unreachable", styleDanger, v.err
	}
	if ver := v.slow.version; ver != nil {
		r.cells[fleetVersion] = strings.TrimSpace(ver.GetVersion() + " " + ver.GetActiveSlot())
	}
	p := v.last
	if v.reachable && !p.at.IsZero() {
		switch {
		case p.hap.ok:
			r.cells[fleetHAProxy], r.tones[fleetHAProxy] = p.hap.version, styleOK
			r.cells[fleetConns], r.keys[fleetConns] = fmt.Sprint(p.hap.conns), float64(p.hap.conns)
			r.cells[fleetSess], r.keys[fleetSess] = fmt.Sprint(p.hap.sessRate), float64(p.hap.sessRate)
		case p.hap.err != "":
			r.cells[fleetHAProxy], r.tones[fleetHAProxy] = "down", styleDanger
		}
		r.cells[fleetCPU], r.keys[fleetCPU] = fmtPct(p.cpu), p.cpu
		if p.memTotal > 0 {
			mem := float64(p.memUsed) / float64(p.memTotal) * 100
			r.cells[fleetMem], r.keys[fleetMem] = fmt.Sprintf("%.0f%%", mem), mem
		}
		r.cells[fleetLoad], r.keys[fleetLoad] = fmtFloat(p.load1, 2), p.load1
	}
	if vr := v.slow.vrrp; vr != nil {
		for _, i := range vr.GetInstances() {
			r.cells[fleetVRRP], r.tones[fleetVRRP] = i.GetRole(), toneOf(i.GetRole())
			if i.GetRole() == "MASTER" {
				break
			}
		}
	}
	var trials []string
	if v.slow.network.GetTrialPending() {
		trials = append(trials, "net")
	}
	if v.slow.firewall.GetTrialPending() {
		trials = append(trials, "fw")
	}
	if v.slow.sysctl != nil {
		trials = append(trials, "sysctl")
	}
	if len(trials) > 0 {
		r.cells[fleetTrial], r.tones[fleetTrial] = strings.Join(trials, ","), styleWarn
	}
	return r
}

func (s *fleetScreen) render(a *app, f *termui.Frame) {
	views := make([]nodeView, len(a.samplers))
	rows := make([]fleetRow, len(a.samplers))
	reachable := 0
	names := make([]string, len(a.samplers))
	for i, smp := range a.samplers {
		views[i] = smp.summary()
		rows[i] = fleetRowOf(views[i])
		names[i] = views[i].name
		if views[i].reachable {
			reachable++
		}
	}
	body := termui.Rect{X: 0, Y: 1, W: f.W, H: f.H - 3}
	cols, shown, trendW := s.layout(body.Inner().W, names)
	s.shown = shown
	if trendW > 0 {
		metric := trendMetrics[s.metric]
		trend := trendOf(a.samplers, views, metric, trendWindows[s.window], s.scale, trendW, a.trendEnd(), time.Duration(a.interval.Load()))
		cols[len(cols)-1].Title = trend.title
		for i := range rows {
			rows[i].cells[fleetTrend], rows[i].keys[fleetTrend], rows[i].tones[fleetTrend] = trend.sparks[i], trend.latest[i], metric.tone
		}
	}
	s.order = make([]int, len(rows))
	for i := range s.order {
		s.order[i] = i
	}
	col := s.sort
	sort.SliceStable(s.order, func(i, j int) bool {
		a, b := rows[s.order[i]], rows[s.order[j]]
		less := false
		switch an, bn := math.IsNaN(a.keys[col]), math.IsNaN(b.keys[col]); {
		case !an && !bn:
			less = a.keys[col] > b.keys[col] // numbers: largest first by default
		case an != bn:
			return !an // a number before none, whatever the direction
		default:
			less = a.cells[col] < b.cells[col]
		}
		if s.desc {
			return !less && a.cells[col] != b.cells[col]
		}
		return less
	})
	s.table.Columns = cols
	s.table.Rows = s.table.Rows[:0]
	for _, i := range s.order {
		cells := make([]string, len(shown))
		for j, c := range shown {
			cells[j] = rows[i].cells[c]
		}
		s.table.Rows = append(s.table.Rows, cells)
	}
	s.table.Tone = func(row, c int) termui.Style {
		if c >= 0 && c < len(shown) && row < len(s.order) {
			return rows[s.order[row]].tones[shown[c]]
		}
		return styleDefault
	}

	// Header: the context, the account, the count.
	clock := a.now().Format("15:04:05")
	f.TextRight(0, f.W-1, 0, clock, styleMuted)
	x := f.Text(1, 0, "janus ▸ ", styleAccent, 0) + 1
	x += f.Text(x, 0, firstOr(a.opts.context, "nodes"), termui.Style{Bold: true}, 0)
	who := a.opts.user
	if a.opts.role != "" {
		who += " (" + a.opts.role + ")"
	}
	if who != "" {
		x += f.Text(x, 0, "  "+strings.TrimSpace(who), styleMuted, 0)
	}
	f.Text(x, 0, fmt.Sprintf("  %d nodes, %d reachable", len(rows), reachable), styleMuted, 0)

	in := f.Box(body, "Fleet", styleFocus, styleTitle)
	s.table.Draw(f, in, true, styleHead, styleCursor)
	if s.table.Cursor < len(s.order) {
		if e := rows[s.order[s.table.Cursor]].err; e != "" {
			f.Text(1, f.H-2, termui.Truncate(views[s.order[s.table.Cursor]].name+": "+e, f.W-2), styleDanger, f.W-2)
		}
	}
}
