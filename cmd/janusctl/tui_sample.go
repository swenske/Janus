package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/haproxystat"
	"github.com/swenske/Janus/internal/ring"
	"github.com/swenske/Janus/internal/termui"
)

// The dashboard's data: what it asks each node, how often, and the
// rates it derives - the same way the Controller's node page does in
// the browser (counters polled, rates computed between two samples).

// tuiTarget is a node the dashboard watches and how to reach it. dial
// is lazy: a node that doesn't answer is a line saying so, not an
// error before the first frame.
type tuiTarget struct {
	name    string
	address string
	dial    func() (*grpc.ClientConn, error)
}

const (
	tuiFetchTimeout = 5 * time.Second
	tuiSlowEvery    = 5   // rounds between two slow rounds (version, modules...)
	tuiHistory      = 900 // points kept for the graphs
	tuiTailLines    = 500 // event and log lines kept
)

// sample is one round of the fast calls - every value the graphs and
// rates come from, with the error of each call that failed.
type sample struct {
	at       time.Time
	sys      *janusv1alpha1.SystemStatResponse
	mem      *janusv1alpha1.MemoryResponse
	load     *janusv1alpha1.LoadAvgResponse
	net      *janusv1alpha1.NetworkDeviceStatsResponse
	svc      *janusv1alpha1.StatsResponse
	info     *janusv1alpha1.ShowInfoResponse
	procs    *janusv1alpha1.ProcessesResponse   // full rounds only
	services *janusv1alpha1.ServiceListResponse // full rounds only
	stat     *haproxystat.Table                 // full rounds only
	errs     map[string]string
}

// slowInfo is what changes rarely: asked every tuiSlowEvery rounds.
type slowInfo struct {
	at       time.Time
	version  *janusv1alpha1.VersionResponse
	hostname string
	cpu      *janusv1alpha1.CPUInfoResponse
	network  *janusv1alpha1.NetworkStatusResponse
	vrrp     *janusv1alpha1.VRRPStatusResponse
	bgp      *janusv1alpha1.BGPStatusResponse
	consul   *janusv1alpha1.ConsulStatusResponse
	firewall *janusv1alpha1.FirewallListResponse
	sysctl   *janusv1alpha1.SysctlTrial
	errs     map[string]string
}

// point is what a sample means once compared with the previous one:
// gauges as they are, counters as rates. NaN is "unknown" (a first
// sample, a counter that went back - HAProxy's reset on reload).
type point struct {
	at       time.Time
	span     time.Duration // since the sample the rates are against; 0 without one
	boot     time.Time     // when the kernel started; zero unknown
	cpu      float64
	memUsed  uint64
	memTotal uint64
	memCache uint64
	load1    float64
	load5    float64
	load15   float64
	net      map[string]netRate
	svcCPU   map[string]float64
	procs    []procRow
	hap      hapPoint
	fronts   []frontendRow
	servers  []serverRow
	services []serviceRow // full rounds only
}

type serviceRow struct{ id, state, health, extension string }

type netRate struct {
	rx, tx float64
	rxErr  uint64
	txErr  uint64
}

type procRow struct {
	pid int32
	cmd string
	cpu float64 // NaN: the node doesn't say (older than ProcessInfo.cpu_seconds)
	rss uint64
}

type hapPoint struct {
	ok       bool
	version  string
	uptime   uint64
	conns    uint32
	maxConns uint32
	connRate uint32
	sessRate uint32
	idle     uint32
	reqRate  float64
	err      string
}

type frontendRow struct {
	name, status           string
	scur, slim, rate, reqs uint64
	hasSlim                bool
	h2xx, h3xx, h4xx, h5xx uint64
	ereq                   uint64
}

// serverRow is a backend's own row (server == "") or one of its servers.
type serverRow struct {
	backend, server, status string
	weight, scur, qcur      uint64
	rate                    uint64
	in, out                 float64 // bytes/s, NaN unknown
	check, lastchg          string
}

// fetchGroup runs the calls of a round in parallel and keeps each
// one's error by name.
type fetchGroup struct {
	wg   sync.WaitGroup
	mu   sync.Mutex
	errs map[string]string
}

func (g *fetchGroup) run(name string, fn func() error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := fn(); err != nil {
			g.mu.Lock()
			g.errs[name] = status.Convert(err).Message()
			g.mu.Unlock()
		}
	}()
}

// fetchSample is one fast round: the six calls the Controller's chart
// polls, plus processes, services and the stats table when full.
func fetchSample(ctx context.Context, conn *grpc.ClientConn, full bool) *sample {
	ctx, cancel := context.WithTimeout(ctx, tuiFetchTimeout)
	defer cancel()
	sys := janusv1alpha1.NewSystemServiceClient(conn)
	hap := janusv1alpha1.NewHAProxyServiceClient(conn)
	empty := &emptypb.Empty{}
	s := &sample{at: time.Now(), errs: map[string]string{}}
	g := &fetchGroup{errs: s.errs}
	g.run("system", func() (err error) { s.sys, err = sys.SystemStat(ctx, empty); return })
	g.run("memory", func() (err error) { s.mem, err = sys.Memory(ctx, empty); return })
	g.run("load", func() (err error) { s.load, err = sys.LoadAvg(ctx, empty); return })
	g.run("network", func() (err error) { s.net, err = sys.NetworkDeviceStats(ctx, empty); return })
	g.run("services", func() (err error) { s.svc, err = sys.Stats(ctx, empty); return })
	g.run("haproxy", func() (err error) { s.info, err = hap.ShowInfo(ctx, empty); return })
	if full {
		g.run("processes", func() (err error) { s.procs, err = sys.Processes(ctx, empty); return })
		g.run("service list", func() (err error) { s.services, err = sys.ServiceList(ctx, empty); return })
		g.run("stats", func() error {
			resp, err := hap.Stats(ctx, empty)
			if err != nil {
				return err
			}
			s.stat, err = haproxystat.Parse(resp.GetRawCsv())
			return err
		})
	}
	g.wg.Wait()
	return s
}

// fetchSlow asks what changes rarely. A module the image doesn't have
// answers MODULE_STATE_NOT_ENABLED (or FailedPrecondition): not an
// error.
func fetchSlow(ctx context.Context, conn *grpc.ClientConn) slowInfo {
	ctx, cancel := context.WithTimeout(ctx, tuiFetchTimeout)
	defer cancel()
	sys := janusv1alpha1.NewSystemServiceClient(conn)
	net := janusv1alpha1.NewNetworkServiceClient(conn)
	empty := &emptypb.Empty{}
	out := slowInfo{at: time.Now(), errs: map[string]string{}}
	g := &fetchGroup{errs: out.errs}
	module := func(err error) error {
		if status.Code(err) == codes.FailedPrecondition {
			return nil
		}
		return err
	}
	g.run("version", func() (err error) { out.version, err = sys.Version(ctx, empty); return })
	g.run("hostname", func() error {
		resp, err := sys.Hostname(ctx, empty)
		out.hostname = resp.GetHostname()
		return err
	})
	g.run("cpu", func() (err error) { out.cpu, err = sys.CPUInfo(ctx, empty); return })
	g.run("network status", func() (err error) { out.network, err = net.NetworkStatus(ctx, empty); return })
	g.run("vrrp", func() (err error) { out.vrrp, err = net.VRRPStatus(ctx, empty); return module(err) })
	g.run("bgp", func() (err error) { out.bgp, err = net.BGPStatus(ctx, empty); return module(err) })
	g.run("consul", func() (err error) { out.consul, err = net.ConsulStatus(ctx, empty); return module(err) })
	g.run("firewall", func() (err error) { out.firewall, err = net.FirewallList(ctx, empty); return module(err) })
	g.run("sysctl", func() error {
		resp, err := sys.SysctlList(ctx, empty)
		if err != nil {
			return module(err)
		}
		out.sysctl = resp.GetTrial()
		return nil
	})
	g.wg.Wait()
	return out
}

// rate is the change of a counter per second between two samples: NaN
// without a previous sample, or when the counter went back.
func rate(prev, cur uint64, dt float64) float64 {
	if dt <= 0 || cur < prev {
		return math.NaN()
	}
	return float64(cur-prev) / dt
}

// derive turns the current sample into a point, rates against prev.
func derive(prev, cur *sample) point {
	p := point{at: cur.at, cpu: math.NaN(), net: map[string]netRate{}, svcCPU: map[string]float64{}}
	p.hap.reqRate = math.NaN()
	var dt float64
	if prev != nil {
		p.span = cur.at.Sub(prev.at)
		dt = p.span.Seconds()
	}
	if cur.sys != nil && cur.sys.GetBootTimeUnix() > 0 {
		p.boot = time.Unix(int64(cur.sys.GetBootTimeUnix()), 0) //nolint:gosec // G115: a boot time fits
	}
	if cur.sys != nil && prev != nil && prev.sys != nil {
		total := float64(cur.sys.GetCpuTotalTicks()) - float64(prev.sys.GetCpuTotalTicks())
		idle := float64(cur.sys.GetCpuIdleTicks()) - float64(prev.sys.GetCpuIdleTicks())
		if total > 0 {
			p.cpu = min(max(1-idle/total, 0), 1) * 100
		}
	}
	if m := cur.mem; m != nil {
		p.memTotal, p.memCache = m.GetTotalBytes(), m.GetCachedBytes()
		if m.GetTotalBytes() > m.GetAvailableBytes() {
			p.memUsed = m.GetTotalBytes() - m.GetAvailableBytes()
		}
	}
	if l := cur.load; l != nil {
		p.load1, p.load5, p.load15 = l.GetLoad1(), l.GetLoad5(), l.GetLoad15()
	}
	if cur.net != nil {
		before := map[string]*janusv1alpha1.NetworkDeviceStat{}
		if prev != nil {
			for _, d := range prev.net.GetDevices() {
				before[d.GetName()] = d
			}
		}
		for _, d := range cur.net.GetDevices() {
			r := netRate{rx: math.NaN(), tx: math.NaN(), rxErr: d.GetRxErrors(), txErr: d.GetTxErrors()}
			if b, ok := before[d.GetName()]; ok {
				r.rx, r.tx = rate(b.GetRxBytes(), d.GetRxBytes(), dt), rate(b.GetTxBytes(), d.GetTxBytes(), dt)
			}
			p.net[d.GetName()] = r
		}
	}
	if cur.svc != nil {
		before := map[string]float64{}
		if prev != nil {
			for _, s := range prev.svc.GetProcesses() {
				before[s.GetId()] = s.GetCpuSeconds()
			}
		}
		for _, s := range cur.svc.GetProcesses() {
			v := math.NaN()
			if b, ok := before[s.GetId()]; ok && dt > 0 && s.GetCpuSeconds() >= b {
				v = (s.GetCpuSeconds() - b) / dt * 100
			}
			p.svcCPU[s.GetId()] = v
		}
	}
	if cur.procs != nil {
		p.procs = deriveProcs(prev, cur, dt)
	}
	for _, s := range cur.services.GetServices() {
		p.services = append(p.services, serviceRow{id: s.GetId(), state: s.GetState(), health: s.GetHealth(), extension: s.GetExtension()})
	}
	if cur.info != nil {
		i := cur.info
		p.hap = hapPoint{ok: true, version: i.GetVersion(), uptime: i.GetUptimeSeconds(), conns: i.GetCurrentConnections(), maxConns: i.GetMaxConnections(),
			connRate: i.GetConnectionRate(), sessRate: i.GetSessionRate(), idle: i.GetIdlePercent(), reqRate: math.NaN()}
		if prev != nil && prev.info != nil {
			p.hap.reqRate = rate(prev.info.GetCumulativeRequests(), i.GetCumulativeRequests(), dt)
		}
	} else if e, ok := cur.errs["haproxy"]; ok {
		p.hap.err = e
	}
	if cur.stat != nil {
		p.fronts, p.servers = deriveStat(prev, cur, dt)
	}
	return p
}

// deriveProcs is each process with its CPU over the interval. A node
// older than ProcessInfo.cpu_seconds sends 0 for every process: then
// nothing is known, rather than every process idle.
func deriveProcs(prev, cur *sample, dt float64) []procRow {
	known := false
	for _, pr := range cur.procs.GetProcesses() {
		if pr.GetCpuSeconds() > 0 {
			known = true
			break
		}
	}
	before := map[int32]float64{}
	if prev != nil && prev.procs != nil {
		for _, pr := range prev.procs.GetProcesses() {
			before[pr.GetPid()] = pr.GetCpuSeconds()
		}
	}
	out := make([]procRow, 0, len(cur.procs.GetProcesses()))
	for _, pr := range cur.procs.GetProcesses() {
		row := procRow{pid: pr.GetPid(), cmd: pr.GetCommand(), cpu: math.NaN(), rss: pr.GetMemoryBytes()}
		if b, ok := before[pr.GetPid()]; ok && known && dt > 0 && pr.GetCpuSeconds() >= b {
			row.cpu = (pr.GetCpuSeconds() - b) / dt * 100
		}
		out = append(out, row)
	}
	return out
}

// deriveStat reads the frontends and the backends with their servers
// out of "show stat", byte rates against the previous table.
func deriveStat(prev, cur *sample, dt float64) ([]frontendRow, []serverRow) {
	t := cur.stat
	type key struct{ px, sv string }
	before := map[key][]string{}
	var bt *haproxystat.Table
	if prev != nil && prev.stat != nil {
		bt = prev.stat
		for _, r := range bt.Rows {
			before[key{bt.Get(r, "pxname"), bt.Get(r, "svname")}] = r
		}
	}
	u := func(r []string, col string) uint64 { v, _ := t.Uint(r, col); return v }
	byteRate := func(r []string, col string) float64 {
		b, ok := before[key{t.Get(r, "pxname"), t.Get(r, "svname")}]
		if !ok {
			return math.NaN()
		}
		pv, pok := bt.Uint(b, col)
		cv, cok := t.Uint(r, col)
		if !pok || !cok {
			return math.NaN()
		}
		return rate(pv, cv, dt)
	}
	var fronts []frontendRow
	var servers []serverRow
	for _, r := range t.Rows {
		px, sv := t.Get(r, "pxname"), t.Get(r, "svname")
		switch sv {
		case "":
			continue
		case haproxystat.Frontend:
			f := frontendRow{name: px, status: t.Get(r, "status"), scur: u(r, "scur"), rate: u(r, "rate"), reqs: u(r, "req_rate"),
				h2xx: u(r, "hrsp_2xx"), h3xx: u(r, "hrsp_3xx"), h4xx: u(r, "hrsp_4xx"), h5xx: u(r, "hrsp_5xx"), ereq: u(r, "ereq")}
			f.slim, f.hasSlim = t.Uint(r, "slim")
			fronts = append(fronts, f)
		case haproxystat.Backend:
			servers = append(servers, serverRow{backend: px, status: t.Get(r, "status"), weight: u(r, "weight"), scur: u(r, "scur"), qcur: u(r, "qcur"),
				rate: u(r, "rate"), in: byteRate(r, "bin"), out: byteRate(r, "bout"), lastchg: t.Get(r, "lastchg")})
		default:
			servers = append(servers, serverRow{backend: px, server: sv, status: t.Get(r, "status"), weight: u(r, "weight"), scur: u(r, "scur"), qcur: u(r, "qcur"),
				rate: u(r, "rate"), in: byteRate(r, "bin"), out: byteRate(r, "bout"), check: t.Get(r, "check_status"), lastchg: t.Get(r, "lastchg")})
		}
	}
	// HAProxy lists a backend's servers before the backend's own row:
	// the dashboard shows the backend first, then its servers.
	var order []string
	groups := map[string][]serverRow{}
	for _, r := range servers {
		if _, seen := groups[r.backend]; !seen {
			order = append(order, r.backend)
		}
		if r.server == "" {
			groups[r.backend] = append([]serverRow{r}, groups[r.backend]...)
		} else {
			groups[r.backend] = append(groups[r.backend], r)
		}
	}
	servers = servers[:0]
	for _, b := range order {
		servers = append(servers, groups[b]...)
	}
	return fronts, servers
}

// nodeView is what a screen draws for one node: a copy taken under the
// sampler's lock.
type nodeView struct {
	name      string
	address   string
	sampled   bool   // at least one round answered or failed
	reachable bool   // the last round got an answer
	err       string // the last round's error when nothing answered, else the dial's
	last      point
	slow      slowInfo
	history   []point // oldest first
	rounds    int
}

// sampler polls one node in the background: a fast round every
// interval, a slow one every tuiSlowEvery rounds, and wakes the screen.
type sampler struct {
	target   tuiTarget
	wake     chan<- struct{}
	kick     chan struct{}
	full     atomic.Bool
	interval *atomic.Int64 // nanoseconds, shared with the app
	paused   *atomic.Bool

	mu      sync.Mutex
	conn    *grpc.ClientConn
	dialErr string
	prev    *sample
	cur     *sample
	slow    slowInfo
	last    point
	rounds  int
	history *ring.Ring[point]
}

func newSampler(t tuiTarget, wake chan<- struct{}, interval *atomic.Int64, paused *atomic.Bool) *sampler {
	return &sampler{target: t, wake: wake, kick: make(chan struct{}, 1), interval: interval, paused: paused, history: ring.New[point](tuiHistory)}
}

// connection dials once, lazily; a failed dial is tried again next round.
func (s *sampler) connection() (*grpc.ClientConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		return s.conn, nil
	}
	conn, err := s.target.dial()
	if err != nil {
		s.dialErr = err.Error()
		return nil, err
	}
	s.conn, s.dialErr = conn, ""
	return conn, nil
}

// run polls until ctx ends.
func (s *sampler) run(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.mu.Unlock()
	}()
	for {
		if !s.paused.Load() {
			s.round(ctx)
		}
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(time.Duration(s.interval.Load()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.kick:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Kick asks for a round now (after an action).
func (s *sampler) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// round is one fast round, and a slow one when due.
func (s *sampler) round(ctx context.Context) {
	defer s.wakeUp()
	conn, err := s.connection()
	if err != nil {
		s.mu.Lock()
		s.rounds++
		s.mu.Unlock()
		return
	}
	full := s.full.Load()
	s.mu.Lock()
	slowDue := s.rounds%tuiSlowEvery == 0 || s.slow.at.IsZero()
	s.mu.Unlock()
	var slow slowInfo
	var wg sync.WaitGroup
	if slowDue {
		wg.Add(1)
		go func() { defer wg.Done(); slow = fetchSlow(ctx, conn) }()
	}
	cur := fetchSample(ctx, conn, full)
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if slowDue && len(slow.errs) < 9 { // keep the old answers when nothing answered
		s.slow = slow
	} else if slowDue {
		s.slow.errs = slow.errs
	}
	s.rounds++
	if len(cur.errs) >= 6 && s.cur != nil && cur.sys == nil { // the node didn't answer: keep what's known, say so
		s.prev, s.cur = s.cur, cur
		return
	}
	s.prev, s.cur = s.cur, cur
	if s.prev != nil && s.prev.sys == nil { // rates against an empty round would lie
		s.prev = nil
	}
	s.last = derive(s.prev, cur)
	s.history.Append(s.last)
}

func (s *sampler) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// view is a copy for drawing.
func (s *sampler) view() nodeView {
	v := s.summary()
	v.history, _ = s.history.Last(tuiHistory)
	return v
}

// summary is view without the history - the fleet's row.
func (s *sampler) summary() nodeView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := nodeView{name: s.target.name, address: s.target.address, last: s.last, slow: s.slow, rounds: s.rounds, sampled: s.rounds > 0}
	switch {
	case s.dialErr != "":
		v.err = s.dialErr
	case s.cur == nil:
	case s.cur.sys != nil || len(s.cur.errs) < 6:
		v.reachable = true
		if e, ok := s.cur.errs["system"]; ok {
			v.err = e
		}
	default:
		v.err = s.cur.errs["system"]
		if v.err == "" {
			for _, e := range s.cur.errs {
				v.err = e
				break
			}
		}
	}
	return v
}

// trendPoint is one point of a trend: pick's value, measured over the
// span before at.
type trendPoint struct {
	at   time.Time
	span time.Duration
	v    float64
}

// trend is pick's value at each point after since, oldest first,
// read from the history in place.
func (s *sampler) trend(since time.Time, pick func(*point) float64) []trendPoint {
	var out []trendPoint
	s.history.Each(func(p *point) {
		if p.at.After(since) {
			out = append(out, trendPoint{at: p.at, span: p.span, v: pick(p)})
		}
	})
	return out
}

// tailLine is one line of the events or logs panel.
type tailLine struct {
	at   time.Time
	text string
	tone termui.Color
}

// tail is the lines a follower collected, newest last.
type tail struct {
	mu    sync.Mutex
	lines *ring.Ring[tailLine]
	err   string // why it stopped, shown in the panel
	// seen is each event received, by ID with its time: a replay after a
	// break brings the same events back (same ID, same time - skipped),
	// a janusd restart brings new ones under old IDs (kept). Time alone
	// can't tell: a first boot's clock steps when NTP answers.
	seen map[uint64]int64
}

func newTail() *tail { return &tail{lines: ring.New[tailLine](tuiTailLines), seen: map[uint64]int64{}} }

func (t *tail) add(l tailLine) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines.Append(l)
}

// accept reports whether e is new, and remembers it.
func (t *tail) accept(e *janusv1alpha1.Event) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if at, ok := t.seen[e.GetId()]; ok && at == e.GetUnixTimeNs() {
		return false
	}
	if len(t.seen) >= 2*tuiTailLines { // the ring holds tuiTailLines: forget the oldest IDs
		for id := range t.seen {
			if id+tuiTailLines < e.GetId() {
				delete(t.seen, id)
			}
		}
	}
	t.seen[e.GetId()] = e.GetUnixTimeNs()
	return true
}

func (t *tail) setErr(err string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.err = err
}

// snapshot is the last n lines and the follower's error.
func (t *tail) snapshot(n int) ([]tailLine, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines, _ := t.lines.Last(n)
	return lines, t.err
}

// eventLine is an event as one line: its type, then its payload's
// fields as k=v.
func eventLine(e *janusv1alpha1.Event) tailLine {
	l := tailLine{at: time.Unix(0, e.GetUnixTimeNs()), text: e.GetType(), tone: eventTone(e.GetType())}
	if fields := compactJSON(e.GetPayload()); fields != "" {
		l.text += " " + fields
	}
	return l
}

// compactJSON is a JSON object as "k=v k=v", keys sorted; anything else
// as it is.
func compactJSON(b []byte) string {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 || string(b) == "{}" || string(b) == "null" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return string(b)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		var s string
		switch x := v.(type) {
		case string:
			s = x
		case float64:
			s = fmt.Sprintf("%g", x)
		case bool:
			s = fmt.Sprint(x)
		case nil:
			s = "null"
		default:
			j, _ := json.Marshal(x)
			s = string(j)
		}
		parts = append(parts, k+"="+s)
	}
	return strings.Join(parts, " ")
}

func eventTone(typ string) termui.Color {
	switch {
	case strings.Contains(typ, "error"), strings.Contains(typ, "fail"), strings.Contains(typ, "denied"), strings.Contains(typ, "exited"),
		strings.Contains(typ, "revert"), strings.Contains(typ, "oom"), strings.Contains(typ, "unhealthy"), strings.Contains(typ, "crash"):
		return termui.ColorDanger
	case strings.Contains(typ, "trial"), strings.Contains(typ, "pending"), strings.Contains(typ, "warn"), strings.Contains(typ, "retry"):
		return termui.ColorWarn
	case strings.Contains(typ, "started"), strings.Contains(typ, "confirmed"), strings.Contains(typ, "applied"), strings.Contains(typ, "registered"),
		strings.Contains(typ, "healthy"), strings.Contains(typ, "reloaded"), strings.Contains(typ, "obtained"), strings.Contains(typ, "renewed"):
		return termui.ColorOK
	}
	return termui.ColorDefault
}

func logTone(line string) termui.Color {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "error"), strings.Contains(l, "alert"), strings.Contains(l, "emerg"), strings.Contains(l, "denied"), strings.Contains(l, "fatal"), strings.Contains(l, "failed"):
		return termui.ColorDanger
	case strings.Contains(l, "warn"), strings.Contains(l, "retry"):
		return termui.ColorWarn
	}
	return termui.ColorDefault
}

// followEvents streams SystemService.Events into t until ctx ends:
// from the start (the ring replays what it holds), again after a break
// - event IDs restart when janusd does, so a replay is told apart from
// a restart by ID and time together (tail.accept).
func followEvents(ctx context.Context, conn *grpc.ClientConn, t *tail) {
	sys := janusv1alpha1.NewSystemServiceClient(conn)
	for ctx.Err() == nil {
		stream, err := sys.Events(ctx, &janusv1alpha1.EventsRequest{})
		if err == nil {
			err = readEvents(stream, t)
		}
		if ctx.Err() != nil {
			return
		}
		if status.Code(err) == codes.PermissionDenied {
			t.setErr("events: " + status.Convert(err).Message())
			return
		}
		t.setErr("events: " + status.Convert(err).Message() + " - retrying")
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func readEvents(stream grpc.ServerStreamingClient[janusv1alpha1.Event], t *tail) error {
	for {
		e, err := stream.Recv()
		if err != nil {
			return err
		}
		if !t.accept(e) {
			continue
		}
		t.add(eventLine(e))
		t.setErr("")
	}
}

// followLogs streams a service's log into t. Logs are for operators:
// a refusal is shown and not retried.
func followLogs(ctx context.Context, conn *grpc.ClientConn, id string, t *tail) {
	sys := janusv1alpha1.NewSystemServiceClient(conn)
	for ctx.Err() == nil {
		stream, err := sys.Logs(ctx, &janusv1alpha1.LogsRequest{Id: id, Follow: true, TailLines: 200})
		if err == nil {
			err = readLogs(stream, t)
		}
		if ctx.Err() != nil {
			return
		}
		switch status.Code(err) {
		case codes.PermissionDenied:
			t.setErr("logs are for operators: " + status.Convert(err).Message())
			return
		case codes.NotFound, codes.InvalidArgument:
			t.setErr("logs: " + status.Convert(err).Message())
			return
		}
		t.setErr("logs: " + status.Convert(err).Message() + " - retrying")
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func readLogs(stream grpc.ServerStreamingClient[janusv1alpha1.Data], t *tail) error {
	for {
		d, err := stream.Recv()
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimRight(string(d.GetBytes()), "\n"), "\n") {
			if line == "" {
				continue
			}
			t.add(tailLine{at: time.Now(), text: line, tone: logTone(line)})
		}
		t.setErr("")
	}
}
