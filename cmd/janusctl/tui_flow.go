package main

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/haproxyflow"
	"github.com/swenske/Janus/internal/haproxystat"
	"github.com/swenske/Janus/internal/termui"
)

// The node screen's traffic flow (f): the map (tui_flowmap.go) in the
// screen, a cursor walking its nodes - the paths through the one under
// it brought forward, its counters and rules beside or below the map -
// and the configuration and maps it needs, read from the node in the
// background.

const (
	flowConfigEvery = 30 * time.Second // the configuration and maps change seldom
	flowFrame       = 150 * time.Millisecond
	flowFlash       = 1200 * time.Millisecond // a number that started from nothing, highlighted
	flowSideWidth   = 46                      // the details beside the map, from
	flowSideFrom    = 150                     // columns
	flowSideMin     = 36                      // narrower when the map needs it
)

// flowConfig is what the node said of its configuration.
type flowConfig struct {
	top     *haproxyflow.Topology // nil: not read (yet, or refused)
	maps    map[string]map[string]string
	mapErrs map[string]string
	err     string // why it couldn't be read
	denied  bool   // the role may not read it
	gen     int    // counts the answers: a new map is laid out again
}

type flowView struct {
	n      *nodeScreen
	mu     sync.Mutex
	cfg    flowConfig
	kick   chan struct{}
	cancel context.CancelFunc

	sel    string // the node under the cursor, "" none
	ox, oy int    // the map's cell at the top left of its box
	uptime uint64 // HAProxy's, last seen: less is a reload, read the configuration again

	// The map laid out each way it can be - top to bottom or not, tight
	// or not - kept while nothing it shows changed; m is the one shown.
	laidOut string
	maps    map[[2]bool]*flowMap
	m       *flowMap
	// autoShown is the direction auto last chose (vertical), nil none
	// yet: o goes back to auto from the other one.
	autoShown *bool
	// room is the cells each node's numbers took at most since the map
	// opened: a label grows with them and never shrinks back, so the map
	// moves once at most as traffic rises, never back and forth.
	room map[string]int
	// The flash: each node's numbers at the last round seen, and until
	// when one that started from nothing is highlighted - or, without
	// the animation, the round it did in.
	figRound   int
	figures    map[string][]float64
	flashUntil map[string]time.Time
	flashRound map[string]int
}

// flowFigures are each node's numbers, the flash watches: an
// interface's traffic, HAProxy's connections and requests, a frontend's,
// backend's or server's traffic and sessions, a log target's records
// and bytes a second (NaN unknown).
func flowFigures(g *flowGraph, v *nodeView) map[string][]float64 {
	out := map[string][]float64{}
	for _, n := range g.nodes {
		switch {
		case n.kind == kindIface:
			out[n.id] = []float64{n.traffic}
		case n.kind == kindHub && v.last.hap.ok:
			out[n.id] = []float64{float64(v.last.hap.conns), v.last.hap.reqRate}
		case n.kind == kindLog:
			out[n.id] = []float64{n.log.msgRate, n.traffic}
		case n.kind == kindFE:
			out[n.id] = []float64{n.traffic, float64(n.fe.scur)}
		case n.row != nil:
			out[n.id] = []float64{n.traffic, float64(n.row.scur)}
		}
	}
	return out
}

// risenFigures are the nodes one of whose numbers was 0 and is more now -
// not one unknown before (NaN: the first round, a counter that went back).
func risenFigures(prev, cur map[string][]float64) []string {
	var out []string
	for id, vs := range cur {
		p, ok := prev[id]
		if !ok || len(p) != len(vs) {
			continue
		}
		for i, x := range vs {
			if p[i] == 0 && x > 0 {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// watchFlash compares this round's numbers with the last one's, once a
// round, and says which nodes flash now.
func (fv *flowView) watchFlash(a *app, m *flowMap, v *nodeView) map[string]bool {
	now := a.now()
	if fv.flashUntil == nil {
		fv.flashUntil, fv.flashRound = map[string]time.Time{}, map[string]int{}
	}
	if v.rounds != fv.figRound {
		cur := flowFigures(m.g, v)
		for _, id := range risenFigures(fv.figures, cur) {
			fv.flashUntil[id], fv.flashRound[id] = now.Add(flowFlash), v.rounds
		}
		fv.figures, fv.figRound = cur, v.rounds
	}
	on := map[string]bool{}
	for id, until := range fv.flashUntil {
		if a.settings.FlowAnimation && now.Before(until) || !a.settings.FlowAnimation && fv.flashRound[id] == v.rounds {
			on[id] = true
		}
	}
	return on
}

// flashing reports whether a flash is on, the animation's frames needed
// for it to end.
func (fv *flowView) flashing(now time.Time) bool {
	for _, until := range fv.flashUntil {
		if now.Before(until) {
			return true
		}
	}
	return false
}

func newFlowView(n *nodeScreen) *flowView {
	return &flowView{n: n, kick: make(chan struct{}, 1)}
}

// start reads the configuration now, then every flowConfigEvery or when
// kicked, until stop.
func (fv *flowView) start(wake chan<- struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	fv.cancel = cancel
	go func() {
		for {
			fv.read(ctx)
			select {
			case wake <- struct{}{}:
			default:
			}
			select {
			case <-ctx.Done():
				return
			case <-fv.kick:
			case <-time.After(flowConfigEvery):
			}
		}
	}()
}

func (fv *flowView) stop() {
	if fv.cancel != nil {
		fv.cancel()
	}
}

// read asks the node for its configuration and the maps a dynamic
// use_backend reads.
func (fv *flowView) read(ctx context.Context) {
	conn, err := fv.n.s.connection()
	if err != nil {
		fv.set(func(c *flowConfig) { c.err = err.Error() })
		return
	}
	hap := janusv1alpha1.NewHAProxyServiceClient(conn)
	cctx, cancel := context.WithTimeout(ctx, tuiFetchTimeout)
	defer cancel()
	resp, err := hap.GetConfig(cctx, &emptypb.Empty{})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		fv.set(func(c *flowConfig) {
			c.top, c.denied, c.err = nil, status.Code(err) == codes.PermissionDenied, status.Convert(err).Message()
		})
		return
	}
	top := haproxyflow.Parse(string(resp.GetConfig()))
	maps, errs := map[string]map[string]string{}, map[string]string{}
	for _, path := range top.Maps {
		m, err := hap.MapGet(cctx, &janusv1alpha1.MapGetRequest{Map: path})
		if err != nil {
			errs[path] = status.Convert(err).Message()
			continue
		}
		maps[path] = m.GetEntries()
	}
	fv.set(func(c *flowConfig) { c.top, c.maps, c.mapErrs, c.err, c.denied = &top, maps, errs, "", false })
}

func (fv *flowView) set(change func(*flowConfig)) {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	change(&fv.cfg)
	fv.cfg.gen++
}

func (fv *flowView) config() flowConfig {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	return fv.cfg
}

// laid is the map of the view's node laid out one way, again only when
// what it shows changed.
func (fv *flowView) laid(a *app, v *nodeView, vertical, tight bool) *flowMap {
	c := fv.config()
	key := fmt.Sprintf("%d/%d/%v", v.rounds, c.gen, a.settings.FlowServers)
	if key != fv.laidOut {
		fv.maps, fv.laidOut = map[[2]bool]*flowMap{}, key
	}
	m := fv.maps[[2]bool{vertical, tight}]
	if m == nil {
		g := buildFlow(v, c.top, c.maps, a.settings.FlowServers)
		if fv.room == nil {
			fv.room = map[string]int{}
		}
		for _, n := range g.nodes {
			n.room = max(flowValuesRoom[n.kind], len([]rune(n.shownValues())), fv.room[n.id])
			fv.room[n.id] = n.room
		}
		m = layoutFlow(g, vertical, tight)
		fv.maps[[2]bool{vertical, tight}] = m
	}
	return m
}

// arrange picks how the map shows in body, under its notes lines.
func (fv *flowView) arrange(a *app, v *nodeView, body termui.Rect, notes int) (*flowMap, termui.Rect, termui.Rect) {
	return arrangeFlow(a.settings.FlowDirection, body, notes, func(vertical, tight bool) *flowMap {
		return fv.laid(a, v, vertical, tight)
	})
}

// arrangeFlow is the direction asked or, auto, left to right when it
// fits, else top to bottom when that does, else the one hiding less of
// itself - with room between its nodes, tight when that's what makes it
// fit - and the details beside it on a wide screen, narrower when the
// map needs the columns, or below it when that hides less. lay lays the
// map out one way.
func arrangeFlow(direction string, body termui.Rect, notes int, lay func(vertical, tight bool) *flowMap) (m *flowMap, mapR, side termui.Rect) {
	directions := []bool{false, true}
	switch direction {
	case "horizontal":
		directions = []bool{false}
	case "vertical":
		directions = []bool{true}
	}
	best := math.Inf(1)
	for _, vertical := range directions {
		for _, tight := range []bool{false, true} {
			lm := lay(vertical, tight)
			var places [][2]termui.Rect
			if body.W >= flowSideFrom {
				sw := min(max(body.W-lm.w-4, flowSideMin), flowSideWidth)
				r := body
				r.W -= sw
				places = append(places, [2]termui.Rect{r, {X: r.W, Y: body.Y, W: sw, H: body.H}})
			}
			dh := min(max(body.H/3, 8), 12)
			r := body
			r.H -= dh
			places = append(places, [2]termui.Rect{r, {X: 0, Y: r.Y + r.H, W: body.W, H: dh}})
			for _, p := range places {
				in := mapInside(p[0], notes)
				hidden := 0.0
				if lm.w > in.W || lm.h > in.H {
					hidden = 1 - float64(min(lm.w, in.W)*min(lm.h, in.H))/float64(lm.w*lm.h)
				}
				if hidden < best {
					best, m, mapR, side = hidden, lm, p[0], p[1]
				}
				if hidden == 0 {
					return m, mapR, side
				}
			}
		}
	}
	return m, mapR, side
}

// mapInside is where the map draws in its box r, under notes lines.
func mapInside(r termui.Rect, notes int) termui.Rect {
	in := termui.Rect{X: r.X + 1, Y: r.Y + 1, W: max(r.W-2, 0), H: max(r.H-2, 0)}
	n := min(notes, max(in.H-2, 0))
	return termui.Rect{X: in.X + 1, Y: in.Y + n, W: max(in.W-2, 0), H: in.H - n}
}

// nextDirection is where o goes: the direction not shown - auto again
// when that's the one auto chose.
func (fv *flowView) nextDirection(a *app) string {
	shown := fv.m != nil && fv.m.vertical
	if a.settings.FlowDirection != "auto" && fv.autoShown != nil && *fv.autoShown != shown {
		return "auto"
	}
	if shown {
		return "horizontal"
	}
	return "vertical"
}

// active reports whether traffic shows on the map: then it's animated.
func (fv *flowView) active() bool {
	if fv.m == nil {
		return false
	}
	for _, e := range fv.m.g.edges {
		if e.look == lookActive {
			return true
		}
	}
	return false
}

func (fv *flowView) hints(a *app) []string {
	h := []string{"? help", "q quit", "f back", "←↑↓→ move"}
	if fv.sel != "" && fv.m != nil {
		if n := fv.m.g.byID[fv.sel]; n != nil && n.kind == kindSrv && roleAllows(a.opts.role, rolesOperators) {
			h = append(h, "Enter state")
		}
	}
	h = append(h, "o "+fv.nextDirection(a), "s servers")
	if roleAllows(a.opts.role, rolesOperators) {
		h = append(h, "R reload")
	}
	return append(h, "M menu")
}

// key handles a key on the traffic flow; false when the node screen
// should take it.
func (fv *flowView) key(a *app, k string) bool {
	m := fv.m
	switch k {
	case "f":
		fv.close()
	case termui.KeyEsc, termui.KeyBackspace:
		if fv.sel != "" {
			fv.sel = ""
		} else {
			fv.close()
		}
	case "o":
		a.settings.FlowDirection = fv.nextDirection(a)
		a.say("traffic flow: "+a.settings.FlowDirection, termui.ColorDefault)
	case "s":
		a.settings.FlowServers = !a.settings.FlowServers
		if a.settings.FlowServers {
			a.say("servers shown", termui.ColorDefault)
		} else {
			a.say("servers hidden", termui.ColorDefault)
		}
	case termui.KeyEnter:
		if m == nil || fv.sel == "" {
			a.say("select a server (←↑↓→) to set it ready, drain or maint", termui.ColorDefault)
			return true
		}
		n := m.g.byID[fv.sel]
		if n.kind != kindSrv {
			a.say("Enter sets a server's state: select one", termui.ColorDefault)
			return true
		}
		a.open(serverAction(*n.row), fv.n.s)
	case termui.KeyUp, termui.KeyDown, termui.KeyLeft, termui.KeyRight, termui.KeyHome, termui.KeyEnd, termui.KeyTab, termui.KeyBackTab:
		if m != nil {
			fv.move(m, k)
		}
	case "R", "B", "J", "C":
		return false // the node's actions
	}
	return true
}

func (fv *flowView) close() {
	fv.stop()
	fv.n.flow = nil
}

// move walks the cursor: along the flow (down, or right) to where the
// node sends, back to what sends to it, across to the next node of its
// level.
func (fv *flowView) move(m *flowMap, k string) {
	vertical := m.vertical
	g := m.g
	if len(m.order) == 0 {
		return
	}
	cur := g.byID[fv.sel]
	if cur == nil {
		fv.sel = g.hub.id
		return
	}
	forward, back, next, prev := termui.KeyRight, termui.KeyLeft, termui.KeyDown, termui.KeyUp
	if vertical {
		forward, back, next, prev = termui.KeyDown, termui.KeyUp, termui.KeyRight, termui.KeyLeft
	}
	idx := 0
	for i, n := range m.order {
		if n == cur {
			idx = i
		}
	}
	switch k {
	case termui.KeyHome:
		fv.sel = m.order[0].id
	case termui.KeyEnd:
		fv.sel = m.order[len(m.order)-1].id
	case termui.KeyTab:
		fv.sel = m.order[(idx+1)%len(m.order)].id
	case termui.KeyBackTab:
		fv.sel = m.order[(idx-1+len(m.order))%len(m.order)].id
	case forward:
		for _, e := range g.edges {
			if e.from == cur.id {
				fv.sel = e.to
				return
			}
		}
		fv.sel = fv.nearest(m, cur, flowLevel[cur.kind]+1, vertical)
	case back:
		for _, e := range g.edges {
			if e.to == cur.id {
				fv.sel = e.from
				return
			}
		}
		fv.sel = fv.nearest(m, cur, flowLevel[cur.kind]-1, vertical)
	case next, prev:
		var same []*flowNode
		for _, n := range m.order {
			if flowLevel[n.kind] == flowLevel[cur.kind] {
				same = append(same, n)
			}
		}
		for i, n := range same {
			if n == cur {
				step := 1
				if k == prev {
					step = -1
				}
				if j := i + step; j >= 0 && j < len(same) {
					fv.sel = same[j].id
				}
				return
			}
		}
	}
}

// nearest is the node of a level closest to n across the flow, n's own
// id when the level has none.
func (fv *flowView) nearest(m *flowMap, n *flowNode, level int, vertical bool) string {
	best, dist := n.id, math.MaxInt
	for _, o := range m.order {
		if flowLevel[o.kind] != level {
			continue
		}
		d := o.y - n.y
		if vertical {
			d = o.x - n.x
		}
		if d < 0 {
			d = -d
		}
		if d < dist {
			best, dist = o.id, d
		}
	}
	return best
}

// follow scrolls the map so the node under the cursor shows whole.
func (fv *flowView) follow(m *flowMap, r termui.Rect) {
	if n := m.g.byID[fv.sel]; n != nil {
		if n.x < fv.ox {
			fv.ox = max(n.x-2, 0)
		}
		if right := n.x + n.width() + 1; right > fv.ox+r.W {
			fv.ox = right - r.W
		}
		if n.y < fv.oy {
			fv.oy = max(n.y-1, 0)
		}
		if bottom := n.y + n.height() + 1; bottom > fv.oy+r.H {
			fv.oy = bottom - r.H
		}
	}
	fv.ox = min(max(fv.ox, 0), max(m.w-r.W, 0))
	fv.oy = min(max(fv.oy, 0), max(m.h-r.H, 0))
}

// centre scrolls a map larger than its box to HAProxy: where it opens,
// and where it goes back to without a node under the cursor.
func (fv *flowView) centre(m *flowMap, r termui.Rect) {
	h := m.g.hub
	fv.ox = min(max(h.x+h.width()/2-r.W/2, 0), max(m.w-r.W, 0))
	fv.oy = min(max(h.y-r.H/2, 0), max(m.h-r.H, 0))
}

func (fv *flowView) render(a *app, f *termui.Frame, v *nodeView) {
	if v.last.hap.ok && fv.uptime > v.last.hap.uptime { // a reload: perhaps a new configuration
		select {
		case fv.kick <- struct{}{}:
		default:
		}
	}
	fv.uptime = v.last.hap.uptime
	// What the map can't say, above it.
	c := fv.config()
	var notes []struct {
		text string
		st   termui.Style
	}
	note := func(t string, st termui.Style) {
		notes = append(notes, struct {
			text string
			st   termui.Style
		}{t, st})
	}
	switch {
	case c.denied:
		note("Your role doesn't read HAProxy's configuration: no frontend is linked to its backends.", styleWarn)
	case c.err != "":
		note("The configuration couldn't be read: "+c.err, styleWarn)
	}
	for path, err := range c.mapErrs {
		note(fmt.Sprintf("The map %s couldn't be read (%s): the backends it routes to aren't linked.", path, err), styleWarn)
	}
	if v.last.stat == nil && v.reachable {
		note("Waiting for HAProxy's counters…", styleMuted)
	}

	m, mapR, side := fv.arrange(a, v, termui.Rect{X: 0, Y: 1, W: f.W, H: f.H - 2}, len(notes))
	if fv.sel != "" && m.g.byID[fv.sel] == nil {
		fv.sel = ""
	}
	fv.m = m
	if a.settings.FlowDirection == "auto" {
		fv.autoShown = &m.vertical
	}
	title := "Traffic flow  left to right"
	if m.vertical {
		title = "Traffic flow  top to bottom"
	}
	if a.settings.FlowDirection == "auto" {
		title += " (auto)"
	}
	in := f.Box(mapR, "", styleFocus, styleTitle)
	f.Text(mapR.X+2, mapR.Y, " "+termui.Truncate(title, mapR.W-6)+" ", styleTitle, mapR.W-4)
	for _, nt := range notes {
		if in.H <= 2 {
			break
		}
		f.Text(in.X+1, in.Y, termui.Truncate(nt.text, in.W-2), nt.st, in.W-2)
		in.Y++
		in.H--
	}
	inner := termui.Rect{X: in.X + 1, Y: in.Y, W: max(in.W-2, 0), H: in.H}
	if fv.sel == "" {
		fv.centre(m, inner)
	} else {
		fv.follow(m, inner)
	}
	near := map[string]bool(nil)
	if fv.sel != "" {
		near = m.g.related(fv.sel)
	}
	ox, oy := fv.ox, fv.oy
	if m.w < inner.W { // a map narrower than its box: in the middle
		ox = -(inner.W - m.w) / 2
	}
	if m.h < inner.H && !m.vertical { // left to right, lower than its box: level with its middle
		oy = -(inner.H - m.h) / 2
	}
	m.draw(f, inner, ox, oy, flowDraw{sel: fv.sel, near: near, phase: int(a.now().UnixMilli() / flowFrame.Milliseconds()), animate: a.settings.FlowAnimation, flash: fv.watchFlash(a, m, v)})
	// More of the map past an edge: an arrow on the border says so.
	if fv.ox > 0 {
		f.Set(mapR.X, mapR.Y+mapR.H/2, '◂', styleFocus)
	}
	if fv.ox+inner.W < m.w {
		f.Set(mapR.X+mapR.W-1, mapR.Y+mapR.H/2, '▸', styleFocus)
	}
	if fv.oy > 0 {
		f.Set(mapR.X+mapR.W/2, mapR.Y, '▴', styleFocus)
	}
	if fv.oy+inner.H < m.h {
		f.Set(mapR.X+mapR.W/2, mapR.Y+mapR.H-1, '▾', styleFocus)
	}
	fv.renderDetails(a, f, side, m, v)
}

// detail is one line of the details: a key and its value, or a line of
// its own when key is "".
type detail struct {
	key, value string
	st         termui.Style
}

func (fv *flowView) renderDetails(a *app, f *termui.Frame, r termui.Rect, m *flowMap, v *nodeView) {
	n := m.g.byID[fv.sel]
	title := "Details"
	var lines []detail
	if n == nil {
		lines = flowLegend(fv.nextDirection(a))
	} else {
		title = n.name + "  " + map[string]string{kindLog: "log target", kindIface: "interface", kindHub: "HAProxy", kindFE: "frontend", kindBE: "backend", kindSrv: "server", kindMore: "servers"}[n.kind]
		lines = flowDetails(m.g, n, v)
	}
	in := f.Box(r, termui.Truncate(title, max(r.W-6, 0)), termui.Style{FG: termui.ColorBoxProc}, styleTitle)
	if in.Empty() {
		return
	}
	kw := 0
	for _, l := range lines {
		kw = max(kw, len([]rune(l.key)))
	}
	kw = min(kw, 16)
	// Below the map, two columns when one is too short and they're wide
	// enough; a line too long goes on under itself.
	cols := 1
	if in.W >= 2*(kw+34) && len(lines) > in.H {
		cols = 2
	}
	colW := in.W / cols
	w := colW - 2
	type row struct {
		key, text string
		st        termui.Style
	}
	var rows []row
	for _, l := range lines {
		if l.key == "" {
			indent := len(l.value) - len(strings.TrimLeft(l.value, " "))
			for i, part := range wrap(strings.TrimLeft(l.value, " "), max(w-indent-2, 8)) {
				pad := indent
				if i > 0 && indent > 0 { // a rule's: under its own text
					pad += 2
				}
				rows = append(rows, row{text: strings.Repeat(" ", pad) + part, st: l.st})
			}
			continue
		}
		for i, part := range wrap(l.value, max(w-kw-1, 8)) {
			k := l.key
			if i > 0 {
				k = ""
			}
			rows = append(rows, row{key: termui.Pad(k, kw), text: part, st: l.st})
		}
	}
	for i, rw := range rows {
		col, y := i/in.H, in.Y+i%in.H
		if col >= cols {
			break
		}
		x := in.X + 1 + col*colW
		if rw.key == "" {
			f.Text(x, y, termui.Truncate(rw.text, w), rw.st, w)
			continue
		}
		f.Text(x, y, rw.key, styleMuted, w)
		f.Text(x+kw+1, y, termui.Truncate(rw.text, w-kw-1), rw.st, w-kw-1)
	}
}

// wrap cuts s into lines of at most w cells, between words when it can.
func wrap(s string, w int) []string {
	var out []string
	for len([]rune(s)) > w {
		rs := []rune(s)
		cut := w
		for i := w; i > w/2; i-- {
			if rs[i] == ' ' {
				cut = i
				break
			}
		}
		out = append(out, strings.TrimRight(string(rs[:cut]), " "))
		s = strings.TrimLeft(string(rs[cut:]), " ")
	}
	return append(out, s)
}

// flowLegend is the details without a node selected; next is where o
// goes.
func flowLegend(next string) []detail {
	return []detail{
		{value: "← ↑ ↓ → select a node: its paths come forward. Enter on a server: ready, drain, maint.", st: styleDefault},
		{value: "o " + next + " · s servers · f or Esc back", st: styleDefault},
		{value: ""},
		{value: "━ traffic  ─ none  ╌ server down, log target failing  ┄ off  ○ named by no rule", st: styleMuted},
		{value: "━ HTTP", st: termui.Style{FG: termui.ColorAccent}},
		{value: "━ TCP", st: termui.Style{FG: termui.ColorTCP}},
		{value: "━ logs, out of the node", st: termui.Style{FG: termui.ColorLog}},
		{value: "━ an interface's, all of it", st: styleDefault},
		{value: "A number that starts from zero lights up a moment.", st: styleMuted},
		{value: ""},
		{value: "Links come from the configuration: default_backend, use_backend, a map's values when use_backend reads one. " +
			"HAProxy doesn't count how a backend's traffic splits between the frontends that send to it.", st: styleMuted},
	}
}

// answers is what a frontend answers itself with, as its configuration
// says it.
func answers(as []string) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a
		if a == "return" || a == "redirect" {
			out[i] = "http-request " + a
		}
	}
	return out
}

// statRow is show stat's row of a proxy's own line or a server's.
func statRow(t *haproxystat.Table, px, sv string) []string {
	if t == nil {
		return nil
	}
	for _, r := range t.Rows {
		if t.Get(r, "pxname") == px && t.Get(r, "svname") == sv {
			return r
		}
	}
	return nil
}

func flowTraffic(in, out float64) string {
	if math.IsNaN(in) || math.IsNaN(out) {
		return "-"
	}
	return fmtRate(in) + " in · " + fmtRate(out) + " out"
}

// ruleText is a rule as the details say it: its line, or the map keys
// that pick the backend.
func ruleText(r haproxyflow.LinkRule) string {
	switch {
	case r.Map == "":
		s := r.Text
		if r.Via != "" {
			s += " (named in a set-var)"
		}
		return s
	case r.IsDefault:
		return "when no key of " + r.Map + " matches"
	case len(r.Keys) == 1:
		return "the key of " + r.Map + ": " + r.Keys[0]
	}
	return fmt.Sprintf("%d keys of %s: %s", len(r.Keys), r.Map, strings.Join(r.Keys, ", "))
}

func flowDetails(g *flowGraph, n *flowNode, v *nodeView) []detail {
	var out []detail
	add := func(k, val string, st termui.Style) {
		if val != "" {
			out = append(out, detail{k, val, st})
		}
	}
	t := v.last.stat
	col := func(row []string, c string) string {
		if row == nil {
			return ""
		}
		return t.Get(row, c)
	}
	// joined is the parts that have a value, the column's then its word.
	joined := func(row []string, parts ...string) string {
		var out []string
		for i := 0; i+1 < len(parts); i += 2 {
			if v := col(row, parts[i]); v != "" && (parts[i] != "slim" || v != "0") {
				out = append(out, strings.TrimSpace(strings.Replace(parts[i+1], "%", v, 1)))
			}
		}
		return strings.Join(out, " · ")
	}
	sessions := func(row []string) string { return joined(row, "scur", "% now", "smax", "% max", "slim", "limit %") }
	rates := func(row []string) string { return joined(row, "rate", "%", "rate_max", "max %") }
	responses := func(row []string) string {
		if col(row, "hrsp_2xx") == "" {
			return ""
		}
		// No-break spaces: a count and its class stay on one line.
		return fmt.Sprintf("%s\u00a02xx · %s\u00a03xx · %s\u00a04xx · %s\u00a05xx", col(row, "hrsp_2xx"), col(row, "hrsp_3xx"), col(row, "hrsp_4xx"), col(row, "hrsp_5xx"))
	}
	switch n.kind {
	case kindLog:
		r := n.log
		add("State", r.state, toneOf(map[string]string{"ok": "up", "failing": "down"}[r.state]))
		add("Sends", r.sends, styleDefault)
		add("To", r.to, styleDefault)
		switch {
		case r.via != "" && n.linked:
			add("Through", r.via, styleDefault)
		case r.via != "":
			add("Through", r.via+" (not on the map)", styleDefault)
		case r.remote != "":
			add("Through", "no route says", styleMuted)
		case r.sent > 0:
			add("Through", "not said - the node runs an older Janus", styleMuted)
		default:
			add("Through", "not connected yet", styleMuted)
		}
		if r.remote != "" {
			add("Connection", cmp.Or(r.local, "?")+" → "+r.remote, styleDefault)
		}
		add("Records/s", fmtFloat(r.msgRate, 1), styleDefault)
		if r.knowsBytes {
			add("Throughput", fmtRate(r.byteRate), styleDefault)
			add("Sent", fmt.Sprintf("%d records · %s", r.sent, humanBytes(r.bytes)), styleDefault)
		} else {
			add("Sent", fmt.Sprintf("%d records", r.sent), styleDefault)
		}
		add("Waiting", fmt.Sprint(r.queued), styleDefault)
		if r.dropped > 0 {
			add("Dropped", fmt.Sprint(r.dropped), styleWarn)
		}
		if r.errors > 0 {
			add("Failed sends", fmt.Sprint(r.errors), styleWarn)
		}
		if !r.lastSent.IsZero() {
			add("Last sent", humanDuration(max(time.Since(r.lastSent), 0).Round(time.Second))+" ago", styleDefault)
		}
		add("Sources", cmp.Or(strings.Join(r.sources, ", "), "all but the access logs"), styleDefault)
		if r.lastErr != "" {
			out = append(out, detail{}, detail{value: r.lastErr, st: styleDanger})
		}
	case kindIface:
		st := n.status
		switch {
		case st == nil:
		case !st.GetUp():
			add("State", "down", styleDanger)
		case !st.GetCarrier():
			add("State", "no carrier", styleWarn)
		default:
			add("State", "up", styleOK)
		}
		if st.GetVlanParent() != "" {
			add("Kind", fmt.Sprintf("VLAN %d on %s", st.GetVlanId(), st.GetVlanParent()), styleDefault)
		} else {
			add("Kind", st.GetKind(), styleDefault)
		}
		add("Addresses", strings.Join(st.GetAddresses(), ", "), styleDefault)
		if rt, ok := v.last.net[n.name]; ok {
			add("Receiving", fmtRate(rt.rx), styleDefault)
			add("Sending", fmtRate(rt.tx), styleDefault)
		}
		out = append(out, detail{}, detail{value: "All of the interface's traffic, not only HAProxy's.", st: styleMuted})
	case kindHub:
		h := v.last.hap
		if !h.ok {
			add("", firstOr(h.err, "HAProxy isn't answering on its stats socket."), styleDanger)
			break
		}
		add("Version", h.version, styleDefault)
		add("Connections", fmt.Sprintf("%d · max %s", h.conns, fmtCount(float64(h.maxConns))), styleDefault)
		add("Requests/s", fmtFloat(h.reqRate, 1), styleDefault)
		add("New conns/s", fmt.Sprint(h.connRate), styleDefault)
		add("Busy", fmt.Sprintf("%d%%", 100-h.idle), styleDefault)
		add("Uptime", humanDuration(time.Duration(h.uptime)*time.Second), styleDefault)
	case kindFE:
		row := statRow(t, n.name, haproxystat.Frontend)
		add("Status", n.fe.status, toneOf(n.fe.status))
		add("Mode", n.fe.mode, styleDefault)
		add("Binds", strings.Join(n.binds, " "), styleDefault)
		add("Sessions", sessions(row), styleDefault)
		add("Sessions/s", rates(row), styleDefault)
		if col(row, "req_rate") != "" {
			add("Requests/s", col(row, "req_rate"), styleDefault)
		}
		add("Traffic", flowTraffic(n.fe.in, n.fe.out), styleDefault)
		add("Responses", responses(row), styleDefault)
		if e := col(row, "ereq"); e != "" && e != "0" {
			add("Errors", e, styleWarn)
		}
		add("Answers itself", strings.Join(answers(n.answers), ", "), styleDefault)
		var sends []*flowEdge
		for _, e := range g.edges {
			if e.from == n.id && e.kind == kindFE {
				sends = append(sends, e)
			}
		}
		if len(sends) > 0 {
			out = append(out, detail{}, detail{value: "Sends to", st: styleHead})
			for _, e := range sends {
				out = append(out, detail{value: "→ " + g.byID[e.to].name, st: termui.Style{FG: termui.ColorAccent}})
				for _, r := range e.rules {
					out = append(out, detail{value: "  " + ruleText(r), st: styleDefault})
				}
			}
		}
		for _, u := range g.unresolved {
			if u.Frontend != n.name {
				continue
			}
			why := "the backend is chosen per request"
			if len(u.Maps) > 0 {
				why = "reads " + strings.Join(u.Maps, ", ") + ", which couldn't be read"
			}
			out = append(out, detail{}, detail{value: "use_backend " + u.Format + ": " + why + " - not on the map.", st: styleWarn})
		}
	case kindBE:
		row := statRow(t, n.name, haproxystat.Backend)
		add("Status", n.row.status, toneOf(n.row.status))
		add("Mode", n.mode, styleDefault)
		add("Servers", fmt.Sprintf("%d of %d up", len(n.servers)-n.down, len(n.servers)), styleDefault)
		add("Sessions", sessions(row), styleDefault)
		if n.row.qcur > 0 {
			add("Queued", fmt.Sprint(n.row.qcur), styleWarn)
		}
		add("Sessions/s", rates(row), styleDefault)
		add("Traffic", flowTraffic(n.row.in, n.row.out), styleDefault)
		if rt := col(row, "rtime"); rt != "" {
			add("Response time", rt+" ms (last 1024)", styleDefault)
		}
		add("Responses", responses(row), styleDefault)
		out = append(out, detail{}, detail{value: "Reached from", st: styleHead})
		from := 0
		for _, e := range g.edges {
			if e.to != n.id || e.kind != kindFE {
				continue
			}
			from++
			out = append(out, detail{value: "← " + g.byID[e.from].name, st: termui.Style{FG: termui.ColorAccent}})
			for _, r := range e.rules {
				out = append(out, detail{value: "  " + ruleText(r), st: styleDefault})
			}
		}
		if from == 0 {
			why := "No frontend rule names it."
			switch {
			case !g.configKnown:
				why = "Unknown: the configuration isn't read."
			case len(g.unresolved) > 0:
				why = "No frontend rule names it - unless a use_backend chosen per request does."
			}
			out = append(out, detail{value: why, st: styleMuted})
		}
	case kindMore:
		for _, s := range n.hidden {
			out = append(out, detail{key: s.server, value: s.status + "  " + fmtRate(sumRate(s.in, s.out)) + " · " + fmt.Sprint(s.scur) + " sess", st: toneOf(s.status)})
		}
	case kindSrv:
		r := n.row
		row := statRow(t, r.backend, r.server)
		add("Backend", r.backend, styleDefault)
		add("Status", r.status, toneOf(r.status))
		check := strings.TrimPrefix(r.check, "* ")
		if check != "" {
			parts := []string{check}
			if d := col(row, "check_desc"); d != "" {
				parts = append(parts, d)
			}
			if d := col(row, "check_duration"); d != "" {
				parts = append(parts, d+" ms")
			}
			add("Check", strings.Join(parts, " · "), styleDefault)
		}
		add("Address", r.addr, styleDefault)
		add("Weight", fmt.Sprint(r.weight), styleDefault)
		add("Sessions", sessions(row), styleDefault)
		if r.qcur > 0 {
			add("Queued", fmt.Sprint(r.qcur), styleWarn)
		}
		add("Sessions/s", rates(row), styleDefault)
		add("Traffic", flowTraffic(r.in, r.out), styleDefault)
		if rt := col(row, "rtime"); rt != "" {
			add("Response time", rt+" ms (last 1024)", styleDefault)
		}
		add("Responses", responses(row), styleDefault)
		if lc := col(row, "lastchg"); lc != "" {
			if s, err := time.ParseDuration(lc + "s"); err == nil {
				add("State for", humanDuration(s), styleDefault)
			}
		}
	}
	return out
}
