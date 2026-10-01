package bgp

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
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
		Channels: []Channel{{Name: "ipv4", State: "UP", Imported: 0, Exported: 1, Preferred: 0}},
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
	disabled map[string]bool // what "configure" brings back: the config's own
}

func startFakeBIRD(t *testing.T, protocols ...string) *fakeBIRD {
	t.Helper()
	RunDir = t.TempDir()
	f := &fakeBIRD{state: map[string]string{}, disabled: map[string]bool{}}
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
		f.state[arg] = map[string]string{"disable": "down", "enable": "up"}[verb]
		fmt.Fprintf(c, "0009-%s: %sd\n0000 \n", arg, verb)
	case cmd == "configure":
		for n := range f.state { // a reconfigured protocol comes back as configured
			f.state[n] = map[bool]string{true: "down", false: "up"}[f.disabled[n]]
		}
		fmt.Fprint(c, "0002-Reading configuration\n0003 Reconfigured\n")
	default:
		fmt.Fprint(c, "9001 syntax error\n")
	}
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

	// Applied while HAProxy doesn't answer: BIRD reconfigures (bringing
	// the protocol back) and the gate puts it down again at once.
	gateOnce(m, false)
	if errs, err := m.Apply("router id 192.0.2.1;\n"); err != nil {
		t.Fatalf("Apply: %v %v", errs, err)
	}
	if !strings.Contains(strings.Join(f.commands, "\n"), "configure") || f.get("haproxy_anycast") != "down" {
		t.Errorf("after Apply: %v %v", f.commands, f.state)
	}
	saved, _, _ := m.Saved()
	run, _ := os.ReadFile(filepath.Join(RunDir, "bird.conf"))
	if saved != "router id 192.0.2.1;\n" || !strings.HasPrefix(string(run), saved) || !strings.Contains(string(run), "log stderr all;") {
		t.Errorf("saved %q, BIRD reads %q", saved, run)
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
