package bgp

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/modcfg"
)

// birdService runs a real BIRD the way the extension's manifest does,
// for Manager.
type birdService struct {
	bin     string
	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{}
	logPath string
}

func (s *birdService) StartService(string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		return nil
	}
	log, err := os.OpenFile(s.logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(s.bin, "-f", "-c", filepath.Join(RunDir, "bird.conf"), "-s", SocketPath(), "-P", filepath.Join(RunDir, "bird.pid"))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		return err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); log.Close(); close(done) }()
	s.cmd, s.done = cmd, done
	return waitFor(func() bool { _, err := Query("show status"); return err == nil })
}

func (s *birdService) StopService(string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		return nil
	}
	_ = s.cmd.Process.Kill()
	<-s.done
	s.cmd = nil
	return nil
}

func (s *birdService) State(string) (extensions.ServiceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil {
		return extensions.ServiceState{State: "stopped"}, nil
	}
	return extensions.ServiceState{State: "running"}, nil
}

func waitFor(ok func() bool) error {
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return nil
		}
	}
	return fmt.Errorf("not within 20s")
}

// queryAt is Query on another BIRD's control socket.
func queryAt(socket, cmd string) ([]Line, error) {
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	if _, err := readReply(r); err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return nil, err
	}
	return readReply(r)
}

// TestRealBIRD drives a real BIRD 2 - the bird extension's binary, given
// as JANUS_TEST_BIRD (skipped without it) - through Manager, peering over
// BGP with a second BIRD, the router, whose view of the anycast route is
// what counts: every update and withdraw it received. BIRD refuses
// loopback neighbours: run it in a network namespace of its own where
// 192.0.2.1 and 192.0.2.3 are the node's -
//
//	go test -c -o bgp.test ./internal/bgp
//	unshare -rn sh -c 'ip link set lo up && ip addr add 192.0.2.1/32 dev lo &&
//	  ip addr add 192.0.2.3/32 dev lo && JANUS_TEST_BIRD=.../usr/local/sbin/bird ./bgp.test -test.run TestRealBIRD -test.v'
//
// The gate closed when BIRD starts: the router never hears the route,
// not even for a moment; a bird.conf applied while it is closed doesn't
// announce it either; the gate open: announced once, the BGP session
// left as it was; applied while open: not withdrawn; closed: withdrawn;
// BIRD restarted while closed: still never announced. A haproxy_*
// protocol bird.conf disables stays down throughout.
//
// The BIRD 2 behaviour this rests on (nest/proto.c, proto_reconfigure):
// "configure" keeps a protocol as it runs only while its "disabled" is
// what the new configuration says, else restarts it so - up or down.
func TestRealBIRD(t *testing.T) {
	bin := os.Getenv("JANUS_TEST_BIRD")
	if bin == "" {
		t.Skip("JANUS_TEST_BIRD: a BIRD 2 binary (the bird extension's usr/local/sbin/bird)")
	}
	dir := t.TempDir()
	RunDir = filepath.Join(dir, "member")
	modcfg.Dir = filepath.Join(dir, "state")
	Binary = bin
	for _, d := range []string{RunDir, modcfg.Dir, filepath.Join(dir, "router")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	port := 20000 + os.Getpid()%20000
	routerConf := fmt.Sprintf(`router id 192.0.2.3;
log stderr all;
protocol device {}
protocol direct { ipv4; }
protocol bgp lb {
  local 192.0.2.3 port %d as 65000;
  neighbor 192.0.2.1 port %d as 65001;
  multihop;
  connect retry time 1;
  error wait time 1, 2;
  ipv4 { import all; export none; };
}
`, port+1, port)
	routerSock := filepath.Join(dir, "router", "bird.ctl")
	if err := os.WriteFile(filepath.Join(dir, "router", "bird.conf"), []byte(routerConf), 0o600); err != nil {
		t.Fatal(err)
	}
	routerLog, _ := os.Create(filepath.Join(dir, "router", "bird.log"))
	router := exec.Command(bin, "-f", "-c", filepath.Join(dir, "router", "bird.conf"), "-s", routerSock, "-P", filepath.Join(dir, "router", "bird.pid"))
	router.Stdout, router.Stderr = routerLog, routerLog
	if err := router.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Process.Kill(); _ = router.Wait() })

	memberConf := fmt.Sprintf(`router id 192.0.2.1;
protocol device {}
# The anycast address, while HAProxy answers.
protocol static haproxy_anycast {
    ipv4;
    route 192.0.2.100/32 blackhole;
}
protocol static haproxy_off { ipv4; route 192.0.2.101/32 blackhole; disabled; }
protocol bgp uplink {
  local 192.0.2.1 port %d as 65001;
  neighbor 192.0.2.3 port %d as 65000;
  multihop;
  connect delay time 1;
  ipv4 { import none; export where source = RTS_STATIC; };
}
`, port, port+1)
	if err := os.WriteFile(filepath.Join(modcfg.Dir, "bird.conf"), []byte(memberConf), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := &birdService{bin: bin, logPath: filepath.Join(dir, "member.log")}
	t.Cleanup(func() { _ = svc.StopService(ServiceID) })
	t.Cleanup(func() {
		if t.Failed() {
			for _, f := range []string{filepath.Join(dir, "member.log"), filepath.Join(dir, "router", "bird.log")} {
				data, _ := os.ReadFile(f)
				t.Logf("--- %s ---\n%s", f, data)
			}
		}
	})
	m := New(svc)

	// The router's session with the member, and its import counters.
	type view struct {
		state                          string
		since                          string
		imported, updates, withdrawals uint64
	}
	routerView := func() view {
		t.Helper()
		lines, err := queryAt(routerSock, "show protocols all lb")
		if err != nil {
			t.Fatalf("the router: %v", err)
		}
		ps := ParseProtocols(lines)
		if len(ps) != 1 {
			t.Fatalf("the router's protocols: %+v", ps)
		}
		v := view{state: ps[0].BGPState, since: ps[0].Since}
		for _, c := range ps[0].Channels {
			v.imported = uint64(c.Imported)
			for _, rc := range c.Changes {
				if rc.Direction == "import" && rc.Outcome == "received" {
					if rc.Kind == "updates" {
						v.updates = rc.Count
					} else {
						v.withdrawals = rc.Count
					}
				}
			}
		}
		return v
	}
	established := func() bool { return routerView().state == "Established" }
	member := func(name string) Protocol {
		t.Helper()
		st, err := m.Status()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range st.Protocols {
			if p.Name == name {
				return p
			}
		}
		t.Fatalf("no %s", name)
		return Protocol{}
	}
	// watch samples the router's view for d: it must never import the
	// anycast route nor count an update or a withdraw.
	quiet := func(what string, d time.Duration) {
		t.Helper()
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			if v := routerView(); v.imported != 0 || v.updates != 0 || v.withdrawals != 0 {
				t.Fatalf("%s: the router heard the anycast route: %+v", what, v)
			}
		}
	}

	// janusd starts with the gate closed (a drained member): BIRD from a
	// copy that disables haproxy_anycast.
	if err := m.Boot(false); err != nil {
		t.Fatal(err)
	}
	if err := svc.StartService(ServiceID); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(established); err != nil {
		t.Fatalf("the session never came up: %+v", routerView())
	}
	quiet("BIRD started with the gate closed", 3*time.Second)
	if p := member("haproxy_anycast"); p.State != "down" || !p.HeldDown {
		t.Fatalf("haproxy_anycast: %+v", p)
	}
	gateOnce(m, false)
	quiet("the gate's first look", time.Second)

	// bird.conf applied while the gate is closed: BIRD reconfigures,
	// haproxy_anycast stays down.
	if errs, err := m.Apply(memberConf + "# applied while drained\n"); err != nil {
		t.Fatalf("Apply: %v %v", errs, err)
	}
	quiet("applied with the gate closed", 3*time.Second)
	session := routerView().since

	// The gate opens: announced once; the session stays as it was.
	gateOnce(m, true)
	if err := waitFor(func() bool { return routerView().imported == 1 }); err != nil {
		t.Fatalf("the route never came: %+v", routerView())
	}
	if v := routerView(); v.updates != 1 || v.withdrawals != 0 || v.since != session || v.state != "Established" {
		t.Fatalf("the gate open: %+v (session since %s)", v, session)
	}
	if p := member("haproxy_off"); p.State != "down" {
		t.Fatalf("haproxy_off, which bird.conf disables, came up: %+v", p)
	}
	if len(m.Held()) != 0 {
		t.Errorf("held %v", m.Held())
	}

	// Applied while open: neither withdrawn nor announced again.
	if errs, err := m.Apply(memberConf + "# applied in service\n"); err != nil {
		t.Fatalf("Apply: %v %v", errs, err)
	}
	time.Sleep(2 * time.Second)
	if v := routerView(); v.imported != 1 || v.updates != 1 || v.withdrawals != 0 || v.since != session {
		t.Fatalf("applied with the gate open: %+v", v)
	}

	// Closed: withdrawn at once.
	gateOnce(m, false)
	if err := waitFor(func() bool { return routerView().imported == 0 }); err != nil {
		t.Fatalf("not withdrawn: %+v", routerView())
	}
	if v := routerView(); v.withdrawals != 1 {
		t.Fatalf("closed: %+v", v)
	}

	// BIRD restarts (a crash) while closed: a new session, the route
	// never heard on it.
	_ = svc.StopService(ServiceID)
	if err := waitFor(func() bool { return !established() }); err != nil {
		t.Fatal("the router's session outlived BIRD")
	}
	if err := svc.StartService(ServiceID); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(established); err != nil {
		t.Fatalf("the session never came back: %+v", routerView())
	}
	gateOnce(m, false)
	quiet("BIRD restarted with the gate closed", 3*time.Second)
	gateOnce(m, true)
	if err := waitFor(func() bool { return routerView().imported == 1 }); err != nil {
		t.Fatalf("the route never came back: %+v", routerView())
	}
	if p := member("haproxy_off"); p.State != "down" {
		t.Fatalf("haproxy_off came up: %+v", p)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "member.log"))
	if !strings.Contains(string(data), "Enabling protocol haproxy_anycast") {
		t.Errorf("BIRD's log doesn't say it enabled haproxy_anycast on reconfiguring:\n%s", data)
	}
}
