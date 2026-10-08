package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/termui"
)

// The node screen: one node, bpytop-style - boxes the digits show or
// hide, graphs of what the sampler derived, tables with a cursor.

type panel int

const (
	panelCPU panel = iota + 1
	panelMemory
	panelNetwork
	panelHAProxy
	panelProcesses
	panelServices
	panelTail
	panelModules
)

var panelNames = map[panel]string{
	panelCPU: "CPU", panelMemory: "Memory", panelNetwork: "Network", panelHAProxy: "HAProxy",
	panelProcesses: "Processes", panelServices: "Services", panelTail: "Events and logs", panelModules: "Modules",
}

// focusOrder is what Tab walks, of the panels shown.
var focusOrder = []panel{panelHAProxy, panelProcesses, panelServices, panelNetwork, panelTail}

var procSorts = []string{"cpu", "memory", "pid", "command"}

type nodeScreen struct {
	s         *sampler
	view      func() nodeView // the sampler's, or a fixed one in tests
	events    *tail
	logs      *tail
	logID     string
	showLogs  bool // on a terminal too narrow for both tails: which one
	hidden    map[panel]bool
	focus     panel
	fromFleet bool
	cancel    context.CancelFunc

	procs    termui.Table
	fronts   termui.Table
	servers  termui.Table
	services termui.Table
	procSort int
	procDesc bool
	tailUp   int // lines scrolled up from the newest (0 = following)

	serverRows []serverRow // what the servers table's rows are
	rects      map[panel]termui.Rect
}

func newNodeScreen(s *sampler, fromFleet bool) *nodeScreen {
	n := &nodeScreen{s: s, events: newTail(), logs: newTail(), logID: "janusd", hidden: map[panel]bool{}, focus: panelHAProxy, fromFleet: fromFleet, procDesc: true}
	n.view = s.view
	n.procs.Columns = []termui.Column{{Title: "PID", Width: 6, Right: true}, {Title: "CPU%", Width: 5, Right: true}, {Title: "RSS", Width: 8, Right: true}, {Title: "COMMAND"}}
	n.fronts.Columns = []termui.Column{{Title: "FRONTEND"}, {Title: "STATUS", Width: 6}, {Title: "CONNS", Width: 6, Right: true}, {Title: "CONN/S", Width: 6, Right: true},
		{Title: "REQ/S", Width: 6, Right: true}, {Title: "5XX", Width: 6, Right: true}, {Title: "EREQ", Width: 5, Right: true}}
	n.servers.Columns = []termui.Column{{Title: "BACKEND/SERVER"}, {Title: "STATUS", Width: 8}, {Title: "WGT", Width: 4, Right: true}, {Title: "CONNS", Width: 6, Right: true},
		{Title: "RATE", Width: 5, Right: true}, {Title: "IN/S", Width: 9, Right: true}, {Title: "OUT/S", Width: 9, Right: true}, {Title: "CHECK", Width: 8}}
	n.services.Columns = []termui.Column{{Title: "SERVICE"}, {Title: "STATE", Width: 9}, {Title: "HEALTH", Width: 9}, {Title: "CPU", Width: 5, Right: true}}
	return n
}

// open starts the full rounds and the tails' followers.
func (n *nodeScreen) open(a *app) {
	n.s.full.Store(true)
	n.s.Kick()
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	go n.follow(ctx)
}

// follow waits for the node's connection, then follows its events and
// the chosen log.
func (n *nodeScreen) follow(ctx context.Context) {
	conn, err := n.s.connection()
	for err != nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		conn, err = n.s.connection()
	}
	go followEvents(ctx, conn, n.events)
	go followLogs(ctx, conn, n.logID, n.logs)
	// The loop redraws every second anyway: lines show up by then.
}

func (n *nodeScreen) close(*app) {
	if n.cancel != nil {
		n.cancel()
	}
	n.s.full.Store(false)
}

// switchLog follows another service's log.
func (n *nodeScreen) switchLog(id string) {
	n.logID = id
	if n.cancel != nil {
		n.cancel()
	}
	n.logs = newTail()
	n.events = newTail()
	ctx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	go n.follow(ctx)
}

// hints are the footer's keys, the ones that matter most first: the
// footer drops what doesn't fit from the right.
func (n *nodeScreen) hints(a *app) []string {
	h := []string{"? help", "q quit"}
	if n.fromFleet {
		h = append(h, "Esc fleet")
	}
	h = append(h, "Tab focus", "↑↓ move", "1-8 panels")
	h = append(h, n.actionHints(a)...)
	if n.focus == panelProcesses {
		h = append(h, "s sort", "r reverse")
	}
	return append(h, "e events", "l logs")
}

func (n *nodeScreen) key(a *app, k string) {
	switch k {
	case termui.KeyEsc, termui.KeyBackspace:
		if n.fromFleet && a.fleet != nil {
			a.show(a.fleet)
		}
	case termui.KeyTab:
		n.cycleFocus(1)
	case termui.KeyBackTab:
		n.cycleFocus(-1)
	case "1", "2", "3", "4", "5", "6", "7", "8":
		p := panel(k[0] - '0')
		n.hidden[p] = !n.hidden[p]
		if n.hidden[n.focus] {
			n.cycleFocus(1)
		}
	case "s":
		if n.focus == panelProcesses {
			n.procSort = (n.procSort + 1) % len(procSorts)
			a.say("processes by "+procSorts[n.procSort], termui.ColorDefault)
		}
	case "r":
		if n.focus == panelProcesses {
			n.procDesc = !n.procDesc
		}
	case "e":
		n.showLogs = false
		n.tailUp = 0
	case "l":
		if n.showLogs || n.bothTails() {
			if n.logID == "janusd" {
				n.switchLog("haproxy")
			} else {
				n.switchLog("janusd")
			}
		}
		n.showLogs = true
		n.tailUp = 0
	default:
		if !n.actionKey(a, k) {
			n.moveKey(k)
		}
	}
}

// bothTails reports whether the last layout showed events and logs
// side by side.
func (n *nodeScreen) bothTails() bool {
	r, ok := n.rects[panelTail]
	return ok && r.W >= 120
}

func (n *nodeScreen) cycleFocus(dir int) {
	var shown []panel
	for _, p := range focusOrder {
		if !n.hidden[p] {
			shown = append(shown, p)
		}
	}
	if len(shown) == 0 {
		return
	}
	i := 0
	for j, p := range shown {
		if p == n.focus {
			i = j
		}
	}
	n.focus = shown[(i+dir+len(shown))%len(shown)]
}

// moveKey sends an arrow to the focused table, or scrolls the tail.
func (n *nodeScreen) moveKey(k string) {
	page := 10
	if r, ok := n.rects[n.focus]; ok {
		page = max(r.H-3, 1)
	}
	switch n.focus {
	case panelProcesses:
		n.procs.Key(k, page)
	case panelHAProxy:
		n.servers.Key(k, page)
	case panelServices:
		n.services.Key(k, page)
	case panelTail:
		switch k {
		case termui.KeyUp:
			n.tailUp++
		case termui.KeyDown:
			n.tailUp = max(n.tailUp-1, 0)
		case termui.KeyPgUp:
			n.tailUp += page
		case termui.KeyPgDn:
			n.tailUp = max(n.tailUp-page, 0)
		case termui.KeyEnd:
			n.tailUp = 0
		case termui.KeyHome:
			n.tailUp = tuiTailLines
		}
	}
}

// selectedServer is the server under the cursor of the servers table,
// nil on a backend's row or without any.
func (n *nodeScreen) selectedServer() *serverRow {
	if n.servers.Cursor < 0 || n.servers.Cursor >= len(n.serverRows) {
		return nil
	}
	r := n.serverRows[n.servers.Cursor]
	if r.server == "" {
		return nil
	}
	return &r
}

// layout places the panels: the tail across the bottom, two columns
// above it, each panel its minimum then the slack by weight; a panel
// that doesn't fit is left out until the terminal grows.
func (n *nodeScreen) layout(w, h int, v *nodeView) map[panel]termui.Rect {
	out := map[panel]termui.Rect{}
	body := termui.Rect{X: 0, Y: 1, W: w, H: h - 2}
	if !n.hidden[panelTail] {
		th := min(max(body.H/4, 6), 12)
		out[panelTail] = termui.Rect{X: 0, Y: body.Y + body.H - th, W: w, H: th}
		body.H -= th
	}
	type spec struct {
		p           panel
		min, weight int
	}
	var left, right []spec
	add := func(col *[]spec, p panel, minH, weight int) {
		if !n.hidden[p] {
			*col = append(*col, spec{p, minH, weight})
		}
	}
	add(&left, panelCPU, 6, 3)
	add(&left, panelMemory, 6, 2)
	add(&left, panelNetwork, 2+min(max(len(interfacesOf(v)), 1), 8), 1)
	add(&left, panelServices, 3+min(max(len(v.last.services), 1), 8), 0)
	if lines := moduleLines(v); len(lines) > 0 {
		add(&left, panelModules, 2+min(len(lines), 6), 0)
	}
	add(&right, panelHAProxy, 12, 3)
	add(&right, panelProcesses, 6, 2)
	lw := min(max(w*2/5, 40), 60)
	leftR, rightR := termui.Rect{X: 0, Y: body.Y, W: lw, H: body.H}, termui.Rect{X: lw, Y: body.Y, W: w - lw, H: body.H}
	switch {
	case len(left) == 0:
		rightR = body
	case len(right) == 0:
		leftR = body
	}
	place := func(col []spec, r termui.Rect) {
		mins, weights := make([]int, len(col)), make([]int, len(col))
		for i, s := range col {
			mins[i], weights[i] = s.min, s.weight
		}
		y := r.Y
		for i, hh := range termui.Distribute(r.H, mins, weights) {
			if hh > 0 {
				out[col[i].p] = termui.Rect{X: r.X, Y: y, W: r.W, H: hh}
				y += hh
			}
		}
	}
	place(left, leftR)
	place(right, rightR)
	return out
}

// interfacesOf is the node's interfaces but the loopback, by name.
func interfacesOf(v *nodeView) []string {
	var names []string
	for name := range v.last.net {
		if name != "lo" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

var (
	styleBorder  = termui.Style{FG: termui.ColorMuted}
	styleFocus   = termui.Style{FG: termui.ColorAccent}
	styleTitle   = termui.Style{FG: termui.ColorAccent, Bold: true}
	styleMuted   = termui.Style{FG: termui.ColorMuted}
	styleHead    = termui.Style{Bold: true}
	styleCursor  = termui.Style{Reverse: true}
	styleAccent  = termui.Style{FG: termui.ColorAccent}
	styleInfo    = termui.Style{FG: termui.ColorInfo}
	styleWarn    = termui.Style{FG: termui.ColorWarn}
	styleDanger  = termui.Style{FG: termui.ColorDanger}
	styleOK      = termui.Style{FG: termui.ColorOK}
	styleBanner  = termui.Style{FG: termui.ColorWarn, Bold: true}
	styleDefault = termui.Style{}
)

// box draws a panel's border with its digit and title; the focused
// panel's border is in the accent.
func (n *nodeScreen) box(f *termui.Frame, p panel, r termui.Rect, title string) termui.Rect {
	border := styleBorder
	if n.focus == p {
		border = styleFocus
	}
	in := f.Box(r, "", border, styleTitle)
	x := f.Text(r.X+1, r.Y, fmt.Sprintf(" %d ", p), styleMuted, r.W-2)
	f.Text(r.X+1+x, r.Y, termui.Truncate(title, r.W-4-x)+" ", styleTitle, r.W-2-x)
	return in
}

func (n *nodeScreen) render(a *app, f *termui.Frame) {
	v := n.view()
	n.rects = n.layout(f.W, f.H, &v)
	n.renderHeader(a, f, &v)
	if r, ok := n.rects[panelCPU]; ok {
		n.renderCPU(f, r, &v)
	}
	if r, ok := n.rects[panelMemory]; ok {
		n.renderMemory(f, r, &v)
	}
	if r, ok := n.rects[panelNetwork]; ok {
		n.renderNetwork(f, r, &v)
	}
	if r, ok := n.rects[panelServices]; ok {
		n.renderServices(f, r, &v)
	}
	if r, ok := n.rects[panelModules]; ok {
		n.renderModules(f, r, &v)
	}
	if r, ok := n.rects[panelHAProxy]; ok {
		n.renderHAProxy(f, r, &v)
	}
	if r, ok := n.rects[panelProcesses]; ok {
		n.renderProcesses(f, r, &v)
	}
	if r, ok := n.rects[panelTail]; ok {
		n.renderTail(f, r)
	}
}

// renderHeader is row 0: the node, its version and slot, uptime,
// HAProxy, the clock - and a banner for each trial pending.
func (n *nodeScreen) renderHeader(a *app, f *termui.Frame, v *nodeView) {
	clock := a.now().Format("15:04:05")
	f.TextRight(0, f.W-1, 0, clock, styleMuted)
	x := f.Text(1, 0, "janus ▸ ", styleAccent, 0) + 1
	host := firstOr(v.slow.hostname, v.name)
	x += f.Text(x, 0, host, termui.Style{Bold: true}, f.W-x-10)
	var parts []string
	if ver := v.slow.version; ver != nil {
		s := ver.GetVersion()
		if ver.GetActiveSlot() != "" {
			s += " slot " + ver.GetActiveSlot()
		}
		parts = append(parts, s)
	}
	if v.last.at.IsZero() {
		if v.sampled && !v.reachable {
			parts = append(parts, "unreachable: "+v.err)
		} else {
			parts = append(parts, "connecting…")
		}
	} else {
		if bt := bootTime(v); !bt.IsZero() {
			parts = append(parts, "up "+humanDuration(a.now().Sub(bt)))
		}
		if v.last.hap.ok {
			parts = append(parts, "HAProxy "+v.last.hap.version)
		}
	}
	if ver := v.slow.version; ver != nil {
		if ver.GetArch() != "" {
			parts = append(parts, ver.GetArch())
		}
		if id := ver.GetSchematicId(); len(id) >= 8 {
			parts = append(parts, "schematic "+id[:8])
		}
	}
	if ns := v.slow.network; ns != nil && ns.GetManaged() {
		if ns.GetTime().GetSynchronized() {
			parts = append(parts, "time ok")
		} else {
			parts = append(parts, "time UNSYNCED")
		}
	}
	x += f.Text(x, 0, termui.Truncate("  "+strings.Join(parts, "  "), f.W-x-10), styleMuted, f.W-x-10)
	for _, b := range banners(v, a.now()) {
		if x+len(b)+4 > f.W-10 {
			break
		}
		x += f.Text(x, 0, "  ["+b+"]", styleBanner, 0)
	}
	if !v.reachable && v.sampled && !v.last.at.IsZero() {
		f.Text(x+2, 0, termui.Truncate("no answer: "+v.err, f.W-x-12), styleDanger, 0)
	}
}

// bootTime is when the node's kernel started, from the last sample.
func bootTime(v *nodeView) time.Time { return v.last.boot }

// banners are the trials pending on the node, with when they revert.
func banners(v *nodeView, now time.Time) []string {
	var out []string
	until := func(unix int64) string {
		if unix <= 0 {
			return ""
		}
		return " reverts in " + humanDuration(time.Unix(unix, 0).Sub(now).Round(time.Second))
	}
	if ns := v.slow.network; ns.GetTrialPending() {
		out = append(out, "network trial"+until(ns.GetTrialRevertAtUnix()))
	}
	if fw := v.slow.firewall; fw.GetTrialPending() {
		out = append(out, "firewall trial"+until(fw.GetTrialRevertAtUnix()))
	}
	if t := v.slow.sysctl; t != nil {
		out = append(out, "sysctl trial"+until(t.GetRevertAtUnix()))
	}
	return out
}

func series(v *nodeView, pick func(p point) float64) []float64 {
	out := make([]float64, len(v.history))
	for i, p := range v.history {
		out[i] = pick(p)
	}
	return out
}

func (n *nodeScreen) renderCPU(f *termui.Frame, r termui.Rect, v *nodeView) {
	title := "CPU " + fmtPct(v.last.cpu)
	if !v.last.at.IsZero() {
		title += fmt.Sprintf("  load %s %s %s", fmtFloat(v.last.load1, 2), fmtFloat(v.last.load5, 2), fmtFloat(v.last.load15, 2))
	}
	if t := compactTopology(v.slow.cpu); t != "" {
		title += "  " + t
	}
	in := n.box(f, panelCPU, r, title)
	termui.Graph(f, in, series(v, func(p point) float64 { return p.cpu }), 100, styleAccent)
}

// compactTopology is "4 cores", or "8 cores, 2 sockets" when there are
// several.
func compactTopology(cpu *janusv1alpha1.CPUInfoResponse) string {
	switch {
	case cpu.GetCores() == 0:
		return ""
	case cpu.GetSockets() > 1:
		return fmt.Sprintf("%d cores, %d sockets", cpu.GetCores(), cpu.GetSockets())
	case cpu.GetCores() == 1:
		return "1 core"
	}
	return fmt.Sprintf("%d cores", cpu.GetCores())
}

func (n *nodeScreen) renderMemory(f *termui.Frame, r termui.Rect, v *nodeView) {
	p := v.last
	title := "Memory"
	if p.memTotal > 0 {
		title = fmt.Sprintf("Memory %s / %s  %.0f%%", humanBytes(p.memUsed), humanBytes(p.memTotal), float64(p.memUsed)/float64(p.memTotal)*100)
	}
	in := n.box(f, panelMemory, r, title)
	if in.Empty() {
		return
	}
	if p.memTotal > 0 {
		// Used is what the kernel can't give back (total - available);
		// the cache it could is shown apart from it, inside the rest.
		cached := min(p.memCache, p.memTotal-min(p.memTotal, p.memUsed))
		termui.Meter(f, termui.Rect{X: in.X, Y: in.Y, W: in.W, H: 1}, []float64{float64(p.memUsed), float64(cached)}, []termui.Style{styleAccent, styleInfo}, float64(p.memTotal), styleMuted)
		legend := fmt.Sprintf("used %s  cached %s  available %s", humanBytes(p.memUsed), humanBytes(cached), humanBytes(p.memTotal-min(p.memTotal, p.memUsed)))
		if in.H >= 2 {
			f.Text(in.X, in.Y+1, termui.Truncate(legend, in.W), styleMuted, in.W)
		}
	}
	if in.H > 2 {
		termui.Graph(f, termui.Rect{X: in.X, Y: in.Y + 2, W: in.W, H: in.H - 2}, series(v, func(p point) float64 {
			if p.memTotal == 0 {
				return math.NaN()
			}
			return float64(p.memUsed) / float64(p.memTotal) * 100
		}), 100, styleAccent)
	}
}

// renderNetwork is one line per interface: its link, rates, a graph of
// both directions.
func (n *nodeScreen) renderNetwork(f *termui.Frame, r termui.Rect, v *nodeView) {
	in := n.box(f, panelNetwork, r, "Network")
	names := interfacesOf(v)
	if in.Empty() || len(names) == 0 {
		return
	}
	status := map[string]*janusv1alpha1.NetworkInterfaceStatus{}
	for _, i := range v.slow.network.GetInterfaces() {
		status[i.GetName()] = i
	}
	rows := max(in.H/len(names), 1)
	for i, name := range names {
		y := in.Y + i*rows
		if y >= in.Y+in.H {
			break
		}
		rt := v.last.net[name]
		dot, dotSt := "●", styleMuted
		if st, ok := status[name]; ok && v.slow.network.GetManaged() {
			if st.GetUp() && st.GetCarrier() {
				dotSt = styleOK
			} else {
				dotSt = styleDanger
			}
		}
		x := in.X
		x += f.Text(x, y, dot, dotSt, 0) + 1
		x += f.Text(x, y, termui.Pad(name, 8), termui.Style{Bold: true}, 0)
		x += f.Text(x, y, "↓", styleInfo, 0)
		x += f.Text(x, y, termui.PadLeft(fmtRate(rt.rx), 10), styleDefault, 0) + 1
		x += f.Text(x, y, "↑", styleAccent, 0)
		x += f.Text(x, y, termui.PadLeft(fmtRate(rt.tx), 10), styleDefault, 0) + 1
		if rt.rxErr+rt.txErr > 0 {
			x += f.Text(x, y, fmt.Sprintf("%d err", rt.rxErr+rt.txErr), styleWarn, 0) + 1
		}
		if gw := in.X + in.W - x; gw >= 4 {
			termui.Graph(f, termui.Rect{X: x, Y: y, W: gw, H: rows}, series(v, func(p point) float64 {
				rt, ok := p.net[name]
				if !ok || math.IsNaN(rt.rx) || math.IsNaN(rt.tx) {
					return math.NaN()
				}
				return rt.rx + rt.tx
			}), 0, styleInfo)
		}
	}
}

func (n *nodeScreen) renderServices(f *termui.Frame, r termui.Rect, v *nodeView) {
	in := n.box(f, panelServices, r, "Services")
	n.services.Rows = n.services.Rows[:0]
	var tones []termui.Style
	if len(v.last.services) > 0 {
		for _, s := range v.last.services {
			cpu := ""
			if c, ok := v.last.svcCPU[s.id]; ok {
				cpu = fmtPct(c)
			}
			n.services.Rows = append(n.services.Rows, []string{s.id, s.state, s.health, cpu})
			tones = append(tones, toneOf(s.state), toneOf(s.health))
		}
	} else {
		for _, id := range []string{"janusd", "haproxy"} {
			cpu := "-"
			if c, ok := v.last.svcCPU[id]; ok {
				cpu = fmtPct(c)
			}
			n.services.Rows = append(n.services.Rows, []string{id, "", "", cpu})
			tones = append(tones, styleDefault, styleDefault)
		}
	}
	n.services.Tone = func(row, col int) termui.Style {
		switch col {
		case 1:
			return tones[row*2]
		case 2:
			return tones[row*2+1]
		}
		return styleDefault
	}
	n.services.Draw(f, in, n.focus == panelServices, styleHead, styleCursor)
}

// moduleLines are the optional modules' states: VRRP instances, BGP
// sessions, Consul, the firewall - only what the image has.
func moduleLines(v *nodeView) []moduleLine {
	var out []moduleLine
	if vr := v.slow.vrrp; vr != nil && vr.GetState() != janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
		if len(vr.GetInstances()) == 0 {
			out = append(out, moduleLine{"VRRP", strings.ToLower(moduleState(vr.GetState())), toneOf(moduleState(vr.GetState()))})
		}
		for _, i := range vr.GetInstances() {
			out = append(out, moduleLine{"VRRP " + i.GetName(), fmt.Sprintf("%s on %s (%d) %s", i.GetRole(), i.GetInterface(), i.GetEffectivePriority(), strings.Join(i.GetVirtualIps(), " ")), toneOf(i.GetRole())})
		}
	}
	if b := v.slow.bgp; b != nil && b.GetState() != janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
		if len(b.GetProtocols()) == 0 {
			out = append(out, moduleLine{"BGP", strings.ToLower(moduleState(b.GetState())), toneOf(moduleState(b.GetState()))})
		}
		for _, p := range b.GetProtocols() {
			state := firstOr(p.GetBgpState(), p.GetState())
			out = append(out, moduleLine{"BGP " + p.GetName(), fmt.Sprintf("%s  %s AS%d", state, p.GetNeighborAddress(), p.GetNeighborAs()), toneOf(state)})
		}
	}
	if c := v.slow.consul; c != nil && c.GetState() != janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
		out = append(out, moduleLine{"Consul", fmt.Sprintf("%s  %s dc %s  %d members", strings.ToLower(moduleState(c.GetState())), c.GetNodeName(), c.GetDatacenter(), len(c.GetMembers())), toneOf(moduleState(c.GetState()))})
	}
	if fw := v.slow.firewall; fw != nil && fw.GetState() != janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
		s := strings.ToLower(moduleState(fw.GetState()))
		if fw.GetTrialPending() {
			s += ", trial pending"
		}
		out = append(out, moduleLine{"Firewall", s, toneOf(moduleState(fw.GetState()))})
	}
	return out
}

type moduleLine struct {
	name, text string
	tone       termui.Style
}

func moduleState(s janusv1alpha1.ModuleState) string {
	switch s {
	case janusv1alpha1.ModuleState_MODULE_STATE_RUNNING:
		return "running"
	case janusv1alpha1.ModuleState_MODULE_STATE_STOPPED:
		return "stopped"
	case janusv1alpha1.ModuleState_MODULE_STATE_ERROR:
		return "error"
	}
	return "not enabled"
}

func (n *nodeScreen) renderModules(f *termui.Frame, r termui.Rect, v *nodeView) {
	in := n.box(f, panelModules, r, "Modules")
	for i, l := range moduleLines(v) {
		if i >= in.H {
			break
		}
		x := f.Text(in.X, in.Y+i, termui.Pad(l.name, 10)+" ", termui.Style{Bold: true}, in.W)
		f.Text(in.X+x, in.Y+i, termui.Truncate(l.text, in.W-x), l.tone, in.W-x)
	}
}

// renderHAProxy is the process (title), a graph of its session rate,
// the frontends, then the backends with their servers.
func (n *nodeScreen) renderHAProxy(f *termui.Frame, r termui.Rect, v *nodeView) {
	h := v.last.hap
	title := "HAProxy"
	titleStyle := styleTitle
	switch {
	case h.ok:
		title = fmt.Sprintf("HAProxy %s  up %s  %d conns  %d sess/s  idle %d%%", h.version, humanDuration(time.Duration(h.uptime)*time.Second), h.conns, h.sessRate, h.idle)
	case h.err != "":
		title = "HAProxy: " + h.err
		titleStyle = styleDanger
	}
	in := n.box(f, panelHAProxy, r, "")
	x := f.Text(r.X+1, r.Y, fmt.Sprintf(" %d ", panelHAProxy), styleMuted, r.W-2)
	f.Text(r.X+1+x, r.Y, termui.Truncate(title, r.W-4-x)+" ", titleStyle, r.W-2-x)
	if in.Empty() {
		return
	}
	y := in.Y
	avail := in.H
	if avail >= 14 {
		termui.Graph(f, termui.Rect{X: in.X, Y: y, W: in.W, H: 3}, series(v, func(p point) float64 {
			if !p.hap.ok {
				return math.NaN()
			}
			return float64(p.hap.sessRate)
		}), 0, styleAccent)
		y += 3
		avail -= 3
	}
	n.fronts.Rows = n.fronts.Rows[:0]
	var frontTones []termui.Style
	for _, fr := range v.last.fronts {
		n.fronts.Rows = append(n.fronts.Rows, []string{fr.name, fr.status, fmt.Sprint(fr.scur), fmt.Sprint(fr.rate), fmt.Sprint(fr.reqs), fmt.Sprint(fr.h5xx), fmt.Sprint(fr.ereq)})
		frontTones = append(frontTones, toneOf(fr.status))
	}
	if fh := min(1+len(n.fronts.Rows), 5); len(n.fronts.Rows) > 0 && avail-fh >= 4 {
		n.fronts.Tone = func(row, col int) termui.Style {
			if col == 1 {
				return frontTones[row]
			}
			return styleDefault
		}
		n.fronts.Draw(f, termui.Rect{X: in.X, Y: y, W: in.W, H: fh}, false, styleHead, styleCursor)
		y += fh + 1
		avail -= fh + 1
	}
	n.serverRows = v.last.servers
	n.servers.Rows = n.servers.Rows[:0]
	for _, s := range v.last.servers {
		if s.server == "" {
			n.servers.Rows = append(n.servers.Rows, []string{fmt.Sprintf("%s  %s  %d conns  %d/s", s.backend, s.status, s.scur, s.rate)})
			continue
		}
		n.servers.Rows = append(n.servers.Rows, []string{"  " + s.server, s.status, fmt.Sprint(s.weight), fmt.Sprint(s.scur), fmt.Sprint(s.rate), fmtRate(s.in), fmtRate(s.out), s.check})
	}
	rows := v.last.servers
	n.servers.Heading = func(i int) bool { return i < len(rows) && rows[i].server == "" }
	n.servers.Tone = func(row, col int) termui.Style {
		if row >= len(rows) {
			return styleDefault
		}
		if col == 1 || (col == -1 && rows[row].server == "") {
			return toneOf(rows[row].status)
		}
		return styleDefault
	}
	if avail >= 2 {
		if len(n.servers.Rows) == 0 && h.ok {
			f.Text(in.X, y, "no backend in haproxy.cfg", styleMuted, in.W)
			return
		}
		n.servers.Draw(f, termui.Rect{X: in.X, Y: y, W: in.W, H: avail}, n.focus == panelHAProxy, styleHead, styleCursor)
	}
}

func (n *nodeScreen) renderProcesses(f *termui.Frame, r termui.Rect, v *nodeView) {
	title := fmt.Sprintf("Processes  %d", len(v.last.procs))
	if n.procSort != 0 || !n.procDesc {
		title += "  by " + procSorts[n.procSort]
		if !n.procDesc {
			title += " ↑"
		}
	}
	in := n.box(f, panelProcesses, r, title)
	procs := append([]procRow(nil), v.last.procs...)
	less := func(i, j int) bool {
		a, b := procs[i], procs[j]
		switch procSorts[n.procSort] {
		case "cpu":
			an, bn := math.IsNaN(a.cpu), math.IsNaN(b.cpu)
			if an != bn {
				return !an // NaN last whatever the direction
			}
			if a.cpu != b.cpu {
				return a.cpu > b.cpu
			}
			return a.rss > b.rss
		case "memory":
			if a.rss != b.rss {
				return a.rss > b.rss
			}
		case "pid":
			return a.pid > b.pid
		case "command":
			if a.cmd != b.cmd {
				return a.cmd > b.cmd
			}
		}
		return a.pid > b.pid
	}
	if !n.procDesc {
		sort.SliceStable(procs, func(i, j int) bool { return less(j, i) })
	} else {
		sort.SliceStable(procs, less)
	}
	n.procs.Rows = n.procs.Rows[:0]
	for _, p := range procs {
		cpu := "-"
		if !math.IsNaN(p.cpu) {
			cpu = fmt.Sprintf("%.1f", p.cpu)
		}
		n.procs.Rows = append(n.procs.Rows, []string{fmt.Sprint(p.pid), cpu, humanBytes(p.rss), p.cmd})
	}
	n.procs.Draw(f, in, n.focus == panelProcesses, styleHead, styleCursor)
}

// renderTail is the events and the log: side by side on a wide
// terminal, else the one e/l chose.
func (n *nodeScreen) renderTail(f *termui.Frame, r termui.Rect) {
	if r.W >= 120 {
		half := r.W / 2
		n.renderOneTail(f, termui.Rect{X: r.X, Y: r.Y, W: half, H: r.H}, n.events, "Events", !n.showLogs, true)
		n.renderOneTail(f, termui.Rect{X: r.X + half, Y: r.Y, W: r.W - half, H: r.H}, n.logs, "Logs "+n.logID, n.showLogs, false)
		return
	}
	if n.showLogs {
		n.renderOneTail(f, r, n.logs, "Logs "+n.logID, true, false)
	} else {
		n.renderOneTail(f, r, n.events, "Events", true, true)
	}
}

// renderOneTail draws a tail's last lines, the newest at the bottom;
// stamped lines get their time (a log line carries its own).
func (n *nodeScreen) renderOneTail(f *termui.Frame, r termui.Rect, t *tail, title string, scrolled, stamped bool) {
	border := styleBorder
	if n.focus == panelTail && scrolled {
		border = styleFocus
	}
	in := f.Box(r, "", border, styleTitle)
	x := f.Text(r.X+1, r.Y, fmt.Sprintf(" %d ", panelTail), styleMuted, r.W-2)
	lines, errText := t.snapshot(tuiTailLines)
	up := 0
	if scrolled {
		up = min(n.tailUp, max(len(lines)-in.H, 0))
		if n.focus == panelTail {
			n.tailUp = up
		}
	}
	if up > 0 {
		title += fmt.Sprintf("  ↑%d", up)
	}
	f.Text(r.X+1+x, r.Y, termui.Truncate(title, r.W-4-x)+" ", styleTitle, r.W-2-x)
	if in.Empty() {
		return
	}
	y := in.Y
	if errText != "" {
		f.Text(in.X, y, termui.Truncate(errText, in.W), styleWarn, in.W)
		y++
	}
	room := in.Y + in.H - y
	if room <= 0 {
		return
	}
	end := len(lines) - up
	start := max(end-room, 0)
	for _, l := range lines[start:end] {
		x := 0
		if stamped {
			x = f.Text(in.X, y, l.at.Format("15:04:05")+" ", styleMuted, in.W)
		}
		f.Text(in.X+x, y, termui.Truncate(l.text, in.W-x), termui.Style{FG: l.tone}, in.W-x)
		y++
	}
}
