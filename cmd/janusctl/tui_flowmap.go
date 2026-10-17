package main

import (
	"cmp"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/haproxyflow"
	"github.com/swenske/Janus/internal/termui"
)

// The node screen's traffic flow (f) is HAProxy drawn as a map - the
// node's interfaces, HAProxy, its frontends, the backends they send to
// and their servers, and beyond the interfaces the log targets its logs
// leave by - with each one's traffic, as the Controller's node page
// draws it (Monitoring › Traffic flow). Which frontend sends to
// which backend is in the configuration (internal/haproxyflow); the
// counters are the sampler's. This file is the map: the graph, where
// each node goes on a grid of cells, top to bottom or left to right,
// and the links routed between them with box-drawing lines. It's pure:
// tui_flow.go asks the node and draws it in the screen.

// The kinds of node, and the level each sits on.
const (
	kindLog   = "log" // a log target, beyond the interface its logs leave by
	kindIface = "iface"
	kindHub   = "hub"
	kindFE    = "fe"
	kindBE    = "be"
	kindSrv   = "srv"
	kindMore  = "more" // "+N more": a backend's servers past flowMaxServers
)

// A level no node sits on takes no room: without log targets, the
// interfaces are the first column (or row).
var flowLevel = map[string]int{kindLog: 0, kindIface: 1, kindHub: 2, kindFE: 3, kindBE: 4, kindSrv: 5, kindMore: 5}

const flowLevels = 6

const (
	flowMaxServers = 8  // a backend's servers shown, the last as "+N more" when there are more
	flowNameMax    = 18 // a node's name, cut beyond
	flowValuesMax  = 22 // its numbers
)

// flowValuesRoom is the cells a node's numbers have whatever they say,
// so the map doesn't move as traffic comes and goes: "14.5KiB/s",
// "14.5KiB/s · 12 sess", "12 conns · 1.7k req/s", "12 msg/s · 1.2KiB/s";
// a server's - at the end of its line - what it says idle, "0B/s · 0
// sess".
var flowValuesRoom = map[string]int{kindLog: 20, kindIface: 9, kindHub: 21, kindFE: 19, kindBE: 19, kindSrv: 13, kindMore: 13}

// How a link looks, worst first: a cell several links cross shows the
// best of them.
const (
	lookOff    = iota // stopped, in maintenance
	lookDown          // to a server that fails its checks
	lookIdle          // nothing goes through
	lookActive        // traffic goes through
)

type flowNode struct {
	id, kind, name string
	backend        string   // a server's, a "+N more"'s
	tone           string   // "" (the kind's colour), ok, warn, danger, off
	values         []string // under the name: traffic and sessions, or what's wrong
	valuesTone     string
	linked         bool    // a backend some frontend rule names
	traffic        float64 // bytes/s; NaN unknown
	busy           bool    // sessions or a rate, before a byte rate is known
	fe             *frontendRow
	row            *serverRow // a backend's own row or a server's
	servers        []*flowNode
	hidden         []serverRow // a "+N more"'s
	down           int         // a backend's servers down
	status         *janusv1alpha1.NetworkInterfaceStatus
	up             int      // an interface: 1 up, 0 down, -1 unknown
	binds, answers []string // a frontend's, from the configuration
	mode           string   // a frontend's or backend's: http, tcp
	log            *logRow  // a log target's
	room           int      // the cells its numbers have: more than they take, so the map holds still

	x, y int // its dot's cell once placed
}

// activity is the traffic through a node: its byte rate, or 1 for one
// busy before a rate is known (the first sample).
func (n *flowNode) activity() float64 {
	if !math.IsNaN(n.traffic) {
		return n.traffic
	}
	if n.busy {
		return 1
	}
	return 0
}

// oneLine reports whether the node's label is one line (a server's)
// rather than its name above its numbers.
func (n *flowNode) oneLine() bool { return n.kind == kindSrv || n.kind == kindMore }

func (n *flowNode) shownName() string { return termui.Truncate(n.name, flowNameMax) }

// tag is what follows a frontend's or backend's name: its mode.
func (n *flowNode) tag() string {
	if (n.kind == kindFE || n.kind == kindBE) && (n.mode == "http" || n.mode == "tcp") {
		return n.mode
	}
	return ""
}

// nameWidth is the cells its name and tag take.
func (n *flowNode) nameWidth() int {
	w := len([]rune(n.shownName()))
	if t := n.tag(); t != "" {
		w += 1 + len(t)
	}
	return w
}

func (n *flowNode) shownValues() string {
	return termui.Truncate(strings.Join(n.values, " · "), flowValuesMax)
}

// width is the cells the node's label takes from its dot.
func (n *flowNode) width() int {
	name, values := n.nameWidth(), max(len([]rune(n.shownValues())), n.room)
	if n.oneLine() {
		if values == 0 {
			return 2 + name
		}
		return 2 + name + 1 + values
	}
	return 2 + max(name, values)
}

func (n *flowNode) height() int {
	if n.oneLine() {
		return 1
	}
	return 2
}

type flowEdge struct {
	id, from, to string
	kind         string // the level it leaves: iface, hub (to a frontend), fe (to a backend), be (to a server), log (a log target to its interface)
	rules        []haproxyflow.LinkRule
	look         int
	proto        string // http, tcp, log - or "" (an interface's: all of its traffic)
}

// flowGraph is the map's nodes and links, before they're placed.
type flowGraph struct {
	nodes       []*flowNode
	byID        map[string]*flowNode
	edges       []*flowEdge
	logs        []*flowNode
	ifaces      []*flowNode
	hub         *flowNode
	frontends   []*flowNode
	backends    []*flowNode
	configKnown bool
	unresolved  []haproxyflow.Unresolved
}

func (g *flowGraph) add(n *flowNode) *flowNode {
	g.nodes = append(g.nodes, n)
	g.byID[n.id] = n
	return n
}

func (g *flowGraph) link(from, to, kind string, rules []haproxyflow.LinkRule) *flowEdge {
	e := &flowEdge{id: from + ">" + to, from: from, to: to, kind: kind, rules: rules}
	g.edges = append(g.edges, e)
	return e
}

func frontendTone(status string) string {
	switch strings.ToUpper(status) {
	case "OPEN":
		return "ok"
	case "FULL":
		return "warn"
	case "STOP":
		return "off"
	}
	return ""
}

// serverTone is the tone of a server's state, as the node page's.
func serverTone(status string) string {
	s := strings.ToUpper(strings.TrimSpace(status))
	switch {
	case strings.HasPrefix(s, "MAINT"):
		return "off"
	case strings.HasPrefix(s, "DRAIN"), strings.HasPrefix(s, "NOLB"), len(s) > 3 && strings.HasPrefix(s, "UP ") && unicode.IsDigit(rune(s[3])): // "UP 1/3": going down
		return "warn"
	case strings.HasPrefix(s, "DOWN"):
		return "danger"
	case strings.HasPrefix(s, "UP"), s == "NO CHECK":
		return "ok"
	}
	return ""
}

func sumRate(in, out float64) float64 {
	if math.IsNaN(in) || math.IsNaN(out) {
		return math.NaN()
	}
	return in + out
}

// naturalLess orders names with their numbers by value: eth2 before eth10.
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		da, db := unicode.IsDigit(rune(a[0])), unicode.IsDigit(rune(b[0]))
		if da && db {
			i, j := 0, 0
			for i < len(a) && unicode.IsDigit(rune(a[i])) {
				i++
			}
			for j < len(b) && unicode.IsDigit(rune(b[j])) {
				j++
			}
			na, nb := strings.TrimLeft(a[:i], "0"), strings.TrimLeft(b[:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[i:], b[j:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

// buildFlow joins the node's counters to the links the configuration
// gives. Without a topology (the configuration isn't readable), nothing
// links frontends to backends; servers always hang from their backend.
func buildFlow(v *nodeView, top *haproxyflow.Topology, maps map[string]map[string]string, servers bool) *flowGraph {
	g := &flowGraph{byID: map[string]*flowNode{}, configKnown: top != nil}
	internal := func(name string) bool { return strings.HasPrefix(name, "<") } // HAProxy's own: <HTTPCLIENT>...

	// The interfaces but the loopback, with their state when known.
	status := map[string]*janusv1alpha1.NetworkInterfaceStatus{}
	seen := map[string]bool{}
	var names []string
	for _, i := range v.slow.network.GetInterfaces() {
		status[i.GetName()] = i
	}
	for _, i := range v.slow.network.GetInterfaces() {
		if !seen[i.GetName()] {
			seen[i.GetName()] = true
			names = append(names, i.GetName())
		}
	}
	for name := range v.last.net {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	for _, name := range names {
		st := status[name]
		if name == "lo" || st.GetKind() == "loopback" {
			continue
		}
		n := &flowNode{id: "if:" + name, kind: kindIface, name: name, traffic: math.NaN(), status: st, up: -1}
		if rt, ok := v.last.net[name]; ok {
			n.traffic = sumRate(rt.rx, rt.tx)
		}
		if st != nil {
			n.up = 0
			if st.GetUp() && st.GetCarrier() {
				n.up = 1
			}
		}
		switch {
		case n.up == 0 && st.GetUp():
			n.values, n.tone = []string{"No carrier"}, "off"
		case n.up == 0:
			n.values, n.tone = []string{"Down"}, "off"
		default:
			n.values = []string{fmtRate(n.traffic)}
		}
		g.ifaces = append(g.ifaces, g.add(n))
	}

	// HAProxy.
	h := v.last.hap
	hub := &flowNode{id: kindHub, kind: kindHub, name: "HAProxy", traffic: math.NaN()}
	switch {
	case h.ok:
		hub.values = []string{fmtCount(float64(h.conns)) + " conns"}
		if !math.IsNaN(h.reqRate) {
			hub.values = append(hub.values, fmtCount(h.reqRate)+" req/s")
		}
	case v.last.at.IsZero():
		hub.values = []string{"…"}
	default:
		hub.values, hub.valuesTone, hub.tone = []string{"not answering"}, "danger", "danger"
	}
	g.hub = g.add(hub)

	// Frontends, in the configuration's order.
	order := map[string]int{}
	conf := map[string]haproxyflow.Frontend{}
	if top != nil {
		for i, f := range top.Frontends {
			order[f.Name], conf[f.Name] = i, f
		}
	}
	var fronts []*frontendRow
	for i := range v.last.fronts {
		if !internal(v.last.fronts[i].name) {
			fronts = append(fronts, &v.last.fronts[i])
		}
	}
	rank := func(name string, i int) int {
		if o, ok := order[name]; ok {
			return o
		}
		return 1e6 + i
	}
	idx := map[*frontendRow]int{}
	for i, f := range fronts {
		idx[f] = i
	}
	sort.SliceStable(fronts, func(i, j int) bool {
		return rank(fronts[i].name, idx[fronts[i]]) < rank(fronts[j].name, idx[fronts[j]])
	})
	for _, f := range fronts {
		n := &flowNode{id: "fe:" + f.name, kind: kindFE, name: f.name, tone: frontendTone(f.status), fe: f, traffic: sumRate(f.in, f.out), busy: f.scur > 0 || f.rate > 0,
			binds: conf[f.name].Binds, answers: conf[f.name].Answers, mode: f.mode}
		if n.tone == "off" {
			n.values = []string{f.status}
		} else {
			n.values = []string{fmtRate(n.traffic), fmtCount(float64(f.scur)) + " sess"}
		}
		g.frontends = append(g.frontends, g.add(n))
		g.link(kindHub, n.id, kindHub, nil)
	}

	// Backends and their servers.
	for i := range v.last.servers {
		r := &v.last.servers[i]
		if internal(r.backend) {
			continue
		}
		if r.server == "" {
			n := &flowNode{id: "be:" + r.backend, kind: kindBE, name: r.backend, row: r, traffic: sumRate(r.in, r.out), busy: r.scur > 0 || r.rate > 0, mode: r.mode}
			g.backends = append(g.backends, g.add(n))
			continue
		}
		be := g.byID["be:"+r.backend]
		if be == nil {
			continue
		}
		s := &flowNode{id: "srv:" + r.backend + "/" + r.server, kind: kindSrv, name: r.server, backend: r.backend, row: r, tone: serverTone(r.status),
			traffic: sumRate(r.in, r.out), busy: r.scur > 0 || r.rate > 0}
		if s.tone == "danger" || s.tone == "warn" || s.tone == "off" {
			for _, p := range []string{r.status, strings.TrimPrefix(r.check, "* ")} {
				if p != "" {
					s.values = append(s.values, p)
				}
			}
			if s.tone != "off" {
				s.valuesTone = s.tone
			}
		} else {
			s.values = []string{fmtRate(s.traffic), fmtCount(float64(r.scur)) + " sess"}
		}
		be.servers = append(be.servers, s)
	}
	for _, b := range g.backends {
		for _, s := range b.servers {
			if s.tone == "danger" {
				b.down++
			}
		}
		st := strings.ToUpper(b.row.status)
		switch {
		case strings.HasPrefix(st, "DOWN"):
			b.tone, b.values, b.valuesTone = "danger", []string{"DOWN"}, "danger"
		case b.down > 0:
			b.tone, b.values, b.valuesTone = "warn", []string{fmt.Sprintf("%d/%d servers up", len(b.servers)-b.down, len(b.servers))}, "warn"
		default:
			if st == "UP" {
				b.tone = "ok"
			}
			b.values = []string{fmtRate(b.traffic), fmtCount(float64(b.row.scur)) + " sess"}
		}
	}

	// What the configuration links.
	if top != nil {
		var names []string
		for _, b := range g.backends {
			names = append(names, b.name)
		}
		links, unresolved := top.Links(names, maps)
		g.unresolved = unresolved
		for _, l := range links {
			if g.byID["fe:"+l.Frontend] == nil {
				continue
			}
			g.link("fe:"+l.Frontend, "be:"+l.Backend, kindFE, l.Rules)
			g.byID["be:"+l.Backend].linked = true
		}
	}

	// The servers shown, then the links that reach them.
	for _, b := range g.backends {
		all := b.servers
		shown := all
		if !servers {
			shown = nil
		} else if len(all) > flowMaxServers {
			shown = all[:flowMaxServers-1]
		}
		b.servers = nil
		for _, s := range shown {
			b.servers = append(b.servers, g.add(s))
			g.link(b.id, s.id, kindBE, nil)
		}
		if servers && len(shown) < len(all) {
			more := &flowNode{id: "more:" + b.name, kind: kindMore, name: fmt.Sprintf("+%d more", len(all)-len(shown)), backend: b.name}
			down := 0
			for _, s := range all[len(shown):] {
				more.hidden = append(more.hidden, *s.row)
				if s.tone == "danger" {
					down++
				}
				more.traffic += s.activity()
			}
			if down > 0 {
				more.tone, more.values, more.valuesTone = "danger", []string{fmt.Sprintf("%d down", down)}, "danger"
			} else {
				more.values = []string{fmt.Sprintf("%d servers", len(all)-len(shown))}
			}
			b.servers = append(b.servers, g.add(more))
			g.link(b.id, more.id, kindBE, nil)
		}
	}
	for _, it := range g.ifaces {
		g.link(it.id, kindHub, kindIface, nil)
	}
	// The log targets, each linked to the interface its logs leave by
	// when that's on the map: drawn from the target, beyond it.
	for i := range v.last.logs {
		r := &v.last.logs[i]
		n := &flowNode{id: "log:" + r.name, kind: kindLog, name: r.name, log: r, traffic: math.NaN(), busy: r.msgRate > 0}
		if r.knowsBytes {
			n.traffic = r.byteRate
		}
		switch r.state {
		case "disabled":
			n.tone, n.values = "off", []string{"disabled"}
		case "failing":
			n.tone, n.values, n.valuesTone = "danger", []string{"failing"}, "danger"
		default:
			if r.state == "ok" {
				n.tone = "ok"
			}
			n.values = []string{fmtCount(r.msgRate) + " msg/s"}
			if r.knowsBytes {
				n.values = append(n.values, fmtRate(r.byteRate))
			}
		}
		g.logs = append(g.logs, g.add(n))
		if it := g.byID["if:"+r.via]; r.via != "" && it != nil {
			n.linked = true
			g.link(n.id, it.id, kindLog, nil)
		}
	}
	// The log targets' and interfaces' links first: they're drawn first.
	sort.SliceStable(g.edges, func(i, j int) bool {
		return flowLevel[g.byID[g.edges[i].from].kind] < flowLevel[g.byID[g.edges[j].from].kind]
	})

	// How each link looks. HAProxy doesn't count how a backend's
	// traffic splits between the frontends that send to it: a link
	// between them is active when both ends are.
	for _, e := range g.edges {
		a, b := g.byID[e.from], g.byID[e.to]
		e.look = lookIdle
		e.proto = flowProto(e, a, b)
		switch e.kind {
		case kindLog:
			switch {
			case a.tone == "danger":
				e.look = lookDown
			case a.tone == "off":
				e.look = lookOff
			case a.activity() > 0:
				e.look = lookActive
			}
		case kindIface:
			if a.up == 0 {
				e.look = lookOff
			} else if a.activity() > 0 {
				e.look = lookActive
			}
		case kindHub:
			if b.tone == "off" {
				e.look = lookOff
			} else if b.activity() > 0 {
				e.look = lookActive
			}
		case kindFE:
			if a.activity() > 0 && b.activity() > 0 {
				e.look = lookActive
			}
		case kindBE:
			switch {
			case b.tone == "danger" && b.kind == kindSrv:
				e.look = lookDown
			case b.tone == "off":
				e.look = lookOff
			case b.activity() > 0:
				e.look = lookActive
			}
		}
	}
	return g
}

// flowProto is what a link's traffic is: a backend's mode from its
// frontend on (a TCP frontend may hand its connections to an HTTP
// backend, never the other way), a frontend's to it, log to a log
// target, "" for an interface's - all of its traffic.
func flowProto(e *flowEdge, a, b *flowNode) string {
	mode := ""
	switch e.kind {
	case kindLog:
		return "log"
	case kindHub:
		mode = b.mode
	case kindFE:
		mode = cmp.Or(b.mode, a.mode)
	case kindBE:
		mode = a.mode
	}
	if mode == "http" || mode == "tcp" {
		return mode
	}
	return ""
}

// related is what a node is part of, to bring forward: the paths
// through it - up to HAProxy, down to the servers - and HAProxy and the
// interfaces, which every path crosses; a log target's link and
// interface; nil for HAProxy and the interfaces (everything).
func (g *flowGraph) related(id string) map[string]bool {
	n := g.byID[id]
	if n == nil || n.kind == kindHub || n.kind == kindIface {
		return nil
	}
	out := map[string]bool{id: true}
	if n.kind == kindLog {
		for _, e := range g.edges {
			if e.from == id {
				out[e.id], out[e.to] = true, true
			}
		}
		return out
	}
	var down, up func(string)
	down = func(from string) {
		for _, e := range g.edges {
			if e.from == from && !out[e.id] {
				out[e.id], out[e.to] = true, true
				down(e.to)
			}
		}
	}
	up = func(to string) {
		for _, e := range g.edges {
			if e.to == to && !out[e.id] && g.byID[e.from].kind != kindIface {
				out[e.id], out[e.from] = true, true
				up(e.from)
			}
		}
	}
	down(id)
	up(id)
	out[kindHub] = true
	for _, e := range g.edges {
		if e.kind == kindIface {
			out[e.id], out[e.from] = true, true
		}
	}
	return out
}

// Directions a line leaves a cell by.
const (
	dirN uint8 = 1 << iota
	dirE
	dirS
	dirW
)

// boxGlyph is the line a cell's directions draw.
var boxGlyph = map[uint8]rune{
	dirN: '│', dirS: '│', dirN | dirS: '│', dirE: '─', dirW: '─', dirE | dirW: '─',
	dirE | dirS: '╭', dirS | dirW: '╮', dirN | dirE: '╰', dirN | dirW: '╯',
	dirN | dirS | dirE: '├', dirN | dirS | dirW: '┤', dirE | dirW | dirS: '┬', dirE | dirW | dirN: '┴', dirN | dirE | dirS | dirW: '┼',
}

// flowMark is what one link drew in a cell: the net it belongs to (the
// links drawn as one: a source's, or a target's when its sources have
// nothing else), the directions, how far along the link the cell is.
type flowMark struct {
	net  string
	bits uint8
	edge *flowEdge
	dist int
}

// flowMap is the graph placed on a grid of cells.
type flowMap struct {
	g        *flowGraph
	w, h     int
	vertical bool
	tight    bool // no blank line or column between neighbours: a map that fits
	marks    map[[2]int][]flowMark
	// order is the nodes as the cursor walks them: the interfaces,
	// HAProxy, the frontends, each backend then its servers.
	order []*flowNode
}

func sign(v int) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// path draws a link along a line of horizontal and vertical segments.
func (m *flowMap) path(e *flowEdge, net string, pts ...[2]int) {
	var cells [][2]int
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		dx, dy := sign(b[0]-a[0]), sign(b[1]-a[1])
		for p := a; ; p = [2]int{p[0] + dx, p[1] + dy} {
			if len(cells) == 0 || cells[len(cells)-1] != p {
				cells = append(cells, p)
			}
			if p == b {
				break
			}
		}
	}
	toward := func(c, n [2]int) uint8 {
		switch {
		case n[0] > c[0]:
			return dirE
		case n[0] < c[0]:
			return dirW
		case n[1] > c[1]:
			return dirS
		}
		return dirN
	}
	for i, c := range cells {
		var bits uint8
		if i > 0 {
			bits |= toward(c, cells[i-1])
		}
		if i+1 < len(cells) {
			bits |= toward(c, cells[i+1])
		}
		if bits == 0 { // a link one cell long
			bits = dirE | dirW
			if m.vertical {
				bits = dirN | dirS
			}
		}
		m.marks[c] = append(m.marks[c], flowMark{net: net, bits: bits, edge: e, dist: i})
	}
}

// lanes gives each net of a gap its lane - its line across the gap -
// sharing one between nets that don't overlap: nets are the links of a
// source, or of a target when every source of the gap has one link
// (interfaces into HAProxy). across is where a node meets the gap.
func lanes(g *flowGraph, edges []*flowEdge, across func(*flowNode) int) (net map[*flowEdge]string, lane map[string]int, n int) {
	outs := map[string]int{}
	for _, e := range edges {
		outs[e.from]++
	}
	byTarget := true
	for _, c := range outs {
		if c > 1 {
			byTarget = false
		}
	}
	net = map[*flowEdge]string{}
	type span struct {
		key    string
		lo, hi int
	}
	spans := map[string]*span{}
	var keys []string
	for _, e := range edges {
		k := "s:" + e.from
		if byTarget {
			k = "t:" + e.to
		}
		net[e] = k
		a, b := across(g.byID[e.from]), across(g.byID[e.to])
		s := spans[k]
		if s == nil {
			s = &span{key: k, lo: min(a, b), hi: max(a, b)}
			spans[k] = s
			keys = append(keys, k)
		}
		s.lo, s.hi = min(s.lo, a, b), max(s.hi, a, b)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := spans[keys[i]], spans[keys[j]]
		return a.lo < b.lo || (a.lo == b.lo && a.hi < b.hi)
	})
	lane = map[string]int{}
	var ends []int // the last across each lane reaches
	for _, k := range keys {
		s := spans[k]
		l := -1
		for i, end := range ends {
			if end < s.lo {
				l = i
				break
			}
		}
		if l < 0 {
			l = len(ends)
			ends = append(ends, 0)
		}
		ends[l] = s.hi
		lane[k] = l
	}
	return net, lane, len(ends)
}

// gapEdges are the links leaving a level (servers apart: placed with
// their backend).
func gapEdges(g *flowGraph, level int) []*flowEdge {
	var out []*flowEdge
	for _, e := range g.edges {
		if flowLevel[g.byID[e.from].kind] == level {
			out = append(out, e)
		}
	}
	return out
}

// rankedBackends are the backends in the order of the frontends that
// reach them, then the rest.
func rankedBackends(g *flowGraph) []*flowNode {
	rank := map[string]int{}
	for fi, f := range g.frontends {
		li := 0
		for _, e := range g.edges {
			if e.from == f.id {
				if _, ok := rank[e.to]; !ok {
					rank[e.to] = fi*10_000 + li
				}
				li++
			}
		}
	}
	out := append([]*flowNode(nil), g.backends...)
	pos := map[*flowNode]int{}
	for i, b := range out {
		pos[b] = i
	}
	key := func(b *flowNode) int {
		if r, ok := rank[b.id]; ok {
			return r
		}
		return 1e9 + pos[b]
	}
	sort.SliceStable(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}

// targets is the mean position of the backends a frontend sends to,
// false for one that sends nowhere known.
func targets(g *flowGraph, f *flowNode, at func(*flowNode) int) (float64, bool) {
	sum, n := 0, 0
	for _, e := range g.edges {
		if e.from == f.id {
			sum += at(g.byID[e.to])
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return float64(sum) / float64(n), true
}

// layoutFlow places the graph top to bottom (vertical) or left to
// right, tight or with room between its nodes, and routes its links.
func layoutFlow(g *flowGraph, vertical, tight bool) *flowMap {
	m := &flowMap{g: g, vertical: vertical, tight: tight, marks: map[[2]int][]flowMark{}}
	bes := rankedBackends(g)
	m.order = append(append(append(append(m.order, g.logs...), g.ifaces...), g.hub), g.frontends...)
	for _, b := range bes {
		m.order = append(append(m.order, b), b.servers...)
	}
	if vertical {
		m.placeDown(bes)
	} else {
		m.placeAcross(bes)
	}
	for _, n := range g.nodes {
		m.w = max(m.w, n.x+n.width())
		m.h = max(m.h, n.y+n.height())
	}
	return m
}

// placeAcross: columns - interfaces, HAProxy, frontends, backends,
// servers. Each backend is as tall as its servers; frontends sit level
// with their backends, HAProxy with its frontends.
func (m *flowMap) placeAcross(bes []*flowNode) {
	g := m.g
	space := m.space()
	y := 0
	for _, b := range bes {
		b.y = y
		for i, s := range b.servers {
			s.y = y + i
		}
		y += max(2, len(b.servers)) + space
	}
	type want struct {
		f  *flowNode
		i  int
		at float64
		ok bool
	}
	var ws []want
	for i, f := range g.frontends {
		at, ok := targets(g, f, func(n *flowNode) int { return n.y })
		ws = append(ws, want{f, i, at, ok})
	}
	sort.SliceStable(ws, func(i, j int) bool {
		if ws[i].ok != ws[j].ok {
			return ws[i].ok
		}
		return ws[i].at < ws[j].at
	})
	step := 2 + space // a frontend's or an interface's two lines, then the space
	prev := -step
	for _, w := range ws {
		fy := prev + step
		if w.ok {
			fy = max(int(math.Round(w.at)), fy)
		}
		w.f.y, prev = fy, fy
	}
	g.hub.y = middle(g.frontends, bes, func(n *flowNode) int { return n.y })
	for i, it := range g.ifaces {
		it.y = g.hub.y - (len(g.ifaces)-1)*step/2 + step*i
	}
	// Log targets level with their interface, a step apart; those no
	// interface on the map says, after.
	m.placeLogs(func(it *flowNode) int { return it.y }, func(n *flowNode, at int) { n.y = at }, step, g.hub.y-(len(g.ifaces)-1)*step/2)
	top := 0
	for _, n := range g.nodes {
		top = min(top, n.y)
	}
	for _, n := range g.nodes {
		n.y -= top
	}

	// Columns, each as wide as its widest label, and the gaps between
	// them as wide as their lanes.
	var cols [flowLevels]int
	var present [flowLevels]bool
	for _, n := range g.nodes {
		l := flowLevel[n.kind]
		cols[l], present[l] = max(cols[l], n.width()), true
	}
	x := 0
	var colX, gapX [flowLevels]int
	nets := map[*flowEdge]string{}
	laneOf := map[*flowEdge]int{}
	for l := 0; l < flowLevels; l++ {
		if !present[l] {
			continue
		}
		colX[l] = x
		x += cols[l]
		next := l + 1
		for next < flowLevels && !present[next] {
			next++
		}
		if next == flowLevels {
			break
		}
		edges := gapEdges(g, l)
		net, lane, n := lanes(g, edges, func(n *flowNode) int { return n.y })
		for _, e := range edges {
			nets[e], laneOf[e] = net[e], lane[net[e]]
		}
		gapX[l] = x
		if n > 0 {
			x += n + 2
		} else {
			x += 3
		}
	}
	for _, n := range g.nodes {
		n.x = colX[flowLevel[n.kind]]
	}
	for _, e := range g.edges {
		a, b := g.byID[e.from], g.byID[e.to]
		start := a.x + 2 + a.nameWidth() + 1
		lx := gapX[flowLevel[a.kind]] + 1 + laneOf[e]
		m.path(e, nets[e], [2]int{start, a.y}, [2]int{lx, a.y}, [2]int{lx, b.y}, [2]int{b.x - 1, b.y})
	}
}

// placeLogs places the log targets along the interfaces' column (or
// row): each level with its interface - at is an interface's place -,
// several on one a step apart, those no interface on the map says after
// the others, from start on.
func (m *flowMap) placeLogs(at func(*flowNode) int, set func(*flowNode, int), step, start int) {
	type want struct {
		n  *flowNode
		i  int
		at int
		ok bool
	}
	var ws []want
	for i, n := range m.g.logs {
		w := want{n: n, i: i}
		for _, e := range m.g.edges {
			if e.from == n.id {
				w.at, w.ok = at(m.g.byID[e.to]), true
			}
		}
		ws = append(ws, w)
	}
	sort.SliceStable(ws, func(i, j int) bool {
		if ws[i].ok != ws[j].ok {
			return ws[i].ok
		}
		return ws[i].at < ws[j].at
	})
	prev := start - step
	for _, w := range ws {
		p := prev + step
		if w.ok {
			p = max(w.at, p)
		}
		set(w.n, p)
		prev = p
	}
}

// space is the blank lines between two neighbours across the flow - one,
// none when tight.
func (m *flowMap) space() int {
	if m.tight {
		return 0
	}
	return 1
}

// middle is the middle of the frontends, or of the backends without
// any: where HAProxy goes.
func middle(fes, bes []*flowNode, at func(*flowNode) int) int {
	ns := fes
	if len(ns) == 0 {
		ns = bes
	}
	if len(ns) == 0 {
		return 0
	}
	lo, hi := at(ns[0]), at(ns[0])
	for _, n := range ns {
		lo, hi = min(lo, at(n)), max(hi, at(n))
	}
	return (lo + hi) / 2
}

// placeDown: rows - interfaces, HAProxy, frontends, backends with their
// servers hanging below them. Frontends sit above the middle of their
// backends, HAProxy above its frontends.
func (m *flowMap) placeDown(bes []*flowNode) {
	g := m.g
	space := 1 + m.space() // columns between two labels
	x := 0
	for _, b := range bes {
		b.x = x
		w := b.width()
		for _, s := range b.servers {
			s.x = b.x + 2
			w = max(w, 2+s.width())
		}
		x += w + space
	}
	type want struct {
		f  *flowNode
		at float64
		ok bool
	}
	var ws []want
	for _, f := range g.frontends {
		at, ok := targets(g, f, func(n *flowNode) int { return n.x })
		ws = append(ws, want{f, at, ok})
	}
	sort.SliceStable(ws, func(i, j int) bool {
		if ws[i].ok != ws[j].ok {
			return ws[i].ok
		}
		return ws[i].at < ws[j].at
	})
	var prev *flowNode
	for _, w := range ws {
		fx := 0
		if prev != nil {
			fx = prev.x + prev.width() + space
		}
		if w.ok {
			fx = max(int(math.Round(w.at)), fx)
		}
		w.f.x, prev = fx, w.f
	}
	g.hub.x = middle(g.frontends, bes, func(n *flowNode) int { return n.x })
	step := 0
	for _, it := range g.ifaces {
		step = max(step, it.width()+space)
	}
	for i, it := range g.ifaces {
		it.x = g.hub.x - (len(g.ifaces)-1)*step/2 + step*i
	}
	// Log targets above their interface, their labels apart.
	logStep := 0
	for _, n := range g.logs {
		logStep = max(logStep, n.width()+space)
	}
	m.placeLogs(func(it *flowNode) int { return it.x }, func(n *flowNode, at int) { n.x = at }, logStep, g.hub.x-(len(g.ifaces)-1)*step/2)
	left := 0
	for _, n := range g.nodes {
		left = min(left, n.x)
	}
	for _, n := range g.nodes {
		n.x -= left
	}

	// Rows, and the gaps between them as tall as their lanes.
	levels := [][]*flowNode{g.logs, g.ifaces, {g.hub}, g.frontends, bes}
	y := 0
	var gapY [flowLevels]int
	nets := map[*flowEdge]string{}
	laneOf := map[*flowEdge]int{}
	placed := false
	for l, ns := range levels {
		if len(ns) == 0 {
			continue
		}
		if placed {
			// The gap from the level above.
			above := l - 1
			for above >= 0 && len(levels[above]) == 0 {
				above--
			}
			edges := gapEdges(g, above)
			net, lane, n := lanes(g, edges, func(n *flowNode) int { return n.x })
			for _, e := range edges {
				nets[e], laneOf[e] = net[e], lane[net[e]]
			}
			gapY[above] = y
			if n > 0 {
				y += n + 2
			} else {
				y++
			}
		}
		for _, n := range ns {
			n.y = y
		}
		y += 2
		placed = true
	}
	for _, b := range bes {
		for i, s := range b.servers {
			s.y = b.y + 2 + i
		}
	}
	for _, e := range g.edges {
		a, b := g.byID[e.from], g.byID[e.to]
		if a.kind == kindBE { // the servers' tree, below their backend
			m.path(e, "tree:"+a.id, [2]int{a.x, a.y + 1}, [2]int{a.x, b.y}, [2]int{b.x - 1, b.y})
			continue
		}
		ly := gapY[flowLevel[a.kind]] + 1 + laneOf[e]
		m.path(e, nets[e], [2]int{a.x, a.y + 1}, [2]int{a.x, ly}, [2]int{b.x, ly}, [2]int{b.x, b.y - 1})
	}
}

// flowDraw is how the map is drawn this frame.
type flowDraw struct {
	sel     string          // the node under the cursor, "" none
	near    map[string]bool // what's brought forward; nil everything
	phase   int             // the animation's step
	animate bool
	flash   map[string]bool // the nodes whose numbers just started from nothing
}

var kindColor = map[string]termui.Color{
	kindLog: termui.ColorLog, kindIface: termui.ColorAccent, kindHub: termui.ColorAccent, kindFE: termui.ColorInfo, kindBE: termui.ColorDefault, kindSrv: termui.ColorOK, kindMore: termui.ColorMuted,
}

// protoColor is an active link's colour: its protocol's, the text's for
// an interface's (all of its traffic).
var protoColor = map[string]termui.Color{"http": termui.ColorAccent, "tcp": termui.ColorTCP, "log": termui.ColorLog, "": termui.ColorDefault}

func toneColor(tone string, fallback termui.Color) termui.Color {
	switch tone {
	case "warn":
		return termui.ColorWarn
	case "danger":
		return termui.ColorDanger
	case "off":
		return termui.ColorMuted
	}
	return fallback
}

// cell is what the map shows at x, y of the grid: a line's glyph and
// style, false for none.
func (m *flowMap) cell(x, y int, d flowDraw) (rune, termui.Style, bool) {
	marks := m.marks[[2]int{x, y}]
	if len(marks) == 0 {
		return 0, termui.Style{}, false
	}
	// One net, or a net meeting itself: its lines join. Two nets crossing
	// each other straight: the lane's line goes through, so a crossing
	// never reads as a junction.
	byNet := map[string]uint8{}
	for _, mk := range marks {
		byNet[mk.net] |= mk.bits
	}
	shown := marks
	if len(byNet) > 1 {
		bends := false
		for _, b := range byNet {
			if b&(dirN|dirS) != 0 && b&(dirE|dirW) != 0 {
				bends = true
			}
		}
		if !bends {
			lane := dirN | dirS // left to right, lanes run down
			if m.vertical {
				lane = dirE | dirW
			}
			var keep []flowMark
			for _, mk := range marks {
				if mk.bits&lane != 0 {
					keep = append(keep, mk)
				}
			}
			if len(keep) > 0 {
				shown = keep
			}
		}
	}
	var bits uint8
	look, dist, near := -1, -1, d.near == nil
	var active *flowEdge
	for _, mk := range shown {
		bits |= mk.bits
		if mk.edge.look > look {
			look = mk.edge.look
		}
		if mk.edge.look == lookActive && (dist < 0 || mk.dist < dist) {
			dist, active = mk.dist, mk.edge
		}
		if d.near[mk.edge.id] {
			near = true
		}
	}
	r := boxGlyph[bits]
	st := termui.Style{FG: termui.ColorMuted}
	switch {
	case !near:
		st.Dim = true
	case look == lookActive:
		st = termui.Style{FG: protoColor[active.proto]}
		// Traffic: a heavier stretch running along the link - out of the
		// node on a log target's, drawn from the target.
		step := dist - d.phase
		if active.kind == kindLog {
			step = dist + d.phase
		}
		if d.animate && dist >= 0 && ((step%4)+4)%4 == 0 {
			switch r {
			case '─':
				r = '━'
			case '│':
				r = '┃'
			}
		}
	case look == lookDown:
		st = termui.Style{FG: termui.ColorDanger}
		switch r {
		case '─':
			r = '╌'
		case '│':
			r = '╎'
		}
	case look == lookOff:
		st.Dim = true
		switch r {
		case '─':
			r = '┄'
		case '│':
			r = '┆'
		}
	}
	return r, st, true
}

// draw paints the map's part that shows in r, from the grid's ox, oy.
func (m *flowMap) draw(f *termui.Frame, r termui.Rect, ox, oy int, d flowDraw) {
	put := func(x, y int, ch rune, st termui.Style) {
		sx, sy := r.X+x-ox, r.Y+y-oy
		if sx >= r.X && sx < r.X+r.W && sy >= r.Y && sy < r.Y+r.H {
			f.Set(sx, sy, ch, st)
		}
	}
	text := func(x, y int, s string, st termui.Style) int {
		n := 0
		for _, ch := range s {
			put(x+n, y, ch, st)
			n++
		}
		return n
	}
	for y := oy; y < oy+r.H && y < m.h; y++ {
		for x := ox; x < ox+r.W && x < m.w; x++ {
			if ch, st, ok := m.cell(x, y, d); ok {
				put(x, y, ch, st)
			}
		}
	}
	for _, n := range m.g.nodes {
		far := d.near != nil && !d.near[n.id]
		dot := '●'
		if (n.kind == kindBE && m.g.configKnown && !n.linked) || (n.kind == kindLog && !n.linked) {
			dot = '○' // no frontend rule names it; no interface on the map is the log target's
		}
		dotSt := termui.Style{FG: toneColor(n.tone, kindColor[n.kind])}
		nameSt := termui.Style{Bold: true}
		valSt := termui.Style{FG: toneColor(n.valuesTone, termui.ColorMuted)}
		tagSt := termui.Style{FG: protoColor[n.tag()]}
		if n.tone == "off" || dot == '○' {
			nameSt = termui.Style{}
		}
		if d.flash[n.id] { // a number that started from nothing
			valSt = termui.Style{FG: termui.ColorAccent, Bold: true, Reverse: true}
		}
		if far {
			dotSt, nameSt, valSt = termui.Style{FG: termui.ColorMuted, Dim: true}, termui.Style{FG: termui.ColorMuted, Dim: true}, termui.Style{FG: termui.ColorMuted, Dim: true}
			tagSt = valSt
		}
		if n.id == d.sel {
			nameSt = styleCursor
			nameSt.Bold = true
		}
		put(n.x, n.y, dot, dotSt)
		w := text(n.x+2, n.y, n.shownName(), nameSt)
		if t := n.tag(); t != "" {
			w += 1 + text(n.x+3+w, n.y, t, tagSt)
		}
		if n.oneLine() {
			text(n.x+3+w, n.y, n.shownValues(), valSt)
		} else {
			text(n.x+2, n.y+1, n.shownValues(), valSt)
		}
	}
}
