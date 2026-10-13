package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/haproxyflow"
	"github.com/swenske/Janus/internal/haproxystat"
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
	golden(t, "tui-flow-vertical-160x48.txt", a.render(160, 48))
	a.settings.FlowDirection = "horizontal"
	golden(t, "tui-flow-horizontal-120x40.txt", a.render(120, 40))

	// A server selected: its paths forward, its counters below.
	a.settings.FlowDirection = "vertical"
	n.flow.sel = "srv:be_blog/dead"
	golden(t, "tui-flow-selected-120x40.txt", a.render(120, 40))
}

func TestFlowMoves(t *testing.T) {
	a, n := flowApp(t)
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
	a.key("o")
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
