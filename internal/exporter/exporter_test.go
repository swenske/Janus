package exporter

import (
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
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

func scrape(port uint32) (string, error) {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
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
