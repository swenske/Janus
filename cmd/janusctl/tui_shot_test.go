package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/haproxyflow"
	"github.com/swenske/Janus/internal/haproxystat"
	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/schematic"
	"github.com/swenske/Janus/internal/termui"
)

// The janusctl tui screenshot of the docs and the landing page (make
// docs-screenshots): the node screen of a busy node, drawn by the
// dashboard's own code. Its samples are made up, the way the
// Controller's screenshots make up their charts' series
// (hack/browser/shots.mjs): eight minutes of a node serving a couple of
// thousand requests a second, in waves, with a burst. -shot DIR writes
// the frame as a 24-bit terminal gets it, Janus's theme on the
// terminal's own background; shots.mjs draws it in the browser.
var shotDir = flag.String("shot", "", "write the tui screenshot's frame into this directory (hack/docs-screenshots.sh)")

const (
	shotCols, shotRows = 140, 41
	shotSamples        = 240 // two seconds apart: more than the widest graph shows
	shotVersion        = "v2026.10.06"
)

// shotClock is shots.yaml's clock, the Controller's screenshots' moment.
var shotClock = time.Date(2026, 10, 6, 9, 41, 0, 0, time.UTC)

// shotHAProxy is the default HAProxy branch's version, as variants.mk
// and versions.mk pin it: what a node built today runs.
func shotHAProxy(t *testing.T) string {
	t.Helper()
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	m := regexp.MustCompile(`(?m)^HAPROXY_BRANCHES\s*:=\s*(\d+)\.(\d+)`).FindStringSubmatch(read("variants.mk"))
	if m == nil {
		t.Fatal("variants.mk: no HAPROXY_BRANCHES")
	}
	v := regexp.MustCompile(fmt.Sprintf(`(?m)^HAPROXY_%s_%s_VERSION\s*:=\s*(\S+)`, m[1], m[2])).FindStringSubmatch(read("versions.mk"))
	if v == nil {
		t.Fatalf("versions.mk: no HAProxy %s.%s version", m[1], m[2])
	}
	return v[1]
}

// shotTraffic is the requests per second at sample k: slow waves, a
// faster ripple, a burst towards the end, and the noise given.
func shotTraffic(k int, noise float64) float64 {
	w := func(a, period, phase float64) float64 { return a * math.Sin(2*math.Pi*float64(k)/period+phase) }
	r := 1750 + w(650, 120, 0.4) + w(220, 37, 1) + w(80, 9, 2) + noise
	if d := float64(k - 178); d > -20 && d < 40 {
		r += 1100 * math.Exp(-d*d/110)
	}
	return r
}

// shotStat is "show stat" for web.cfg (examples/haproxy/web.cfg) with
// these totals: the web frontend, the app backend's two servers, the
// prometheus frontend.
func shotStat(t *testing.T, conns, sessRate, reqRate, sessions, bin, bout uint64) *haproxystat.Table {
	t.Helper()
	half := func(v uint64, first bool) uint64 {
		if first {
			return v * 51 / 100
		}
		return v - v*51/100
	}
	var b strings.Builder
	b.WriteString("# pxname,svname,qcur,scur,slim,stot,bin,bout,ereq,status,weight,lastchg,rate,check_status,req_rate,hrsp_2xx,hrsp_3xx,hrsp_4xx,hrsp_5xx,\n")
	fmt.Fprintf(&b, "web,FRONTEND,,%d,32741,%d,%d,%d,3,OPEN,,,%d,,%d,%d,%d,%d,14,\n", conns, sessions, bin, bout, sessRate, reqRate, sessions*3*97/100, sessions*3*2/100, sessions*3/100)
	for i, name := range []string{"app1", "app2"} {
		first := i == 0
		fmt.Fprintf(&b, "app,%s,0,%d,,%d,%d,%d,,UP,1,1052220,%d,L7OK,,,,,,\n", name, half(conns*7/10, first), half(sessions, first), half(bin, first), half(bout, first), half(sessRate, first))
	}
	fmt.Fprintf(&b, "app,BACKEND,0,%d,3275,%d,%d,%d,,UP,2,1052220,%d,,,,,,,\n", conns*7/10, sessions, bin, bout, sessRate)
	b.WriteString("prometheus,FRONTEND,,1,32741,4183,312040,51207133,0,OPEN,,,0,,0,4183,0,0,0,\n")
	stat, err := haproxystat.Parse([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	return stat
}

// shotSamples are the rounds of a node that has been serving for a
// while: counters running sums of the made-up rates; the last two are
// full rounds (processes, services, the stats table).
func shotSamplesFor(t *testing.T, haproxy string) []*sample {
	t.Helper()
	rnd := rand.New(rand.NewPCG(2026, 1006)) //nolint:gosec // G404: a screenshot's noise
	const cores, dt = 4, 2.0
	boot := uint64(shotClock.Add(-(12*24*time.Hour + 4*time.Hour + 17*time.Minute)).Unix())
	// HAProxy was reloaded 39 minutes ago (the configuration applied in
	// the events): its counters start there.
	hapUp := uint64(39*60) - shotSamples*2
	ticks, idle := uint64(1052220*100*cores), uint64(1052220*100*cores*92/100)
	reqs, sessions := uint64(hapUp*2100), uint64(hapUp*780)
	rx, tx := uint64(41_337_000_000), uint64(512_901_000_000)
	bin, bout := reqs*760, reqs*9700
	janusd, haproxyCPU, keepalived, exporter := 1840.2, 361.7, 96.4, 512.9
	var out []*sample
	for k := range shotSamples {
		r := shotTraffic(k, rnd.NormFloat64()*70)
		cpu := 0.04 + r/7200 + rnd.NormFloat64()*0.01
		ticks += cores * 100 * dt
		idle += uint64(cores * 100 * dt * (1 - cpu))
		reqs += uint64(r * dt)
		sessRate := r / 2.7
		sessions += uint64(sessRate * dt)
		in, outB := r*dt*(740+rnd.Float64()*50), r*dt*(9400+rnd.Float64()*800)
		rx, tx = rx+uint64(in*1.03), tx+uint64(outB*1.02)
		bin, bout = bin+uint64(in), bout+uint64(outB)
		haproxyCPU += dt * cores * cpu * 0.84
		janusd += dt * (0.011 + rnd.Float64()*0.006)
		keepalived += dt * 0.0012
		exporter += dt * (0.002 + rnd.Float64()*0.003)
		conns := uint32(r*0.52 + rnd.NormFloat64()*12)
		hapUp += 2
		out = append(out, &sample{
			at:   shotClock.Add(time.Duration(k-shotSamples+1) * 2 * time.Second),
			sys:  &janusv1alpha1.SystemStatResponse{BootTimeUnix: boot, CpuTotalTicks: ticks, CpuIdleTicks: idle, ContextSwitches: 1000 * ticks, ProcessesCreated: 2817},
			mem:  &janusv1alpha1.MemoryResponse{TotalBytes: 4 << 30, AvailableBytes: (4 << 30) - (1180 << 20) - uint64(conns)*24<<10, CachedBytes: 912 << 20},
			load: &janusv1alpha1.LoadAvgResponse{Load1: math.Round(cores*cpu*90) / 100, Load5: 1.24, Load15: 1.17},
			net: &janusv1alpha1.NetworkDeviceStatsResponse{Devices: []*janusv1alpha1.NetworkDeviceStat{
				{Name: "lo", RxBytes: 88_120_331, TxBytes: 88_120_331}, {Name: "eth0", RxBytes: rx, TxBytes: tx}}},
			svc: &janusv1alpha1.StatsResponse{Processes: []*janusv1alpha1.ProcessStat{
				{Id: "janusd", CpuSeconds: janusd, MemoryBytes: 52 << 20}, {Id: "haproxy", CpuSeconds: haproxyCPU, MemoryBytes: 214 << 20},
				{Id: "keepalived", CpuSeconds: keepalived, MemoryBytes: 9 << 20}, {Id: "prometheus-node-exporter", CpuSeconds: exporter, MemoryBytes: 27 << 20}}},
			info: &janusv1alpha1.ShowInfoResponse{Version: haproxy, UptimeSeconds: hapUp, CurrentConnections: conns, MaxConnections: 32741,
				CumulativeConnections: sessions, CumulativeRequests: reqs, ConnectionRate: uint32(sessRate), SessionRate: uint32(sessRate),
				IdlePercent: uint32(math.Round(100 - cpu*100*0.84))},
			errs: map[string]string{},
		})
		if k >= shotSamples-2 {
			s := out[len(out)-1]
			s.procs = &janusv1alpha1.ProcessesResponse{Processes: []*janusv1alpha1.ProcessInfo{
				{Pid: 1, Command: "/sbin/init", MemoryBytes: 6 << 20, CpuSeconds: 4.1},
				{Pid: 214, Command: "/sbin/janusd -addr :9505 -manage-host", MemoryBytes: 52 << 20, CpuSeconds: janusd},
				{Pid: 297, Command: "/usr/local/sbin/keepalived --dont-fork --log-console --log-detail --no-syslog --use-file=/run/janus/keepalived/keepalived.conf", MemoryBytes: 5 << 20, CpuSeconds: keepalived / 3},
				{Pid: 301, Command: "/usr/local/sbin/keepalived --dont-fork --log-console --log-detail --no-syslog --use-file=/run/janus/keepalived/keepalived.conf", MemoryBytes: 4 << 20, CpuSeconds: keepalived * 2 / 3},
				{Pid: 305, Command: "/usr/local/sbin/node_exporter --web.listen-address=:9100 --collector.disable-defaults --collector.cpu", MemoryBytes: 27 << 20, CpuSeconds: exporter},
				{Pid: 2214, Command: "/usr/local/sbin/haproxy -f /etc/haproxy/haproxy.cfg -sf 1187", MemoryBytes: 214 << 20, CpuSeconds: haproxyCPU}}}
			s.services = &janusv1alpha1.ServiceListResponse{Services: []*janusv1alpha1.ServiceInfo{
				{Id: "janusd", State: "running", Health: "healthy"}, {Id: "haproxy", State: "running", Health: "healthy"},
				{Id: "keepalived", State: "running", Health: "healthy", Extension: "keepalived"},
				{Id: "prometheus-node-exporter", State: "running", Health: "healthy", Extension: "prometheus-node-exporter"}}}
			s.stat = shotStat(t, uint64(conns), uint64(sessRate), uint64(r), sessions, bin, bout)
		}
	}
	return out
}

// shotSampler is edge-par-1 of the Controller's screenshots after those
// rounds, with its slow round: version, interfaces, VRRP, firewall.
func shotSampler(t *testing.T, a *app) *sampler {
	t.Helper()
	sc := &schematic.Schematic{Customization: schematic.Customization{Extensions: []string{"keepalived", "nftables", "prometheus-node-exporter"}}}
	if err := sc.Normalize(); err != nil {
		t.Fatal(err)
	}
	samples := shotSamplesFor(t, shotHAProxy(t))
	s := newSampler(tuiTarget{name: "edge-par-1", address: "10.0.10.21:9505", dial: func() (*grpc.ClientConn, error) { return nil, errors.New("not dialled in tests") }}, a.wake, &a.interval, &a.paused)
	var prev *sample
	for _, cur := range samples {
		s.last = derive(prev, cur)
		s.history.Append(s.last)
		prev = cur
	}
	s.prev, s.cur, s.rounds = samples[len(samples)-2], samples[len(samples)-1], len(samples)
	s.slow = slowInfo{
		at:       shotClock,
		version:  &janusv1alpha1.VersionResponse{Version: shotVersion, ActiveSlot: "A", Arch: "amd64", SchematicId: sc.ID()},
		hostname: "edge-par-1",
		cpu:      &janusv1alpha1.CPUInfoResponse{Cores: 4, Sockets: 1},
		network: &janusv1alpha1.NetworkStatusResponse{Managed: true, Time: &janusv1alpha1.TimeStatus{Synchronized: true}, Interfaces: []*janusv1alpha1.NetworkInterfaceStatus{
			{Name: "lo", Kind: "loopback", Up: true, Carrier: true}, {Name: "eth0", Kind: "physical", Up: true, Carrier: true, Addresses: []string{"10.0.10.21/24", "10.0.10.20/24"}}}},
		vrrp: &janusv1alpha1.VRRPStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_RUNNING, Instances: []*janusv1alpha1.VRRPInstance{
			{Name: "VI_1", Role: "MASTER", Interface: "eth0", VirtualRouterId: 51, Priority: 150, EffectivePriority: 150, VirtualIps: []string{"10.0.10.20/24"}}}},
		bgp:      &janusv1alpha1.BGPStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED},
		consul:   &janusv1alpha1.ConsulStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED},
		firewall: &janusv1alpha1.FirewallListResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_RUNNING},
		errs:     map[string]string{},
	}
	return s
}

// TestScreenshotFrame draws the screenshot's frame - every run, so a
// change that breaks it fails here - and with -shot writes it.
func TestScreenshotFrame(t *testing.T) {
	local := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = local })
	a := newApp(nil, tuiOptions{context: "par", user: "alice", role: pki.RoleOperator}, 2*time.Second)
	a.now = func() time.Time { return shotClock }
	s := shotSampler(t, a)
	a.samplers = []*sampler{s}
	n := newNodeScreen(s, false)
	// alice applied web.cfg from the Controller, then drained app2 and
	// captured packets from janusctl: what the node logged and published.
	for _, e := range []struct {
		at, typ, json, log string
	}{
		{"09:00:31", "", "", "timesync: synchronized with 10.0.10.1 (offset 1.204ms)"},
		{"09:02:14", "haproxy.config.applied", `{"sha256":"5c1e0a9d7b3f2e6481c0d9a7f4b2e8c1d6a3f9e0b7c4d1a8e5f2b9c6d3a0e7f4"}`, "api: HAProxyService/ApplyConfig: alice (os:operator) via janus-controller"},
		{"09:14:52", "haproxy.server.state", `{"backend":"app","server":"app2","state":"drain"}`, "api: HAProxyService/ServerSetState: alice (os:operator)"},
		{"09:16:07", "haproxy.server.state", `{"backend":"app","server":"app2","state":"maint"}`, "api: HAProxyService/ServerSetState: alice (os:operator)"},
		{"09:21:33", "haproxy.server.state", `{"backend":"app","server":"app2","state":"ready"}`, "api: HAProxyService/ServerSetState: alice (os:operator)"},
		{"09:27:40", "pcap.started", `{"interface":"eth0","filter":"tcp port 80","duration_seconds":30,"promiscuous":false}`, "api: SystemService/PacketCapture: alice (os:operator)"},
		{"09:28:10", "pcap.finished", `{"interface":"eth0"}`, ""},
		{"09:38:21", "haproxy.server.state", `{"backend":"app","server":"app1","state":"ready"}`, "api: HAProxyService/ServerSetState: alice (os:operator)"},
	} {
		at, err := time.Parse("2006-01-02 15:04:05", "2026-10-06 "+e.at)
		if err != nil {
			t.Fatal(err)
		}
		if e.typ != "" {
			n.events.add(eventLine(&janusv1alpha1.Event{UnixTimeNs: at.UnixNano(), Type: e.typ, Payload: []byte(e.json)}))
		}
		if e.log != "" {
			n.logs.add(tailLine{at: at, text: e.log, tone: logTone(e.log)})
		}
	}
	a.screen = n
	f := a.render(shotCols, shotRows)
	text := strings.Join(f.Lines(), "\n")
	for _, want := range []string{"edge-par-1  " + shotVersion + " slot A", "VRRP VI_1  MASTER on eth0", "app1", "app2", "keepalived", "haproxy.server.state"} {
		if !strings.Contains(text, want) {
			t.Errorf("the screenshot's frame misses %q:\n%s", want, text)
		}
	}
	flow := shotFlow(t, a, n)
	if *shotDir == "" {
		return
	}
	p := termui.Palette{Depth: termui.DepthTrue, Theme: termui.ThemeJanus, Background: true}
	for name, fr := range map[string]*termui.Frame{"janusctl-tui.ansi": f, "janusctl-tui-flow.ansi": flow} {
		if err := os.WriteFile(filepath.Join(*shotDir, name), []byte(fr.Render(nil, p)), 0o644); err != nil { //nolint:gosec // G306: a screenshot's input
			t.Fatal(err)
		}
		t.Logf("wrote %s:\n%s", filepath.Join(*shotDir, name), strings.Join(fr.Lines(), "\n"))
	}
}

// shotFlow is the same node's traffic flow (f), examples/haproxy/web.cfg
// read as its configuration, the web frontend under the cursor.
func shotFlow(t *testing.T, a *app, n *nodeScreen) *termui.Frame {
	t.Helper()
	cfg, err := os.ReadFile(filepath.Join("..", "..", "examples", "haproxy", "web.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	top := haproxyflow.Parse(string(cfg))
	n.flow = newFlowView(n)
	n.flow.cfg = flowConfig{top: &top, gen: 1}
	n.flow.sel = "fe:web"
	f := a.render(shotCols, shotRows)
	text := strings.Join(f.Lines(), "\n")
	for _, want := range []string{"Traffic flow", "● HAProxy", "● web", "● prometheus", "● app", "● app1", "● app2", "→ app", "default_backend app"} {
		if !strings.Contains(text, want) {
			t.Errorf("the traffic flow's frame misses %q:\n%s", want, text)
		}
	}
	return f
}
