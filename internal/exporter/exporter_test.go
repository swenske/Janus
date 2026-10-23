package exporter

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWrite(t *testing.T) {
	var b strings.Builder
	err := Write(&b, []Family{
		{Name: "janus_build_info", Help: "Build.\nSecond line \\ here.", Type: Gauge, Samples: []Sample{
			{Labels: L("version", "v2026.10.01", "odd", "a\"b\\c\nd"), Value: 1},
		}},
		{Name: "janus_empty", Help: "Nothing.", Type: Gauge},
		{Name: "janus_things_total", Help: "Things.", Type: Counter, Samples: []Sample{
			{Value: 3}, {Labels: L("k", "v"), Value: 0.25}, {Value: math.Inf(1)}, {Value: 1e21},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `# HELP janus_build_info Build.\nSecond line \\ here.
# TYPE janus_build_info gauge
janus_build_info{version="v2026.10.01",odd="a\"b\\c\nd"} 1
# HELP janus_things_total Things.
# TYPE janus_things_total counter
janus_things_total 3
janus_things_total{k="v"} 0.25
janus_things_total +Inf
janus_things_total 1e+21
`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestLoadSave(t *testing.T) {
	Dir = t.TempDir()
	cfg, isDefault, err := Load()
	if err != nil || !isDefault || cfg != DefaultConfig() {
		t.Fatalf("Load with nothing saved = %+v %v %v", cfg, isDefault, err)
	}
	if err := Save(Config{Enabled: false, Port: 12345}); err != nil {
		t.Fatal(err)
	}
	cfg, isDefault, err = Load()
	if err != nil || isDefault || cfg != (Config{Enabled: false, Port: 12345}) {
		t.Fatalf("Load after Save = %+v %v %v", cfg, isDefault, err)
	}
	bad := Config{Enabled: true, Port: 70000}
	if err := bad.Validate(); err == nil {
		t.Fatal("port 70000 accepted")
	}
	zero := Config{Enabled: true}
	if err := zero.Validate(); err != nil || zero.Port != DefaultPort {
		t.Fatalf("port 0 = %+v %v", zero, err)
	}
}

func freePort(t *testing.T) uint32 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port)
}

func scrape(port uint32) (string, error) { return scrapeAt("127.0.0.1", port) }

func scrapeAt(host string, port uint32) (string, error) {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://%s/metrics", net.JoinHostPort(host, fmt.Sprint(port))))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return string(data), err
}

func TestServerApply(t *testing.T) {
	var calls atomic.Int32
	s := New(func() []Family {
		calls.Add(1)
		return []Family{{Name: "janus_up", Help: "Up.", Type: Gauge, Samples: []Sample{{Value: 1}}}}
	})
	defer s.Stop()

	a, b := freePort(t), freePort(t)
	if err := s.Apply(Config{Enabled: true, Port: a}); err != nil {
		t.Fatal(err)
	}
	if out, err := scrape(a); err != nil || !strings.Contains(out, "janus_up 1\n") {
		t.Fatalf("scrape on %d: %q %v", a, out, err)
	}

	// Moving to b closes a.
	if err := s.Apply(Config{Enabled: true, Port: b}); err != nil {
		t.Fatal(err)
	}
	if _, err := scrape(b); err != nil {
		t.Fatalf("scrape on the new port: %v", err)
	}
	if _, err := scrape(a); err == nil {
		t.Fatal("the old port still answers")
	}

	// A port that can't be bound: refused, and b keeps serving.
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if err := s.Apply(Config{Enabled: true, Port: uint32(busy.Addr().(*net.TCPAddr).Port)}); err == nil {
		t.Fatal("a busy port was accepted")
	}
	if _, err := scrape(b); err != nil {
		t.Fatalf("the previous listener stopped after a failed move: %v", err)
	}
	if cfg, listening, lastErr := s.Status(); cfg.Port != b || !listening || lastErr == "" {
		t.Fatalf("status after a failed move: %+v %v %q", cfg, listening, lastErr)
	}

	// Another address on the same port: the old listener held the port,
	// it gave way. An address that can't be bound: refused, and the
	// previous listener comes back.
	if err := s.Apply(Config{Enabled: true, Port: b, Address: "127.0.0.1"}); err != nil {
		t.Fatalf("127.0.0.1 on the same port: %v", err)
	}
	if _, err := scrape(b); err != nil {
		t.Fatalf("scrape on 127.0.0.1: %v", err)
	}
	if err := s.Apply(Config{Enabled: true, Port: b, Address: "192.0.2.1"}); err == nil {
		t.Fatal("an address this host doesn't have was accepted")
	}
	if _, err := scrape(b); err != nil {
		t.Fatalf("the previous listener didn't come back after a failed address change: %v", err)
	}
	if cfg, listening, _ := s.Status(); cfg.Address != "127.0.0.1" || !listening {
		t.Fatalf("status after a failed address change: %+v %v", cfg, listening)
	}
	if err := s.Apply(Config{Enabled: true, Port: b}); err != nil {
		t.Fatalf("back on every address: %v", err)
	}

	// Disabled: nothing listens; enabled again on the same port.
	if err := s.Apply(Config{Enabled: false, Port: b}); err != nil {
		t.Fatal(err)
	}
	if _, err := scrape(b); err == nil {
		t.Fatal("still answering once disabled")
	}
	if err := s.Apply(Config{Enabled: true, Port: b}); err != nil {
		t.Fatal(err)
	}
	if _, err := scrape(b); err != nil {
		t.Fatalf("re-enabled: %v", err)
	}
	if n := calls.Load(); n < 3 {
		t.Fatalf("collector called %d times", n)
	}
}

func BenchmarkWrite(b *testing.B) {
	samples := make([]Sample, 300)
	for i := range samples {
		samples[i] = Sample{Labels: L("proxy", fmt.Sprint("fe_", i), "server", "web01", "state"+fmt.Sprint(i%3), "UP"), Value: float64(i)}
	}
	families := []Family{
		{Name: "janus_haproxy_server_status", Help: "Server status, by proxy and server.", Type: Gauge, Samples: samples},
		{Name: "janus_build_info", Help: "Build.", Type: Gauge, Samples: samples[:1]},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := Write(io.Discard, families); err != nil {
			b.Fatal(err)
		}
	}
}

// TestAuto: the main interface's addresses, followed when they change -
// without dropping a connection when they don't; every address when
// there are none.
func TestAuto(t *testing.T) {
	var mu sync.Mutex
	main := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	setMain := func(a ...netip.Addr) { mu.Lock(); main = a; mu.Unlock() }
	s := New(func() []Family {
		return []Family{{Name: "janus_up", Help: "Up.", Type: Gauge, Samples: []Sample{{Value: 1}}}}
	})
	s.Main = func() []netip.Addr { mu.Lock(); defer mu.Unlock(); return slices.Clone(main) }
	defer s.Stop()

	p := freePort(t)
	if err := s.Apply(Config{Enabled: true, Port: p, Address: Auto}); err != nil {
		t.Fatal(err)
	}
	if _, err := scrapeAt("127.0.0.1", p); err != nil {
		t.Fatalf("scrape on the main address: %v", err)
	}
	if got := s.Addresses(); !slices.Equal(got, []string{"127.0.0.1"}) {
		t.Errorf("addresses = %v", got)
	}

	// Unchanged: a connection open across a Refresh still answers.
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(p)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := s.Refresh(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "GET /metrics HTTP/1.1\r\nHost: x\r\n\r\n")
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if line, err := bufio.NewReader(c).ReadString('\n'); err != nil || !strings.Contains(line, "200") {
		t.Fatalf("a connection across an unchanged Refresh: %q %v", line, err)
	}

	// The main address moved: the exporter with it.
	setMain(netip.MustParseAddr("127.0.0.2"))
	if err := s.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := scrapeAt("127.0.0.2", p); err != nil {
		t.Fatalf("scrape on the new main address: %v", err)
	}
	if _, err := scrapeAt("127.0.0.1", p); err == nil {
		t.Fatal("the old main address still answers")
	}

	// No main interface: every address.
	setMain()
	if err := s.Refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := scrapeAt("127.0.0.3", p); err != nil || s.Addresses() != nil {
		t.Fatalf("every address: %v %v", err, s.Addresses())
	}
	s.Main = nil
	if err := s.Apply(Config{Enabled: true, Port: p, Address: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(); err != nil || !slices.Equal(s.Addresses(), []string{"127.0.0.1"}) {
		t.Fatalf("a fixed address isn't Auto's to move: %v %v", err, s.Addresses())
	}

	for addr, ok := range map[string]bool{Auto: true, "": true, "192.0.2.1": true, "automatic": false} {
		c := Config{Enabled: true, Address: addr}
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("Validate(%q) = %v", addr, err)
		}
	}
}
