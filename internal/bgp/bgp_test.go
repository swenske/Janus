package bgp

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/modcfg"
)

func fixture(t *testing.T, name string) []Line {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	lines, err := readReply(bufio.NewReader(strings.NewReader(string(data))))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return lines
}

// The fixtures are BIRD 2.19.2's own replies (the extension's build), two
// instances peering over a real network.
func TestParseEstablishedSession(t *testing.T) {
	got := ParseProtocols(fixture(t, "show-protocols-all-bgp.txt"))
	want := []Protocol{{
		Name: "peer_b", Proto: "BGP", Table: "---", State: "up", Since: "01:41:34.348", Info: "Established",
		BGPState: "Established", NeighborAddress: "172.17.0.3", NeighborAS: 65002, LocalAS: 65001,
		Channels: []Channel{{Name: "ipv4", State: "UP", Imported: 0, Exported: 1, Preferred: 0, Counted: true, Changes: []RouteChange{
			{"import", "updates", "received", 0}, {"import", "updates", "rejected", 0}, {"import", "updates", "filtered", 0}, {"import", "updates", "ignored", 0}, {"import", "updates", "accepted", 0},
			{"import", "withdraws", "received", 0}, {"import", "withdraws", "rejected", 0}, {"import", "withdraws", "ignored", 0}, {"import", "withdraws", "accepted", 0},
			{"export", "updates", "received", 3}, {"export", "updates", "rejected", 0}, {"export", "updates", "filtered", 0}, {"export", "updates", "accepted", 3},
			{"export", "withdraws", "received", 2}, {"export", "withdraws", "accepted", 2},
		}}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseStaticAndSummary(t *testing.T) {
	got := ParseProtocols(fixture(t, "show-protocols-all-static.txt"))
	if len(got) != 1 || got[0].Name != "haproxy_anycast" || got[0].Proto != "Static" || got[0].Table != "master4" ||
		len(got[0].Channels) != 1 || got[0].Channels[0].Imported != 1 || got[0].Channels[0].Preferred != 1 {
		t.Errorf("static: %+v", got)
	}
	// One line per protocol, the second and next continuing the first's code.
	var names []string
	for _, p := range ParseProtocols(fixture(t, "show-protocols.txt")) {
		names = append(names, p.Name+"/"+p.State+"/"+p.Info)
	}
	if want := []string{"device1/up/", "haproxy_anycast/up/", "peer_b/up/Established"}; !reflect.DeepEqual(names, want) {
		t.Errorf("summary: %v, want %v", names, want)
	}
}

// BIRD 2.20.0's own replies (the bird extension's static binary, the one
// nodes run): two instances peering over a veth pair with qemu-bgp-test's
// protocols - B imports with "import keep filtered on" and filters one
// route; then A's haproxy_anycast disabled (janusd's gate), then back.
// janusd's "timeformat protocol iso long". Captured on a host whose local
// time was CET.
func TestParseBIRD220(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	find := func(ps []Protocol, name string) Protocol {
		t.Helper()
		for _, p := range ps {
			if p.Name == name {
				return p
			}
		}
		t.Fatalf("no %s in %+v", name, ps)
		return Protocol{}
	}
	changes := func(c Channel) map[string]uint64 {
		m := map[string]uint64{}
		for _, rc := range c.Changes {
			m[rc.Direction+" "+rc.Kind+" "+rc.Outcome] = rc.Count
		}
		return m
	}

	b := find(ParseProtocols(fixture(t, "bird-2.20.0-b-established.txt")), "peer_a")
	if b.State != "up" || b.Since != "2026-10-28 23:53:40" || b.BGPState != "Established" || len(b.Channels) != 1 {
		t.Fatalf("peer_a: %+v", b)
	}
	if at, ok := b.SinceTime(time.Date(2026, 10, 28, 23, 0, 0, 0, cet)); !ok || !at.Equal(time.Date(2026, 10, 28, 23, 53, 40, 0, cet)) {
		t.Errorf("since: %v %v", at, ok)
	}
	c := b.Channels[0]
	if !c.Counted || c.Imported != 2 || c.Exported != 0 || c.Preferred != 2 || c.Filtered == nil || *c.Filtered != 1 {
		t.Errorf("routes: %+v", c)
	}
	got := changes(c)
	want := map[string]uint64{
		"import updates received": 3, "import updates rejected": 0, "import updates filtered": 1, "import updates ignored": 0, "import updates accepted": 2,
		"import withdraws received": 0, "import withdraws rejected": 0, "import withdraws ignored": 1, "import withdraws accepted": 0,
		"export updates received": 2, "export updates rejected": 2, "export updates filtered": 0, "export updates accepted": 0,
		"export withdraws received": 0, "export withdraws accepted": 0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("peer_a's changes:\n got  %v\n want %v", got, want)
	}

	// The anycast route withdrawn: B counts the withdraw, A its export.
	if got := changes(find(ParseProtocols(fixture(t, "bird-2.20.0-b-withdrawn.txt")), "peer_a").Channels[0]); got["import withdraws received"] != 1 || got["import withdraws accepted"] != 1 {
		t.Errorf("B after the withdraw: %v", got)
	}
	back := find(ParseProtocols(fixture(t, "bird-2.20.0-b-back.txt")), "peer_a").Channels[0]
	if got := changes(back); back.Imported != 2 || got["import updates accepted"] != 3 {
		t.Errorf("B with the route back: %+v %v", back, got)
	}
	a := ParseProtocols(fixture(t, "bird-2.20.0-a-withdrawn.txt"))
	if got := changes(find(a, "peer_b").Channels[0]); got["export withdraws accepted"] != 1 || find(a, "peer_b").Channels[0].Exported != 2 {
		t.Errorf("A after the withdraw: %v", got)
	}
	// A channel that's down: BIRD prints no counts - nothing counted,
	// no table, no filtered.
	held := find(a, "haproxy_anycast")
	if held.State != "down" || len(held.Channels) != 1 || held.Channels[0].State != "DOWN" || held.Channels[0].Counted ||
		held.Channels[0].Changes != nil || held.Channels[0].Filtered != nil {
		t.Errorf("held down: %+v", held)
	}
	// No "filtered" without "import keep filtered on"; protocols without
	// a channel have none.
	if p := find(a, "peer_b"); p.Channels[0].Filtered != nil {
		t.Errorf("peer_b: %+v", p.Channels[0])
	}
	if p := find(a, "device1"); len(p.Channels) != 0 || p.Since != "2026-10-28 23:53:36" {
		t.Errorf("device1: %+v", p)
	}
}

// BIRD's default time format: a time of day within 20 hours (today's,
// or yesterday's when later than now), a date alone after - no moment.
func TestSinceTime(t *testing.T) {
	now := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	for since, want := range map[string]time.Time{
		"00:41:34.348":        time.Date(2026, 10, 29, 0, 41, 34, 348e6, time.UTC),
		"23:10:00.000":        time.Date(2026, 10, 28, 23, 10, 0, 0, time.UTC),
		"2026-10-20 10:00:01": time.Date(2026, 10, 20, 10, 0, 1, 0, time.UTC),
		"2026-10-20":          {},
		"":                    {},
		"Established":         {},
	} {
		got, ok := Protocol{Since: since}.SinceTime(now)
		if ok != !want.IsZero() || !got.Equal(want) {
			t.Errorf("%q: %v %v, want %v", since, got, ok, want)
		}
	}
}

func TestWithTimeFormat(t *testing.T) {
	for in, added := range map[string]bool{
		"router id 192.0.2.1;\n":                                true,
		"timeformat protocol iso long;\nrouter id 192.0.2.1;\n": false,
		"  timeformat protocol \"%T\";\n":                       false,
		"# timeformat protocol iso short;\n":                    true,
		"timeformat route iso long;\nrouter id 192.0.2.1;\n":    true,
	} {
		out := WithTimeFormat(in)
		if got := strings.Contains(out, "timeformat protocol iso long; # added by janusd"); got != added || !strings.HasPrefix(out, in) {
			t.Errorf("%q: %q", in, out)
		}
	}
}

func TestParseOlderSinceAndError(t *testing.T) {
	lines := []Line{
		{1002, "upstream   BGP        ---        start  2026-09-30 22:01:02  Active        Socket: Connection refused"},
		{1006, "  BGP state:          Active"},
		{1006, "    Last error:       Socket: Connection refused"},
	}
	p := ParseProtocols(lines)[0]
	if p.Since != "2026-09-30 22:01:02" || p.Info != "Active Socket: Connection refused" || p.LastError != "Socket: Connection refused" {
		t.Errorf("%+v", p)
	}
}

func TestReadReplyErrors(t *testing.T) {
	lines, err := readReply(bufio.NewReader(strings.NewReader("9001 syntax error, unexpected CF_SYM_UNDEFINED\n")))
	if err != nil || len(lines) != 1 || lines[0].Code != 9001 {
		t.Fatalf("%v %v", lines, err)
	}
	if _, err := readReply(bufio.NewReader(strings.NewReader("1002-cut short\n"))); err == nil {
		t.Error("a reply without its last line read as complete")
	}
}

func TestWithLog(t *testing.T) {
	for _, c := range []struct {
		in    string
		added bool
	}{
		{"router id 192.0.2.1;\nprotocol device {}", true},
		{"log stderr all;\nrouter id 192.0.2.1;\n", false},
		{"router id 192.0.2.1;\n  log \"x\" { info };\n", false},
		{"# log stderr all;\nrouter id 192.0.2.1;\n", true},
		{"/* log stderr all;\n*/\nrouter id 192.0.2.1;\n", true},
		{"protocol bgp b { description \"blog stuff\"; }\n", true},
	} {
		out := WithLog(c.in)
		if added := strings.Contains(out, "added by janusd"); added != c.added {
			t.Errorf("%q: added=%v, want %v", c.in, added, c.added)
		}
		if !strings.HasPrefix(out, c.in) {
			t.Errorf("%q: the operator's text moved: %q", c.in, out)
		}
	}
}

// fakeBIRD answers on SocketPath like BIRD: a greeting, then one command.
type fakeBIRD struct {
	mu       sync.Mutex
	state    map[string]string // protocol -> "up", "down", "start"
	order    []string
	commands []string
	disabled map[string]bool       // what "configure" brings back: the config's own
	ups      map[string]int        // how many times each protocol came up
	read     func() (string, bool) // the copy of bird.conf BIRD reads, if any
}

func startFakeBIRD(t *testing.T, protocols ...string) *fakeBIRD {
	t.Helper()
	RunDir = t.TempDir()
	f := &fakeBIRD{state: map[string]string{}, disabled: map[string]bool{}, ups: map[string]int{}}
	f.read = func() (string, bool) {
		data, err := os.ReadFile(filepath.Join(RunDir, "bird.conf"))
		return string(data), err == nil
	}
	for _, p := range protocols {
		name, st, _ := strings.Cut(p, "=")
		f.order = append(f.order, name)
		f.state[name] = st
		f.disabled[name] = st == "down"
	}
	l, err := net.Listen("unix", SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeBIRD) serve(c net.Conn) {
	defer c.Close()
	fmt.Fprint(c, "0001 BIRD fake ready.\n")
	cmd, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return
	}
	cmd = strings.TrimSpace(cmd)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, cmd)
	verb, arg, _ := strings.Cut(cmd, " ")
	switch {
	case cmd == "show protocols":
		fmt.Fprint(c, "2002-Name       Proto      Table      State  Since         Info\n")
		for i, n := range f.order {
			sep := " "
			if i == 0 {
				sep = "1002-"
			}
			fmt.Fprintf(c, "%s%-10s Static     master4    %-6s 01:00:00.000  \n", sep, n, f.state[n])
		}
		fmt.Fprint(c, "0000 \n")
	case verb == "disable" || verb == "enable":
		if _, ok := f.state[arg]; !ok {
			fmt.Fprintf(c, "8003 %s: no such protocol\n", arg)
			return
		}
		f.set(arg, map[string]string{"disable": "down", "enable": "up"}[verb])
		fmt.Fprintf(c, "0009-%s: %sd\n0000 \n", arg, verb)
	case cmd == "configure":
		f.configure()
		fmt.Fprint(c, "0002-Reading configuration\n0003 Reconfigured\n")
	default:
		fmt.Fprint(c, "9001 syntax error\n")
	}
}

// configure: each protocol as the copy of bird.conf BIRD reads says -
// disabled when bird.conf disables it, or janusd's gate (BIRD 2 restarts
// a protocol whose "disabled" changed, else reconfigures it in place).
// Without a copy, as bird.conf itself says. Called with mu held.
func (f *fakeBIRD) configure() {
	run, ok := f.read()
	for n := range f.state {
		down := f.disabled[n]
		if ok && regexp.MustCompile(`protocol \w+ `+n+` \{[^}]*held down by janusd`).MatchString(run) {
			down = true
		}
		f.set(n, map[bool]string{true: "down", false: "up"}[down])
	}
}

// start is BIRD starting: every protocol as its copy of bird.conf says.
func (f *fakeBIRD) start() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for n := range f.state {
		f.state[n] = "down"
	}
	f.configure()
}

// set changes a protocol's state, counting it coming up. Called with mu
// held.
func (f *fakeBIRD) set(name, state string) {
	if state == "up" && f.state[name] != "up" {
		f.ups[name]++
	}
	f.state[name] = state
}

func (f *fakeBIRD) upCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ups[name]
}

func (f *fakeBIRD) get(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[name]
}

func gateOnce(m *Manager, healthy bool) {
	stop := make(chan struct{})
	close(stop)
	m.KeepGate(func() bool { return healthy }, nil, 1, stop)
}

func TestGate(t *testing.T) {
	f := startFakeBIRD(t, "device1=up", "haproxy_anycast=up", "haproxy_off=down", "peer=up")
	m := New(nil)

	gateOnce(m, false)
	if f.get("haproxy_anycast") != "down" || f.get("device1") != "up" || f.get("peer") != "up" {
		t.Fatalf("HAProxy down: %v", f.state)
	}
	if got := m.Held(); !reflect.DeepEqual(got, []string{"haproxy_anycast"}) {
		t.Errorf("held %v - a protocol bird.conf itself disables isn't janusd's", got)
	}
	if m.HAProxyHealthy() {
		t.Error("the gate didn't record HAProxy's health")
	}
	gateOnce(m, false) // already down: nothing more sent
	if n := strings.Count(strings.Join(f.commands, "\n"), "disable"); n != 1 {
		t.Errorf("%d disable commands, want 1: %v", n, f.commands)
	}

	gateOnce(m, true)
	if f.get("haproxy_anycast") != "up" || f.get("haproxy_off") != "down" {
		t.Fatalf("HAProxy back: %v - only what janusd disabled comes back", f.state)
	}
	if len(m.Held()) != 0 {
		t.Errorf("still held: %v", m.Held())
	}
}

// A change signalled through changed is acted on at once, not at the next
// poll: a deliberate stop of HAProxy withdraws the anycast routes before
// its listeners close.
func TestGateWakesOnChange(t *testing.T) {
	f := startFakeBIRD(t, "haproxy_anycast=up")
	m := New(nil)
	var mu sync.Mutex
	healthy := true
	ch := make(chan struct{})
	changed := func() <-chan struct{} { mu.Lock(); defer mu.Unlock(); return ch }
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		m.KeepGate(func() bool { mu.Lock(); defer mu.Unlock(); return healthy }, changed, time.Hour, stop)
		close(done)
	}()
	mu.Lock()
	healthy = false
	close(ch)
	ch = make(chan struct{})
	mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for f.get("haproxy_anycast") != "down" {
		if time.Now().After(deadline) {
			t.Fatal("haproxy_anycast still up - polling is hourly here")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	<-done
}

type fakeServices struct{ state, started, stopped string }

func (s *fakeServices) StartService(id string) error { s.started = id; return nil }
func (s *fakeServices) StopService(id string) error  { s.stopped = id; return nil }
func (s *fakeServices) State(string) (extensions.ServiceState, error) {
	return extensions.ServiceState{State: s.state}, nil
}

func TestApply(t *testing.T) {
	f := startFakeBIRD(t, "haproxy_anycast=up")
	modcfg.Dir = t.TempDir()
	bin := filepath.Join(t.TempDir(), "bird")
	// bird -p -c FILE: refuses a file with "bogus" in it, like BIRD's parser.
	script := "#!/bin/sh\nif grep -q bogus \"$3\"; then echo \"$3:1:1 syntax error\"; exit 1; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	Binary = bin
	svc := &fakeServices{state: "running"}
	m := New(svc)

	errs, err := m.Apply("bogus;\n")
	if err == nil || len(errs) != 1 || !strings.Contains(errs[0], "bird.conf:1:1 syntax error") {
		t.Fatalf("a refused bird.conf: %v %v", errs, err)
	}
	if _, isDefault, _ := m.Saved(); !isDefault {
		t.Fatal("a refused bird.conf was saved")
	}

	// Applied while HAProxy doesn't answer: BIRD reconfigures from a copy
	// that disables haproxy_anycast - it never comes up, not even for
	// the moment before the gate's next look.
	conf := "router id 192.0.2.1;\nprotocol static haproxy_anycast { ipv4; }\n"
	gateOnce(m, false)
	ups := f.upCount("haproxy_anycast")
	if errs, err := m.Apply(conf); err != nil {
		t.Fatalf("Apply: %v %v", errs, err)
	}
	if !strings.Contains(strings.Join(f.commands, "\n"), "configure") || f.get("haproxy_anycast") != "down" || f.upCount("haproxy_anycast") != ups {
		t.Errorf("after Apply: %v %v, came up %d times", f.commands, f.state, f.upCount("haproxy_anycast")-ups)
	}
	if got := m.Held(); !reflect.DeepEqual(got, []string{"haproxy_anycast"}) {
		t.Errorf("held %v", got)
	}
	saved, _, _ := m.Saved()
	run, _ := os.ReadFile(filepath.Join(RunDir, "bird.conf"))
	if saved != conf || !strings.Contains(string(run), "haproxy_anycast { ipv4; disabled; /* held down by janusd */ }") ||
		!strings.Contains(string(run), "log stderr all;") || !strings.Contains(string(run), "timeformat protocol iso long;") {
		t.Errorf("saved %q, BIRD reads %q", saved, run)
	}
	// HAProxy answers: the copy no longer disables it, BIRD reconfigures
	// from it and the protocol comes up; applied again, it stays up.
	gateOnce(m, true)
	run, _ = os.ReadFile(filepath.Join(RunDir, "bird.conf"))
	if f.get("haproxy_anycast") != "up" || strings.Contains(string(run), "held down") || len(m.Held()) != 0 {
		t.Errorf("gate open: %v, held %v, BIRD reads %q", f.state, m.Held(), run)
	}
	ups = f.upCount("haproxy_anycast")
	if _, err := m.Apply(conf + "# again\n"); err != nil || f.get("haproxy_anycast") != "up" || f.upCount("haproxy_anycast") != ups {
		t.Errorf("applied with the gate open: %v %v", err, f.state)
	}

	svc.state = "stopped"
	if _, err := m.Apply("router id 192.0.2.2;\n"); err != nil || svc.started != ServiceID {
		t.Errorf("a stopped BIRD isn't started: %v %q", err, svc.started)
	}
	if _, err := m.Apply(""); err != nil || svc.stopped != ServiceID {
		t.Errorf("an empty bird.conf doesn't stop BIRD: %v %q", err, svc.stopped)
	}
	if _, isDefault, _ := m.Saved(); !isDefault {
		t.Error("an empty bird.conf isn't removed")
	}
}

// The gate closed when janusd starts (a drained cluster member, an
// HAProxy that doesn't answer): BIRD starts from a copy that disables the
// haproxy_* protocols - they never come up until the gate opens, when
// BIRD reconfigures from a copy that no longer does. One bird.conf
// itself disables stays down.
func TestGateAtBoot(t *testing.T) {
	f := startFakeBIRD(t, "device1=up", "haproxy_anycast=up", "haproxy_off=down", "peer=up")
	modcfg.Dir = t.TempDir()
	bin := filepath.Join(t.TempDir(), "bird")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	Binary = bin
	conf := "protocol device {}\nprotocol static haproxy_anycast { ipv4; }\nprotocol static haproxy_off { ipv4; disabled; }\nprotocol bgp peer { local as 1; }\n"
	if err := os.WriteFile(filepath.Join(modcfg.Dir, "bird.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(nil)
	if err := m.Boot(false); err != nil {
		t.Fatal(err)
	}
	run, _ := os.ReadFile(filepath.Join(RunDir, "bird.conf"))
	if !strings.Contains(string(run), "protocol static haproxy_anycast { ipv4; disabled; /* held down by janusd */ }") ||
		!strings.Contains(string(run), "protocol static haproxy_off { ipv4; disabled; }\n") {
		t.Fatalf("BIRD reads %q", run)
	}
	if got := m.Held(); !reflect.DeepEqual(got, []string{"haproxy_anycast"}) {
		t.Errorf("held %v", got)
	}
	f.start()
	gateOnce(m, false)
	if f.get("haproxy_anycast") != "down" || f.upCount("haproxy_anycast") != 0 || f.get("peer") != "up" {
		t.Fatalf("started with the gate closed: %v, haproxy_anycast came up %d times", f.state, f.upCount("haproxy_anycast"))
	}
	if strings.Contains(strings.Join(f.commands, "\n"), "disable") {
		t.Errorf("the gate had to disable something: %v", f.commands)
	}

	gateOnce(m, true)
	run, _ = os.ReadFile(filepath.Join(RunDir, "bird.conf"))
	if f.get("haproxy_anycast") != "up" || f.get("haproxy_off") != "down" || strings.Contains(string(run), "held down") || len(m.Held()) != 0 {
		t.Fatalf("gate open: %v, held %v, BIRD reads %q", f.state, m.Held(), run)
	}
	if strings.Contains(strings.Join(f.commands, "\n"), "enable") {
		t.Errorf("enabled from the CLI: %v - BIRD reconfigures from bird.conf, which knows what it disables", f.commands)
	}

	// Closed again: the CLI takes it down at once, and the copy says so -
	// a BIRD restarting now starts with it down.
	gateOnce(m, false)
	run, _ = os.ReadFile(filepath.Join(RunDir, "bird.conf"))
	if f.get("haproxy_anycast") != "down" || !strings.Contains(string(run), "held down") {
		t.Fatalf("gate closed: %v, BIRD reads %q", f.state, run)
	}
	ups := f.upCount("haproxy_anycast")
	f.start()
	gateOnce(m, false)
	if f.get("haproxy_anycast") != "down" || f.upCount("haproxy_anycast") != ups {
		t.Errorf("BIRD restarted with the gate closed: %v", f.state)
	}

	// Started with the gate open: nothing disabled in the copy.
	if err := m.Boot(true); err != nil {
		t.Fatal(err)
	}
	run, _ = os.ReadFile(filepath.Join(RunDir, "bird.conf"))
	if strings.Contains(string(run), "held down") {
		t.Errorf("gate open at boot, BIRD reads %q", run)
	}
}

// A copy BIRD would refuse is refused before anything is saved.
func TestCheckHeldCopy(t *testing.T) {
	RunDir = t.TempDir()
	modcfg.Dir = t.TempDir()
	bin := filepath.Join(t.TempDir(), "bird")
	script := "#!/bin/sh\nif grep -q 'held down' \"$3\"; then echo \"$3:2:40 syntax error\"; exit 1; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	Binary = bin
	m := New(nil)
	errs, err := m.Check("router id 192.0.2.1;\nprotocol static haproxy_anycast { ipv4; }\n")
	if err == nil || len(errs) != 2 || !strings.Contains(errs[0], "haproxy_* protocols disabled") || !strings.Contains(errs[1], "bird.conf:2:40") {
		t.Errorf("%v %v", errs, err)
	}
	if errs, err := m.Check("router id 192.0.2.1;\n"); err != nil {
		t.Errorf("no haproxy_* protocol: %v %v", errs, err)
	}
}

func TestHoldDown(t *testing.T) {
	const held = "disabled; /* held down by janusd */ "
	for _, c := range []struct {
		name, in, want string
		names          []string
	}{
		{"static", "protocol static haproxy_anycast { ipv4; route 192.0.2.100/32 blackhole; }\n",
			"protocol static haproxy_anycast { ipv4; route 192.0.2.100/32 blackhole; " + held + "}\n", []string{"haproxy_anycast"}},
		{"empty body, closing brace alone on its line", "protocol static haproxy_a {\n}\n", "protocol static haproxy_a {\n" + held + "}\n", []string{"haproxy_a"}},
		{"channel blocks, others untouched",
			"protocol device {}\nprotocol bgp haproxy_up {\n  local as 65001;\n  ipv4 { import none; export all; };\n}\nprotocol bgp upstream { ipv4 { export all; }; }\n",
			"protocol device {}\nprotocol bgp haproxy_up {\n  local as 65001;\n  ipv4 { import none; export all; };\n" + held + "}\nprotocol bgp upstream { ipv4 { export all; }; }\n",
			[]string{"haproxy_up"}},
		{"two of them", "protocol static haproxy_v4 { ipv4; }\nprotocol static haproxy_v6 { ipv6; }\n",
			"protocol static haproxy_v4 { ipv4; " + held + "}\nprotocol static haproxy_v6 { ipv6; " + held + "}\n", []string{"haproxy_v4", "haproxy_v6"}},
		{"comments and strings",
			"# protocol static haproxy_x { }\n/* protocol static haproxy_y { } */\nprotocol bgp haproxy_z { description \"a } b { c\"; # } \n ipv4; }\n",
			"# protocol static haproxy_x { }\n/* protocol static haproxy_y { } */\nprotocol bgp haproxy_z { description \"a } b { c\"; # } \n ipv4; " + held + "}\n",
			[]string{"haproxy_z"}},
		{"timeformat protocol, functions, a template",
			"timeformat protocol iso long;\nfunction f() { if net = 192.0.2.0/24 then { return true; } return false; }\ntemplate bgp haproxy_tpl { local as 1; }\nprotocol bgp haproxy_b from haproxy_tpl { neighbor 192.0.2.2 as 2; }\nprotocol bgp from haproxy_tpl { neighbor 192.0.2.3 as 2; }\n",
			"timeformat protocol iso long;\nfunction f() { if net = 192.0.2.0/24 then { return true; } return false; }\ntemplate bgp haproxy_tpl { local as 1; }\nprotocol bgp haproxy_b from haproxy_tpl { neighbor 192.0.2.2 as 2; " + held + "}\nprotocol bgp from haproxy_tpl { neighbor 192.0.2.3 as 2; }\n",
			[]string{"haproxy_b"}},
		{"two-word type, apostrophed name", "protocol ospf v3 haproxy_o { area 0 { interface \"eth0\"; }; }\nprotocol static 'haproxy_q' { ipv4; }\n",
			"protocol ospf v3 haproxy_o { area 0 { interface \"eth0\"; }; " + held + "}\nprotocol static 'haproxy_q' { ipv4; " + held + "}\n", []string{"haproxy_o", "haproxy_q"}},
		{"disabled by bird.conf", "protocol static haproxy_a { disabled; ipv4; }\nprotocol static haproxy_b { ipv4; disabled yes; }\nprotocol static haproxy_c { disabled 1; }\n",
			"protocol static haproxy_a { disabled; ipv4; }\nprotocol static haproxy_b { ipv4; disabled yes; }\nprotocol static haproxy_c { disabled 1; }\n", nil},
		{"enabled by bird.conf", "protocol static haproxy_a { disabled no; ipv4; }\nprotocol static haproxy_b { disabled; disabled off; }\n",
			"protocol static haproxy_a { disabled no; ipv4; " + held + "}\nprotocol static haproxy_b { disabled; disabled off; " + held + "}\n", []string{"haproxy_a", "haproxy_b"}},
		{"a channel's word isn't the protocol's", "protocol bgp haproxy_a { ipv4 { import filter { if disabled then reject; accept; }; }; }\n",
			"protocol bgp haproxy_a { ipv4 { import filter { if disabled then reject; accept; }; }; " + held + "}\n", []string{"haproxy_a"}},
		{"no haproxy_* protocol", "router id 192.0.2.1;\nprotocol static anycast { ipv4; }\n", "router id 192.0.2.1;\nprotocol static anycast { ipv4; }\n", nil},
		{"unterminated comment", "protocol static haproxy_a { ipv4; }\n/* no end\n", "protocol static haproxy_a { ipv4; }\n/* no end\n", nil},
		{"unterminated string", "protocol static haproxy_a { description \"x; }\n", "protocol static haproxy_a { description \"x; }\n", nil},
		{"unbalanced", "protocol static haproxy_a { ipv4;\n", "protocol static haproxy_a { ipv4;\n", nil},
		{"one brace too many", "protocol static haproxy_a { ipv4; }\n}\n", "protocol static haproxy_a { ipv4; }\n}\n", nil},
	} {
		got, names := HoldDown(c.in)
		if got != c.want || !reflect.DeepEqual(names, c.names) {
			t.Errorf("%s:\n got  %q %v\n want %q %v", c.name, got, names, c.want, c.names)
		}
		if strings.Count(got, "\n") != strings.Count(c.in, "\n") {
			t.Errorf("%s: the line numbers moved", c.name)
		}
	}
}
