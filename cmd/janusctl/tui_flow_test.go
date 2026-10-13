package main

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/haproxyflow"
	"github.com/swenske/Janus/internal/haproxystat"
	"github.com/swenske/Janus/internal/termui"
)

// A node's HAProxy with a map, a down server, one in maintenance, more
// servers than shown, a listen section, a TCP frontend, a backend no
// rule names - two samples two seconds apart.
const (
	flowCfg = `
defaults
    mode http
frontend https
    bind :443 ssl crt /etc/haproxy/files/certs/
    acl is_api path_beg /api
    use_backend api if is_api
    use_backend %[req.hdr(host),lower,map(/etc/haproxy/files/hosts.map,be_default)]
frontend admin
    bind :81
    http-request return status 200 content-type text/plain string "ok"
listen stats
    bind :8404
    stats enable
frontend tcp_in
    mode tcp
    bind :5000
    default_backend tcp_pool
backend be_default
backend be_www
backend be_blog
backend be_many
backend api
backend tcp_pool
backend unused
`
	flowStatHead = "# pxname,svname,qcur,scur,smax,slim,stot,bin,bout,status,weight,lastchg,rate,rate_max,check_status,check_desc,req_rate,hrsp_2xx,hrsp_3xx,hrsp_4xx,hrsp_5xx,addr,mode,\n"
)

// flowStat is show stat with every byte counter times k.
func flowStat(k uint64) string {
	var b strings.Builder
	b.WriteString(flowStatHead)
	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	row := func(px, sv, status string, scur, bytes uint64, check, addr string) {
		b.WriteString(strings.Join([]string{px, sv, "0", u(scur), u(scur + 2), "", "100", u(bytes * k), u(bytes * 4 * k), status, "1", "30", "1", "4", check, "", "2", "90", "0", "3", "1", addr, "http"}, ",") + ",\n")
	}
	row("https", "FRONTEND", "OPEN", 8, 9000, "", "")
	row("admin", "FRONTEND", "OPEN", 0, 0, "", "")
	row("stats", "FRONTEND", "OPEN", 0, 0, "", "")
	row("tcp_in", "FRONTEND", "OPEN", 1, 400, "", "")
	for _, s := range []struct {
		be, sv, status string
		scur, bytes    uint64
		check, addr    string
	}{
		{"be_default", "app1", "UP", 1, 1200, "L7OK", "10.0.10.11:8080"},
		{"be_www", "app1", "UP", 2, 2000, "L7OK", "10.0.10.11:8080"},
		{"be_www", "app2", "UP", 2, 1800, "L7OK", "10.0.10.12:8080"},
		{"be_blog", "app2", "UP", 1, 2500, "L7OK", "10.0.10.12:8080"},
		{"be_blog", "dead", "DOWN", 0, 0, "* L4TOUT", "10.0.10.99:8080"},
		{"api", "app1", "UP", 1, 500, "L7OK", "10.0.10.11:8080"},
		{"api", "app2", "MAINT", 0, 0, "", "10.0.10.12:8080"},
		{"tcp_pool", "app1", "UP", 1, 400, "L4OK", "10.0.10.11:8080"},
		{"unused", "app1", "UP", 0, 0, "L4OK", "10.0.10.11:8080"},
		{"stats", "BACKEND", "UP", 0, 0, "", ""},
	} {
		row(s.be, s.sv, s.status, s.scur, s.bytes, s.check, s.addr)
	}
	for i := range 10 {
		row("be_many", "s"+strconv.Itoa(i+1), "UP", 0, 100, "L7OK", "10.0.10.11:8080")
	}
	for _, be := range []string{"be_default", "be_www", "be_blog", "be_many", "api", "tcp_pool", "unused"} {
		row(be, "BACKEND", "UP", 1, 1000, "", "")
	}
	return b.String()
}

// flowApp is a node screen on the traffic flow of flowCfg's node, its
// configuration and map already read.
func flowApp(t *testing.T) (*app, *nodeScreen) {
	t.Helper()
	a := testApp(t)
	s := testSampler(t, a, "lgslbpub01")
	prev, cur := testSamples(t)
	var err error
	if prev.stat, err = haproxystat.Parse([]byte(flowStat(1))); err != nil {
		t.Fatal(err)
	}
	if cur.stat, err = haproxystat.Parse([]byte(flowStat(3))); err != nil {
		t.Fatal(err)
	}
	s.prev, s.cur = prev, cur
	s.last = derive(prev, cur)
	a.samplers = []*sampler{s}
	n := newNodeScreen(s, false)
	n.flow = newFlowView(n)
	top := haproxyflow.Parse(flowCfg)
	n.flow.cfg = flowConfig{top: &top, maps: map[string]map[string]string{"/etc/haproxy/files/hosts.map": {
		"www.example.com": "be_www", "example.com": "be_www", "blog.example.com": "be_blog", "many.example.com": "be_many",
	}}, gen: 1}
	a.screen = n
	return a, n
}

func TestFlowGolden(t *testing.T) {
	a, n := flowApp(t)
	a.settings.FlowDirection = "vertical"
	golden(t, "tui-flow-vertical-160x48.txt", a.render(160, 48))
	// Left to right, tight: with room between its backends it's too tall.
	a.settings.FlowDirection = "horizontal"
	golden(t, "tui-flow-horizontal-120x40.txt", a.render(120, 40))
	if m := n.flow.m; !m.tight {
		t.Errorf("120x40: the map is %dx%d, tight would fit", m.w, m.h)
	}

	// A server selected: its paths forward, its counters below.
	a.settings.FlowDirection = "vertical"
	n.flow.sel = "srv:be_blog/dead"
	golden(t, "tui-flow-selected-120x40.txt", a.render(120, 40))
}

func TestFlowMoves(t *testing.T) {
	a, n := flowApp(t)
	a.settings.FlowDirection = "vertical"
	a.render(160, 48)
	press := func(keys ...string) string {
		for _, k := range keys {
			a.key(k)
		}
		a.render(160, 48)
		return n.flow.sel
	}
	if got := press("\x1b[B"); got != "hub" {
		t.Fatalf("the first arrow selects HAProxy, got %q", got)
	}
	// Top to bottom: down follows the traffic, right goes along a level.
	if got := press("\x1b[B"); got != "fe:https" {
		t.Errorf("down from HAProxy: %q", got)
	}
	if got := press("\x1b[B"); got != "be:api" {
		t.Errorf("down from https: %q", got)
	}
	// The backends in the order https's rules name them: api, then the
	// map's by key.
	if got := press("\x1b[C"); got != "be:be_blog" {
		t.Errorf("right from api: %q", got)
	}
	if got := press("\x1b[B"); got != "srv:be_blog/app2" {
		t.Errorf("down from be_blog: %q", got)
	}
	if got := press("\x1b[A"); got != "be:be_blog" {
		t.Errorf("up from its server: %q", got)
	}
	// Left to right: right follows the traffic.
	a.settings.FlowDirection = "horizontal"
	a.render(160, 48)
	if got := press("\x1b[C"); got != "srv:be_blog/app2" {
		t.Errorf("right from be_blog, left to right: %q", got)
	}
	// Enter on a server: its state dialog.
	a.key("\r")
	if a.modal == nil || !strings.Contains(a.modal.act.title, "be_blog/app2") {
		t.Fatalf("Enter on a server: %+v", a.modal)
	}
	a.key("\x1b")
	// Esc: the selection first, then back to the panels.
	a.key("\x1b")
	if n.flow == nil || n.flow.sel != "" {
		t.Fatal("Esc clears the selection first")
	}
	a.key("\x1b")
	if n.flow != nil {
		t.Fatal("Esc again leaves the traffic flow")
	}
	a.key("f")
	if n.flow == nil {
		t.Fatal("f opens it")
	}
	n.flow.stop()
}

// Auto: left to right when it fits, tight when that's what fits, top to
// bottom when only that fits, else what hides less; the details beside
// the map on a wide screen - narrower for a wide map - or below it.
func TestFlowArrange(t *testing.T) {
	sizes := map[[2]bool][2]int{
		{false, false}: {100, 30}, {false, true}: {100, 20}, // left to right: tall
		{true, false}: {150, 12}, {true, true}: {140, 12}, // top to bottom: wide
	}
	lay := func(vertical, tight bool) *flowMap {
		s := sizes[[2]bool{vertical, tight}]
		return &flowMap{vertical: vertical, tight: tight, w: s[0], h: s[1]}
	}
	for _, c := range []struct {
		direction       string
		w, h            int
		vertical, tight bool
		side            termui.Rect
	}{
		{"auto", 200, 40, false, false, termui.Rect{X: 154, Y: 1, W: 46, H: 40}},
		{"auto", 200, 24, false, true, termui.Rect{X: 154, Y: 1, W: 46, H: 24}},
		{"auto", 200, 16, true, false, termui.Rect{X: 154, Y: 1, W: 46, H: 16}},
		{"auto", 180, 16, true, true, termui.Rect{X: 144, Y: 1, W: 36, H: 16}}, // the details narrower
		{"auto", 120, 16, true, true, termui.Rect{X: 0, Y: 9, W: 120, H: 8}},   // nothing fits
		{"horizontal", 120, 16, false, true, termui.Rect{X: 0, Y: 9, W: 120, H: 8}},
		{"vertical", 200, 40, true, false, termui.Rect{X: 154, Y: 1, W: 46, H: 40}},
		{"vertical", 170, 40, true, false, termui.Rect{X: 0, Y: 29, W: 170, H: 12}}, // fits with the details below
	} {
		m, mapR, side := arrangeFlow(c.direction, termui.Rect{X: 0, Y: 1, W: c.w, H: c.h}, 0, lay)
		if m.vertical != c.vertical || m.tight != c.tight || side != c.side {
			t.Errorf("%s in %dx%d: vertical %v tight %v, details %+v; want %v %v %+v", c.direction, c.w, c.h, m.vertical, m.tight, side, c.vertical, c.tight, c.side)
		}
		if mapR.W+side.W != c.w && mapR.H+side.H != c.h {
			t.Errorf("%s in %dx%d: map %+v and details %+v don't share the screen", c.direction, c.w, c.h, mapR, side)
		}
	}
	// A map needing the columns: the details narrower, down to theirs.
	sizes[[2]bool{false, false}] = [2]int{110, 30}
	if _, _, side := arrangeFlow("auto", termui.Rect{X: 0, Y: 1, W: 160, H: 40}, 0, lay); side.W != 46 {
		t.Errorf("a map with room: details %d wide", side.W)
	}
	sizes[[2]bool{false, false}] = [2]int{114, 30}
	if _, _, side := arrangeFlow("auto", termui.Rect{X: 0, Y: 1, W: 160, H: 40}, 0, lay); side.W != 42 {
		t.Errorf("a map 114 wide in 160: details %d wide, want 42", side.W)
	}
	// Notes above the map take its lines.
	if m, _, _ := arrangeFlow("auto", termui.Rect{X: 0, Y: 1, W: 200, H: 24}, 3, lay); m.vertical != true || m.tight {
		t.Errorf("three notes over 24 lines: vertical %v tight %v", m.vertical, m.tight)
	}
}

// The map holds still as traffic comes and goes: a node's numbers have
// room kept for them, and what they took once stays theirs.
func TestFlowHoldsStill(t *testing.T) {
	a, n := flowApp(t)
	a.settings.FlowDirection = "horizontal"
	s := a.samplers[0]
	sample := func(k1, k2 uint64) {
		var err error
		if s.prev.stat, err = haproxystat.Parse([]byte(flowStat(k1))); err != nil {
			t.Fatal(err)
		}
		if s.cur.stat, err = haproxystat.Parse([]byte(flowStat(k2))); err != nil {
			t.Fatal(err)
		}
		s.last = derive(s.prev, s.cur)
		s.rounds++
	}
	at := func() map[string][2]int {
		a.render(160, 48)
		out := map[string][2]int{}
		for _, nd := range n.flow.m.g.nodes {
			out[nd.id] = [2]int{nd.x, nd.y}
		}
		return out
	}
	sample(1, 1) // idle: "0B/s · 0 sess" everywhere
	idle := at()
	sample(1, 30) // busy: ten times v2026.10.13's busy sample
	if busy := at(); fmt.Sprint(busy) != fmt.Sprint(idle) {
		t.Errorf("traffic moved the map:\nidle %v\nbusy %v", idle, busy)
	}
	// Past the room kept: the labels grow once, then never shrink back.
	sample(1, 300000)
	huge := at()
	sample(1, 1)
	if back := at(); fmt.Sprint(back) != fmt.Sprint(huge) {
		t.Errorf("the map moved back when traffic fell:\nbusy %v\nidle %v", huge, back)
	}
}

// o shows the other direction, then auto again.
func TestFlowNextDirection(t *testing.T) {
	a, n := flowApp(t)
	a.render(160, 48)
	if a.settings.FlowDirection != "auto" || n.flow.m.vertical {
		t.Fatalf("by default, auto and left to right: %q vertical %v", a.settings.FlowDirection, n.flow.m.vertical)
	}
	for _, want := range []string{"vertical", "auto", "vertical"} {
		a.key("o")
		a.render(160, 48)
		if a.settings.FlowDirection != want {
			t.Errorf("o: %q, want %q", a.settings.FlowDirection, want)
		}
	}
	// Left to right asked, as auto shows it: o is top to bottom.
	a.settings.FlowDirection = "horizontal"
	a.render(160, 48)
	if got := n.flow.nextDirection(a); got != "vertical" {
		t.Errorf("after left to right: %q", got)
	}
}

// Links that cross are drawn straight through each other, never as a
// junction; a node's links join.
func TestFlowLines(t *testing.T) {
	a, n := flowApp(t)
	a.settings.FlowDirection = "horizontal"
	f := a.render(160, 48)
	m := n.flow.m
	for _, e := range m.g.edges {
		if len(e.id) == 0 {
			t.Fatal("an edge without id")
		}
	}
	text := strings.Join(f.Lines(), "\n")
	for _, want := range []string{"HAProxy", "https", "be_www", "+3 more", "dead", "○ unused"} {
		if !strings.Contains(text, want) {
			t.Errorf("the map lacks %q", want)
		}
	}
	// Every link is drawn from its node to the next.
	if len(m.marks) == 0 {
		t.Fatal("no line drawn")
	}
}

// Two links crossing are drawn straight through each other - the
// lane's line goes on - never as a junction; one net's lines join.
func TestFlowCrossing(t *testing.T) {
	e1, e2 := &flowEdge{id: "a", look: lookIdle}, &flowEdge{id: "b", look: lookIdle}
	m := &flowMap{marks: map[[2]int][]flowMark{
		{0, 0}: {{net: "s:a", bits: dirE | dirW, edge: e1}, {net: "s:b", bits: dirN | dirS, edge: e2}},
		{1, 0}: {{net: "s:a", bits: dirE | dirW, edge: e1}, {net: "s:a", bits: dirW | dirS, edge: e1}},
	}}
	if r, _, _ := m.cell(0, 0, flowDraw{}); r != '│' {
		t.Errorf("left to right, a crossing is the lane's line: %q", r)
	}
	m.vertical = true
	if r, _, _ := m.cell(0, 0, flowDraw{}); r != '─' {
		t.Errorf("top to bottom, a crossing is the lane's line: %q", r)
	}
	if r, _, _ := m.cell(1, 0, flowDraw{}); r != '┬' {
		t.Errorf("one net's lines join: %q", r)
	}
	// Traffic: a heavier stretch every four cells, moving on.
	e1.look = lookActive
	m.marks[[2]int{2, 0}] = []flowMark{{net: "s:a", bits: dirE | dirW, edge: e1, dist: 5}}
	if r, _, _ := m.cell(2, 0, flowDraw{animate: true, phase: 1}); r != '━' {
		t.Errorf("phase 1 at 5: %q", r)
	}
	if r, _, _ := m.cell(2, 0, flowDraw{animate: true, phase: 2}); r != '─' {
		t.Errorf("phase 2 at 5: %q", r)
	}
	if r, _, _ := m.cell(2, 0, flowDraw{phase: 1}); r != '─' {
		t.Errorf("no animation: %q", r)
	}
}

// Without the configuration - the role may not read it - the map has
// the proxies, servers under their backends, no frontend linked, and
// says why.
func TestFlowWithoutConfig(t *testing.T) {
	a, n := flowApp(t)
	n.flow.cfg = flowConfig{denied: true, err: "PermissionDenied", gen: 2}
	text := strings.Join(a.render(160, 48).Lines(), "\n")
	if !strings.Contains(text, "Your role doesn't read HAProxy's configuration") {
		t.Errorf("the refusal must be said:\n%s", text)
	}
	g := n.flow.m.g
	for _, e := range g.edges {
		if e.kind == kindFE {
			t.Errorf("a frontend linked without the configuration: %s", e.id)
		}
	}
	if len(g.byID["be:be_www"].servers) != 2 {
		t.Error("servers stay under their backend")
	}
	// No backend is drawn as named by no rule: that isn't known.
	a.settings.FlowDirection = "horizontal"
	if wide := strings.Join(a.render(200, 60).Lines(), "\n"); !strings.Contains(wide, "● unused") {
		t.Errorf("unused, without the configuration, is a backend like the others:\n%s", wide)
	}
}
