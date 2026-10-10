package main

import (
	"crypto/x509"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/haproxystat"
	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/termui"
)

var tuiNow = time.Date(2026, 10, 8, 15, 4, 5, 0, time.UTC)

const (
	tuiStatPrev = `# pxname,svname,qcur,scur,slim,stot,bin,bout,ereq,status,weight,lastchg,rate,check_status,req_rate,hrsp_2xx,hrsp_5xx,
fe,FRONTEND,,3,2000,100,10000,50000,0,OPEN,,100,2,,4,90,1,
web,web1,0,2,,60,6000,30000,,UP,1,100,1,L4OK,,,,
web,web2,0,1,,40,4000,20000,,DOWN,1,50,1,L4CON,,,,
web,BACKEND,0,3,200,100,10000,50000,,UP,2,100,2,,,,,
empty,BACKEND,0,0,200,0,0,0,,DOWN,0,100,0,,,,,
`
	tuiStatCur = `# pxname,svname,qcur,scur,slim,stot,bin,bout,ereq,status,weight,lastchg,rate,check_status,req_rate,hrsp_2xx,hrsp_5xx,
fe,FRONTEND,,4,2000,108,14000,58000,0,OPEN,,102,3,,4,97,1,
web,web1,0,3,,65,8000,34000,,UP,1,102,2,L4OK,,,,
web,web2,0,1,,43,6000,24000,,DOWN,1,52,1,L4CON,,,,
web,BACKEND,0,4,200,108,14000,58000,,UP,2,102,3,,,,,
empty,BACKEND,0,0,200,0,0,0,,DOWN,0,102,0,,,,,
`
)

// testSamples are two full rounds two seconds apart, on a node that
// has every field.
func testSamples(t *testing.T) (*sample, *sample) {
	t.Helper()
	boot := uint64(tuiNow.Add(-(3*24*time.Hour + 4*time.Hour)).Unix())
	mk := func(at time.Time, total, idle uint64, rx, tx uint64, janusd, haproxy float64, reqs uint64, csv string) *sample {
		stat, err := haproxystat.Parse([]byte(csv))
		if err != nil {
			t.Fatal(err)
		}
		return &sample{
			at:   at,
			sys:  &janusv1alpha1.SystemStatResponse{BootTimeUnix: boot, CpuTotalTicks: total, CpuIdleTicks: idle, ContextSwitches: 1000, ProcessesCreated: 200},
			mem:  &janusv1alpha1.MemoryResponse{TotalBytes: 4 << 30, AvailableBytes: 2700 << 20, CachedBytes: 1 << 30},
			load: &janusv1alpha1.LoadAvgResponse{Load1: 0.42, Load5: 0.38, Load15: 0.35},
			net: &janusv1alpha1.NetworkDeviceStatsResponse{Devices: []*janusv1alpha1.NetworkDeviceStat{
				{Name: "lo", RxBytes: 100, TxBytes: 100}, {Name: "eth0", RxBytes: rx, TxBytes: tx, RxErrors: 0, TxErrors: 0}}},
			svc: &janusv1alpha1.StatsResponse{Processes: []*janusv1alpha1.ProcessStat{
				{Id: "janusd", CpuSeconds: janusd, MemoryBytes: 40 << 20}, {Id: "haproxy", CpuSeconds: haproxy, MemoryBytes: 60 << 20}}},
			info: &janusv1alpha1.ShowInfoResponse{Version: "3.2.9", UptimeSeconds: 273600, CurrentConnections: 12, MaxConnections: 262144, CumulativeConnections: 5000, CumulativeRequests: reqs, ConnectionRate: 4, SessionRate: 4, IdlePercent: 99},
			procs: &janusv1alpha1.ProcessesResponse{Processes: []*janusv1alpha1.ProcessInfo{
				{Pid: 1, Command: "/sbin/init", CpuPercent: 0.1, MemoryBytes: 8 << 20, CpuSeconds: 0.5},
				{Pid: 42, Command: "/usr/local/sbin/janusd -manage-host", CpuPercent: 1.2, MemoryBytes: 40 << 20, CpuSeconds: janusd},
				{Pid: 77, Command: "/usr/local/sbin/haproxy -f /etc/haproxy/haproxy.cfg", CpuPercent: 0.4, MemoryBytes: 60 << 20, CpuSeconds: haproxy}}},
			services: &janusv1alpha1.ServiceListResponse{Services: []*janusv1alpha1.ServiceInfo{
				{Id: "janusd", State: "running", Health: "healthy"}, {Id: "haproxy", State: "running", Health: "healthy"},
				{Id: "prometheus-node-exporter", State: "waiting", Health: "waiting", Extension: "prometheus-node-exporter"}}},
			stat: stat,
			errs: map[string]string{},
		}
	}
	prev := mk(tuiNow.Add(-2*time.Second), 1000, 800, 1_000_000, 500_000, 10.0, 3.0, 1000, tuiStatPrev)
	cur := mk(tuiNow, 1400, 1100, 1_024_576, 508_192, 10.4, 3.1, 1008, tuiStatCur)
	return prev, cur
}

func testSlow() slowInfo {
	return slowInfo{
		at: tuiNow,
		version: &janusv1alpha1.VersionResponse{Version: "v2026.10.08", ActiveSlot: "A", Arch: "amd64", SchematicId: "a055fbb6e1c2d3f4",
			Extensions: []*janusv1alpha1.ExtensionInfo{{Name: "prometheus-node-exporter", Version: "1.10.2"}},
			Haproxy:    &janusv1alpha1.ImageComponent{Variant: "3.2", Version: "3.2.9", ReleaseDefault: true}},
		hostname: "lgslbpub01",
		cpu:      &janusv1alpha1.CPUInfoResponse{Cores: 4, Sockets: 1, Cpus: []*janusv1alpha1.CPUInfo{{Processor: 0}, {Processor: 1}, {Processor: 2}, {Processor: 3}}},
		network: &janusv1alpha1.NetworkStatusResponse{Managed: true, Interfaces: []*janusv1alpha1.NetworkInterfaceStatus{
			{Name: "lo", Kind: "loopback", Up: true, Carrier: true}, {Name: "eth0", Kind: "physical", Up: true, Carrier: true, Addresses: []string{"172.16.1.151/24"}}},
			Time: &janusv1alpha1.TimeStatus{Synchronized: true}, TrialPending: true, TrialRevertAtUnix: tuiNow.Add(42 * time.Second).Unix()},
		vrrp: &janusv1alpha1.VRRPStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_RUNNING, Instances: []*janusv1alpha1.VRRPInstance{
			{Name: "VI_1", Role: "MASTER", Interface: "eth0", VirtualRouterId: 51, Priority: 150, EffectivePriority: 150, VirtualIps: []string{"172.16.1.150/24"}}}},
		bgp:      &janusv1alpha1.BGPStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED},
		consul:   &janusv1alpha1.ConsulStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED},
		firewall: &janusv1alpha1.FirewallListResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_RUNNING},
		errs:     map[string]string{},
	}
}

// testSampler is a sampler that already did two rounds, nothing dialled.
func testSampler(t *testing.T, a *app, name string) *sampler {
	t.Helper()
	prev, cur := testSamples(t)
	s := newSampler(tuiTarget{name: name, address: "172.16.1.151:9505", dial: func() (*grpc.ClientConn, error) { return nil, errors.New("not dialled in tests") }}, a.wake, &a.interval, &a.paused)
	s.prev, s.cur, s.slow, s.rounds = prev, cur, testSlow(), 2
	s.history.Append(derive(nil, prev))
	s.last = derive(prev, cur)
	s.history.Append(s.last)
	return s
}

func testApp(t *testing.T) *app {
	t.Helper()
	// Times are shown in the local zone: the frames are drawn in UTC.
	local := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = local })
	a := newApp(nil, tuiOptions{context: "lab", user: "sebastien", role: pki.RoleAdmin}, 2*time.Second)
	a.now = func() time.Time { return tuiNow }
	return a
}

func TestDerive(t *testing.T) {
	prev, cur := testSamples(t)
	first := derive(nil, prev)
	if !math.IsNaN(first.cpu) || !math.IsNaN(first.net["eth0"].rx) || !math.IsNaN(first.hap.reqRate) || !math.IsNaN(first.procs[1].cpu) {
		t.Errorf("a first sample knows no rate: %+v", first)
	}
	if first.memUsed != (4<<30)-(2700<<20) || first.load1 != 0.42 || !first.hap.ok || first.hap.conns != 12 || first.boot.IsZero() {
		t.Errorf("gauges come from the sample itself: %+v", first)
	}
	p := derive(prev, cur)
	if math.Abs(p.cpu-25) > 0.01 {
		t.Errorf("cpu = %v, want 25 (1 - 300/400)", p.cpu)
	}
	if e := p.net["eth0"]; math.Abs(e.rx-12288) > 0.01 || math.Abs(e.tx-4096) > 0.01 {
		t.Errorf("eth0 = %+v, want 12288 rx, 4096 tx bytes/s", e)
	}
	if math.Abs(p.hap.reqRate-4) > 0.01 || math.Abs(p.svcCPU["janusd"]-20) > 0.01 || math.Abs(p.svcCPU["haproxy"]-5) > 0.01 {
		t.Errorf("rates: req %v janusd %v haproxy %v", p.hap.reqRate, p.svcCPU["janusd"], p.svcCPU["haproxy"])
	}
	if len(p.procs) != 3 || math.Abs(p.procs[1].cpu-20) > 0.01 || p.procs[0].cpu != 0 {
		t.Errorf("processes = %+v", p.procs)
	}
	if len(p.fronts) != 1 || p.fronts[0].name != "fe" || p.fronts[0].scur != 4 || !p.fronts[0].hasSlim || p.fronts[0].reqs != 4 {
		t.Errorf("frontends = %+v", p.fronts)
	}
	// The backend's own row first, then its servers; rates against the previous table.
	if len(p.servers) != 4 || p.servers[0].backend != "web" || p.servers[0].server != "" || p.servers[1].server != "web1" || p.servers[3].backend != "empty" {
		t.Fatalf("servers = %+v", p.servers)
	}
	if w := p.servers[1]; math.Abs(w.in-1000) > 0.01 || math.Abs(w.out-2000) > 0.01 || w.status != "UP" || w.weight != 1 || w.check != "L4OK" {
		t.Errorf("web1 = %+v", w)
	}
	if len(p.services) != 3 || p.services[2].extension != "prometheus-node-exporter" {
		t.Errorf("services = %+v", p.services)
	}

	// A counter that went back (HAProxy reloaded): unknown, never negative.
	cur.info.CumulativeRequests = 3
	cur.stat, _ = haproxystat.Parse([]byte(strings.Replace(tuiStatCur, "web,web1,0,3,,65,8000,34000", "web,web1,0,3,,1,10,20", 1)))
	p = derive(prev, cur)
	if !math.IsNaN(p.hap.reqRate) || !math.IsNaN(p.servers[1].in) {
		t.Errorf("after a reload: req %v, web1 in %v - want NaN", p.hap.reqRate, p.servers[1].in)
	}
	// A node older than ProcessInfo.cpu_seconds: every process unknown, not idle.
	for _, pr := range prev.procs.Processes {
		pr.CpuSeconds = 0
	}
	for _, pr := range cur.procs.Processes {
		pr.CpuSeconds = 0
	}
	p = derive(prev, cur)
	for _, pr := range p.procs {
		if !math.IsNaN(pr.cpu) {
			t.Errorf("old node: pid %d cpu = %v, want NaN", pr.pid, pr.cpu)
		}
	}
	// A round without the system call: no cpu, but the rest.
	cur.sys = nil
	if p = derive(prev, cur); !math.IsNaN(p.cpu) || p.memTotal == 0 {
		t.Errorf("without SystemStat: %+v", p)
	}
}

func golden(t *testing.T, name string, f *termui.Frame) {
	t.Helper()
	got := strings.Join(f.Lines(), "\n") + "\n"
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test ./cmd/janusctl -run %s -update writes it)", err, t.Name())
	}
	if string(want) != got {
		t.Errorf("%s differs from the frame drawn (go test ./cmd/janusctl -run %s -update rewrites it):\n%s", path, t.Name(), got)
	}
}

func TestNodeScreenGolden(t *testing.T) {
	a := testApp(t)
	s := testSampler(t, a, "lgslbpub01")
	a.samplers = []*sampler{s}
	n := newNodeScreen(s, false)
	for i, e := range []struct{ typ, payload string }{
		{"janusd.started", `{"version":"v2026.10.08"}`}, {"haproxy.started", `{"pid":77}`}, {"haproxy.config.applied", `{"by":"sebastien","reload":true}`},
		{"network.trial.started", `{"reverts_in":"2m"}`},
	} {
		n.events.add(eventLine(&janusv1alpha1.Event{Id: uint64(i + 1), UnixTimeNs: tuiNow.Add(time.Duration(i-10) * time.Minute).UnixNano(), Type: e.typ, Payload: []byte(e.payload)}))
	}
	n.logs.add(tailLine{at: tuiNow.Add(-time.Minute), text: "api: listening on :9505", tone: logTone("api: listening on :9505")})
	n.logs.add(tailLine{at: tuiNow.Add(-30 * time.Second), text: "haproxy: reload: WARNING: config: 'option forwardfor' ignored", tone: logTone("WARNING")})
	a.screen = n
	golden(t, "tui-node-120x40.txt", a.render(120, 40))

	// Logs refused: the panel says so, the rest stands.
	n.logs.setErr("logs are for operators: PermissionDenied")
	lines := a.render(120, 40).Lines()
	if !strings.Contains(strings.Join(lines, "\n"), "logs are for operators") {
		t.Error("a refused log must be said in its panel")
	}
	// A small terminal: fewer panels, never a broken one.
	n.logs.setErr("")
	golden(t, "tui-node-80x24.txt", a.render(80, 24))
}

func TestFleetScreenGolden(t *testing.T) {
	a := testApp(t)
	trendFleet(t, a)
	ok := a.samplers[0]
	golden(t, "tui-fleet-120x40.txt", a.render(120, 40))
	// Enter opens the node under the cursor; Esc comes back.
	a.fleet.table.Cursor = 0
	a.key(termui.KeyEnter)
	n, isNode := a.screen.(*nodeScreen)
	if !isNode || n.s != ok || !n.fromFleet {
		t.Fatalf("Enter opened %T", a.screen)
	}
	n.cancel() // the follower started by open: tests don't dial
	a.key(termui.KeyEsc)
	if a.screen != a.fleet {
		t.Error("Esc doesn't come back to the fleet")
	}
}

func TestNodeScreenKeys(t *testing.T) {
	a := testApp(t)
	s := testSampler(t, a, "lgslbpub01")
	a.samplers = []*sampler{s}
	n := newNodeScreen(s, true)
	a.screen = n
	a.render(120, 40)
	if n.focus != panelHAProxy {
		t.Fatalf("focus starts on %v", n.focus)
	}
	want := []panel{panelProcesses, panelServices, panelNetwork, panelTail, panelHAProxy}
	for _, p := range want {
		a.key(termui.KeyTab)
		if n.focus != p {
			t.Errorf("Tab: focus %v, want %v", n.focus, p)
		}
	}
	a.key(termui.KeyBackTab)
	if n.focus != panelTail {
		t.Errorf("Shift-Tab: focus %v", n.focus)
	}
	// 5 hides the processes: HAProxy takes the whole right column.
	before := n.rects[panelHAProxy]
	a.key("5")
	a.render(120, 40)
	if _, shown := n.rects[panelProcesses]; shown || n.rects[panelHAProxy].H <= before.H {
		t.Errorf("after 5: processes shown %v, HAProxy %+v (was %+v)", shown, n.rects[panelHAProxy], before)
	}
	a.key("5")
	a.render(120, 40)
	if _, shown := n.rects[panelProcesses]; !shown {
		t.Error("5 again shows the processes")
	}
	// Sorting the processes.
	n.focus = panelProcesses
	a.key("s")
	if procSorts[n.procSort] != "memory" {
		t.Errorf("s: sort %q", procSorts[n.procSort])
	}
	a.render(120, 40)
	if n.procs.Rows[0][3] != "/usr/local/sbin/haproxy -f /etc/haproxy/haproxy.cfg" {
		t.Errorf("by memory: first row %v", n.procs.Rows[0])
	}
	a.key("r")
	a.render(120, 40)
	if n.procs.Rows[0][3] != "/sbin/init" {
		t.Errorf("reversed: first row %v", n.procs.Rows[0])
	}
	// Moving in the servers table, the selected server.
	n.focus = panelHAProxy
	a.render(120, 40)
	if n.selectedServer() != nil {
		t.Error("the cursor starts on the backend's row")
	}
	a.key(termui.KeyDown)
	if sv := n.selectedServer(); sv == nil || sv.server != "web1" {
		t.Errorf("down: selected %+v", sv)
	}
	// Pause, intervals, help, quit.
	a.key("p")
	if !a.paused.Load() || a.status.text != "paused - p resumes" {
		t.Errorf("p: paused %v, status %q", a.paused.Load(), a.status.text)
	}
	a.key("+")
	if time.Duration(a.interval.Load()) != 5*time.Second {
		t.Errorf("+: interval %v", time.Duration(a.interval.Load()))
	}
	a.key("-")
	a.key("-")
	a.key("-")
	if time.Duration(a.interval.Load()) != time.Second {
		t.Errorf("---: interval %v", time.Duration(a.interval.Load()))
	}
	a.key("?")
	if !a.help {
		t.Error("? opens the help")
	}
	if strings.Join(a.render(120, 40).Lines(), "\n") == "" || !strings.Contains(strings.Join(a.render(120, 40).Lines(), "\n"), "Shift-Tab") {
		t.Error("the help overlay lists the keys")
	}
	a.key("x")
	if a.help {
		t.Error("any key closes the help")
	}
	if !a.key("q") || !a.key(termui.KeyCtrlC) {
		t.Error("q and Ctrl-C leave")
	}
}

func TestLayoutFits(t *testing.T) {
	a := testApp(t)
	s := testSampler(t, a, "lgslbpub01")
	a.samplers = []*sampler{s}
	n := newNodeScreen(s, false)
	v := s.view()
	for _, size := range [][2]int{{80, 24}, {100, 30}, {120, 40}, {200, 60}, {80, 100}, {300, 24}} {
		w, h := size[0], size[1]
		rects := n.layout(w, h, &v)
		if len(rects) == 0 {
			t.Errorf("%dx%d: nothing laid out", w, h)
		}
		var all []termui.Rect
		for p, r := range rects {
			if r.X < 0 || r.Y < 1 || r.X+r.W > w || r.Y+r.H > h-1 || r.W < 2 || r.H < 2 {
				t.Errorf("%dx%d: %v at %+v is outside the body", w, h, panelNames[p], r)
			}
			all = append(all, r)
		}
		for i := range all {
			for j := i + 1; j < len(all); j++ {
				x, y := all[i], all[j]
				if x.X < y.X+y.W && y.X < x.X+x.W && x.Y < y.Y+y.H && y.Y < x.Y+x.H {
					t.Errorf("%dx%d: %+v and %+v overlap", w, h, x, y)
				}
			}
		}
		if _, ok := rects[panelHAProxy]; !ok {
			t.Errorf("%dx%d: HAProxy must always be there", w, h)
		}
		// Every frame draws without a panic, whatever the size.
		a.screen = n
		a.render(w, h)
	}
	a.render(79, 23) // too small: a message, no panic
}

func TestRoleFromCertificate(t *testing.T) {
	ca, err := pki.NewCA("test CA")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := ca.Issue(pki.IssueOptions{CommonName: "ops", Roles: []string{pki.RoleOperator}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "client.crt")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := roleFromCertFile(path); got != pki.RoleOperator {
		t.Errorf("role = %q", got)
	}
	if roleFromCertFile(filepath.Join(t.TempDir(), "missing")) != "" || roleFromTLS(nil) != "" {
		t.Error("no certificate: no role")
	}
	if roleFromAsRoles("os:reader,os:operator") != "os:reader" || roleFromAsRoles("") != "" {
		t.Error("roleFromAsRoles")
	}
	if !roleAllows("", rolesAdmins) || roleAllows(pki.RoleReader, rolesOperators) || !roleAllows(pki.RoleOperator, rolesOperators) || roleAllows(pki.RoleOperator, rolesAdmins) {
		t.Error("roleAllows")
	}
}

func TestSnapshotUnreachable(t *testing.T) {
	targets := []tuiTarget{{name: "gone", address: "10.0.0.9:9505", dial: func() (*grpc.ClientConn, error) { return nil, errors.New("dial tcp 10.0.0.9:9505: no route to host") }}}
	if _, err := snapshot(t.Context(), targets, tuiOptions{}, 120, 40); err == nil || !strings.Contains(err.Error(), "no route to host") {
		t.Errorf("snapshot of an unreachable node = %v", err)
	}
}

func TestTUIFormatting(t *testing.T) {
	cases := map[time.Duration]string{0: "0s", 45 * time.Second: "45s", 12*time.Minute + 3*time.Second: "12m 03s", 4*time.Hour + 12*time.Minute: "4h 12m", 76 * time.Hour: "3d 4h"}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
	if compactJSON([]byte(`{"b":1,"a":"x","c":true,"d":null,"e":[1,2]}`)) != "a=x b=1 c=true d=null e=[1,2]" {
		t.Errorf("compactJSON = %q", compactJSON([]byte(`{"b":1,"a":"x","c":true,"d":null,"e":[1,2]}`)))
	}
	if compactJSON([]byte(`{}`)) != "" || compactJSON([]byte(`not json`)) != "not json" {
		t.Error("compactJSON edge cases")
	}
	if fmtRate(math.NaN()) != "-" || fmtRate(12288) != "12.0KiB/s" || fmtPct(math.NaN()) != "-" || fmtPct(25.4) != "25%" {
		t.Error("fmtRate/fmtPct")
	}
	for state, want := range map[string]termui.Color{"UP": termui.ColorOK, "UP 1/3": termui.ColorOK, "DOWN": termui.ColorDanger, "MAINT": termui.ColorWarn, "DRAIN": termui.ColorWarn, "no check": termui.ColorMuted, "running": termui.ColorOK, "waiting": termui.ColorWarn, "MASTER": termui.ColorOK, "BACKUP": termui.ColorWarn, "FAULT": termui.ColorDanger, "OPEN": termui.ColorOK} {
		if got := toneOf(state).FG; got != want {
			t.Errorf("toneOf(%q) = %v, want %v", state, got, want)
		}
	}
	if eventTone("haproxy.exited") != termui.ColorDanger || eventTone("bootcommit.confirmed") != termui.ColorOK || eventTone("network.trial.started") != termui.ColorWarn {
		t.Error("eventTone")
	}
}

func TestActions(t *testing.T) {
	a := testApp(t) // an admin
	s := testSampler(t, a, "lgslbpub01")
	a.samplers = []*sampler{s}
	n := newNodeScreen(s, false)
	a.screen = n
	a.render(120, 40)
	// Enter on the backend's own row: a hint, no dialog.
	a.key(termui.KeyEnter)
	if a.modal != nil || !strings.Contains(a.status.text, "select a server") {
		t.Fatalf("Enter on a backend: modal %v, status %q", a.modal != nil, a.status.text)
	}
	a.key(termui.KeyDown) // web1
	a.key(termui.KeyEnter)
	if a.modal == nil || a.modal.act.title != "Server web/web1" || a.modal.cursor != 0 {
		t.Fatalf("Enter on web1: %+v", a.modal)
	}
	golden(t, "tui-node-modal-120x40.txt", a.render(120, 40))
	a.key(termui.KeyRight)
	if a.modal.cursor != 1 {
		t.Errorf("→: cursor %d", a.modal.cursor)
	}
	a.key(termui.KeyEsc)
	if a.modal != nil {
		t.Error("Esc closes the dialog")
	}
	// A dangerous action starts on Cancel: Enter there does nothing.
	a.key("B")
	if a.modal == nil || a.modal.cursor != 3 {
		t.Fatalf("B: %+v", a.modal)
	}
	a.key(termui.KeyEnter)
	if a.modal != nil {
		t.Error("Enter on Cancel closes the dialog")
	}
	// Running one: the test node isn't dialled, the footer says what the dial said.
	a.key("R")
	a.key(termui.KeyEnter)
	if a.modal == nil || !a.modal.busy {
		t.Fatal("Enter on Reload runs it")
	}
	select {
	case res := <-a.actions:
		a.finish(res)
	case <-time.After(5 * time.Second):
		t.Fatal("no result")
	}
	if a.modal != nil || !strings.Contains(a.status.text, "not dialled") || a.status.tone != termui.ColorDanger {
		t.Errorf("after the action: modal %v, status %q", a.modal != nil, a.status.text)
	}
	// A trial is pending in the test data: C offers it to an admin.
	a.key("C")
	if a.modal == nil || a.modal.act.title != "Confirm a trial" || strings.Join(a.modal.act.choices, ",") != "network,Cancel" {
		t.Fatalf("C: %+v", a.modal)
	}
	a.key("n")
	// A reader is offered nothing, and told.
	a.opts.role = pki.RoleReader
	a.key("R")
	if a.modal != nil || !strings.Contains(a.status.text, "can't") {
		t.Errorf("reader R: modal %v, status %q", a.modal != nil, a.status.text)
	}
	hints := strings.Join(n.hints(a), " ")
	if strings.Contains(hints, "reload") || strings.Contains(hints, "confirm") {
		t.Errorf("reader hints: %s", hints)
	}
	a.opts.role = pki.RoleOperator
	hints = strings.Join(n.hints(a), " ")
	if !strings.Contains(hints, "R reload") || strings.Contains(hints, "C confirm") {
		t.Errorf("operator hints: %s", hints)
	}
	a.key("C")
	if a.modal != nil || !strings.Contains(a.status.text, "os:admin") {
		t.Errorf("operator C: modal %v, status %q", a.modal != nil, a.status.text)
	}
	a.opts.role = ""
	a.key("C")
	if a.modal == nil {
		t.Error("an unknown role is offered everything")
	}
	a.key(termui.KeyEsc)
	// Without a trial, C says so.
	s.mu.Lock()
	s.slow.network.TrialPending = false
	s.mu.Unlock()
	a.key("C")
	if a.modal != nil || !strings.Contains(a.status.text, "no trial") {
		t.Errorf("C without a trial: modal %v, status %q", a.modal != nil, a.status.text)
	}
}

func TestTailAcceptsReplaysOnce(t *testing.T) {
	tl := newTail()
	e := func(id uint64, ns int64) *janusv1alpha1.Event {
		return &janusv1alpha1.Event{Id: id, UnixTimeNs: ns, Type: "x"}
	}
	if !tl.accept(e(1, 100)) || !tl.accept(e(2, 200)) {
		t.Fatal("new events are kept")
	}
	// The clock stepped back (NTP on a first boot): a later event with an
	// earlier time is still new.
	if !tl.accept(e(3, 50)) {
		t.Error("an event with an earlier time is new when its ID is")
	}
	// A reconnection replays the ring: the same events again.
	if tl.accept(e(1, 100)) || tl.accept(e(2, 200)) || tl.accept(e(3, 50)) {
		t.Error("a replay is skipped")
	}
	// janusd restarted: IDs start over with other times.
	if !tl.accept(e(1, 900)) || !tl.accept(e(2, 950)) {
		t.Error("a restart's events are new")
	}
	if !tl.accept(e(4, 1000)) {
		t.Error("a new ID is new")
	}
	for id := uint64(10); id < 3000; id++ { // the memory of IDs stays bounded
		tl.accept(e(id, int64(id)))
	}
	if len(tl.seen) > 2*tuiTailLines+1 {
		t.Errorf("seen holds %d IDs", len(tl.seen))
	}
}
