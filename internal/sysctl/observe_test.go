package sysctl

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeProc is a copy of testdata/proc - real samples of a kernel's files
// - the test edits, with this process's own /proc/<pid> linked in.
func fakeProc(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	err := filepath.WalkDir("testdata/proc", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("testdata/proc", path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, rel), 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(os.Getpid())
	if err := os.Symlink("/proc/"+pid, filepath.Join(dir, pid)); err != nil {
		t.Fatal(err)
	}
	return dir
}

// setCounter rewrites one counter of a netstat or snmp file.
func setCounter(t *testing.T, path, prefix, name string, value int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for i := 0; i+1 < len(lines); i += 2 {
		names := strings.Fields(lines[i])
		if len(names) == 0 || names[0] != prefix+":" {
			continue
		}
		j := slices.Index(names, name)
		if j < 0 {
			t.Fatalf("%s: no %s %s", path, prefix, name)
		}
		values := strings.Fields(lines[i+1])
		values[j] = strconv.FormatInt(value, 10)
		lines[i+1] = strings.Join(values, " ")
		mustWrite(t, path, strings.Join(lines, "\n"))
		return
	}
	t.Fatalf("%s: no %s", path, prefix)
}

func TestReadCounters(t *testing.T) {
	o := &Observer{Proc: "testdata/proc"}
	c, err := o.readCounters()
	if err != nil {
		t.Fatal(err)
	}
	if c != (counters{activeOpens: 17996}) {
		t.Errorf("counters %+v", c)
	}
	cols, err := statColumns("testdata/proc/net/stat/nf_conntrack")
	if err != nil || cols["invalid"] != 0x8d {
		t.Errorf("conntrack statistics, summed over the CPUs: %v, %v", cols, err)
	}
	if mem, err := (ProcProbes{Proc: "testdata/proc"}).MemTotal(); err != nil || mem < 1<<30 {
		t.Errorf("memory %d, %v", mem, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// listenWith opens a listener on 127.0.0.1 with backlog.
func listenWith(t *testing.T, backlog int) netip.AddrPort {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, backlog); err != nil {
		t.Fatal(err)
	}
	sa, _ := unix.Getsockname(fd)
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(sa.(*unix.SockaddrInet4).Port))
}

// TestObserverHours: counters summed by hour, gauges maxed by hour,
// HAProxy's listeners, the window, what's kept across a restart, what's
// forgotten after Keep.
func TestObserverHours(t *testing.T) {
	fakeNode(t)
	proc := fakeProc(t)
	netstat, snmp := filepath.Join(proc, "net/netstat"), filepath.Join(proc, "net/snmp")
	now := time.Date(2026, 10, 7, 10, 0, 5, 0, time.UTC)
	o := &Observer{Proc: proc, Now: func() time.Time { return now }, Logf: t.Logf}

	// "HAProxy" is this test, and its listener.
	o.HAProxyPid = os.Getpid
	mustWrite(t, filepath.Join(proc, "sys/fs/file-nr"), "100\t0\t1000\n")
	addr := listenWith(t, 7)
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sample := func(at time.Duration, overflows, cookies, opens int64) {
		t.Helper()
		now = time.Date(2026, 10, 7, 10, 0, 5, 0, time.UTC).Add(at)
		setCounter(t, netstat, "TcpExt", "ListenOverflows", overflows)
		setCounter(t, netstat, "TcpExt", "SyncookiesSent", cookies)
		setCounter(t, snmp, "Tcp", "ActiveOpens", opens)
		o.Sample()
	}
	sample(0, 100, 0, 1000)               // the first sample: where counters start
	sample(15*time.Second, 105, 2, 3000)  // hour 10: 5 overflows, 2 cookies
	sample(30*time.Second, 105, 2, 6000)  // nothing new
	sample(45*time.Second, 105, 2, 9000)  // ...
	sample(60*time.Second, 105, 2, 19000) // a port sample: 18000 opens in 60 s
	sample(time.Hour, 108, 2, 19100)      // hour 11: 3
	sample(2*time.Hour, 108, 2, 19100)    // hour 12: none
	mustWrite(t, filepath.Join(proc, "sys/fs/file-nr"), "900\t0\t1000\n")
	sample(3*time.Hour, 110, 2, 19100) // hour 13: 2 - and 90% of the files
	mustWrite(t, filepath.Join(proc, "sys/fs/file-nr"), "500\t0\t1000\n")

	m := o.Metrics()
	if s := m.Signals["accept_overflows"]; s.Hours != 3 || s.Total != 10 || s.Peak != 5 {
		t.Errorf("accept_overflows %+v, want 3 hours, 10, 5 at the most", s)
	}
	if s := m.Signals["syn_cookies"]; s.Hours != 1 || s.Total != 2 {
		t.Errorf("syn_cookies %+v", s)
	}
	if s := m.Signals["outgoing_rate"]; s.Hours != 1 || s.Peak != 300 {
		t.Errorf("outgoing_rate %+v, want 18000 in 60 s", s)
	}
	if s := m.Signals["open_files"]; s.Hours != 1 || s.Peak != 0.9 || s.PeakAbs != 900 {
		t.Errorf("open_files %+v", s)
	}
	if s := m.Signals["source_ports"]; s.PeakAbs < 1 || s.Peak <= 0 {
		t.Errorf("source_ports %+v: this test's own connection, at least", s)
	}
	if !slices.Contains(m.Listeners, Listener{Addr: addr, Backlog: 7}) || !slices.Contains(m.ListeningPorts, int64(addr.Port())) {
		t.Errorf("listeners %v, ports %v: want %s", m.Listeners, m.ListeningPorts, addr)
	}

	// A restart: what's kept comes back, the hour in progress goes on.
	o.save()
	o2 := &Observer{Proc: proc, Now: func() time.Time { return now }, Logf: t.Logf, HAProxyPid: os.Getpid}
	o2.load()
	setCounter(t, netstat, "TcpExt", "ListenOverflows", 300)
	o2.Sample() // where counters start, again
	setCounter(t, netstat, "TcpExt", "ListenOverflows", 304)
	now = now.Add(time.Minute)
	o2.Sample()
	if s := o2.Metrics().Signals["accept_overflows"]; s.Hours != 3 || s.Total != 14 || s.Peak != 6 {
		t.Errorf("after a restart: %+v, want 3 hours, 14, 6 at the most", s)
	}
	if got := o2.Observation().Since; !got.Equal(time.Date(2026, 10, 7, 10, 0, 5, 0, time.UTC)) {
		t.Errorf("observing since %v", got)
	}

	// The window: 7 days later, hour 10 is out of it.
	now = time.Date(2026, 10, 14, 10, 30, 0, 0, time.UTC)
	if s := o2.Metrics().Signals["accept_overflows"]; s.Hours != 2 || s.Total != 9 {
		t.Errorf("a week later: %+v, want 2 hours, 9", s)
	}
	// Two weeks without a sample - a node off that long: the hours are
	// forgotten, the observation starts over.
	now = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	o2.Sample()
	o2.save()
	if s := o2.Metrics().Signals["accept_overflows"]; s.Hours != 0 {
		t.Errorf("two weeks later: %+v", s)
	}
	if got := o2.Observation().Since; !got.Equal(now) {
		t.Errorf("observing since %v, want %v", got, now)
	}
	if data := readFile(t, filepath.Join(Dir, observationsName)); strings.Contains(data, `"accept_overflows"`) {
		t.Errorf("forgotten hours still saved: %s", data)
	}

	// Closed - the machine going down: saved, then never written again.
	setCounter(t, netstat, "TcpExt", "ListenOverflows", 400)
	o2.Sample()
	o2.Close()
	saved := readFile(t, filepath.Join(Dir, observationsName))
	if !strings.Contains(saved, `"accept_overflows"`) {
		t.Errorf("the last hour isn't saved at Close: %s", saved)
	}
	now = now.Add(15 * time.Minute)
	setCounter(t, netstat, "TcpExt", "ListenOverflows", 500)
	o2.Sample()
	o2.save()
	if got := readFile(t, filepath.Join(Dir, observationsName)); got != saved {
		t.Error("written after Close")
	}
}

// TestObserverStartsOverOnABadFile: an unreadable file is said and
// replaced, not fatal.
func TestObserverStartsOverOnABadFile(t *testing.T) {
	fakeNode(t)
	mustWrite(t, filepath.Join(Dir, observationsName), "{not json")
	var logged []string
	o := &Observer{Proc: "testdata/proc", Now: time.Now, Logf: func(f string, a ...any) { logged = append(logged, f) }}
	o.load()
	if len(logged) != 1 || !strings.Contains(logged[0], "starting over") {
		t.Errorf("logged %q", logged)
	}
	o.Sample()
	o.save()
	if !strings.Contains(readFile(t, filepath.Join(Dir, observationsName)), `"version":1`) {
		t.Error("not replaced")
	}
}

// TestSnapshotSuggests: the Manager shows what the rules suggest from
// what the node observed - checked like a change, so a suggested range
// over a port the node listens on isn't shown - and never writes it.
func TestSnapshotSuggests(t *testing.T) {
	booted(t)
	range_ := Lookup("net.ipv4.ip_local_port_range")
	mustWrite(t, range_.Path(Root), "32768\t60999\n")
	now := time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)
	hour := now.Unix() / 3600
	o := &Observer{Proc: fakeProc(t), Now: func() time.Time { return now }, Logf: t.Logf}
	o.since = now.Add(-5 * time.Hour)
	o.hours = map[string][]bucket{"source_ports": {
		{Hour: hour - 3, Value: 0.55, Abs: 15600, Detail: "10.0.0.5:8080"},
		{Hour: hour - 2, Value: 0.6, Abs: 17000, Detail: "10.0.0.5:8080"},
		{Hour: hour - 1, Value: 0.4, Abs: 11000, Detail: "10.0.0.5:8080"},
		{Hour: hour, Value: 0.51, Abs: 14500, Detail: "10.0.0.6:8080"},
	}}
	o.listening = []int64{22, 9505, 10056}

	m := NewManager(true, fakeProbes{listening: []int64{22, 9505, 10056}, open: 1000, mem: 1 << 30}, nil)
	m.Observe(o)
	s := m.Snapshot()
	rec := paramState(t, s, range_.Name).Recommendation
	if rec == nil || rec.Value != "10240 65023" || rec.RuleID != "port_range.usage" {
		t.Fatalf("suggestion %+v", rec)
	}
	if got := measuredText(*rec); !strings.Contains(got, "60% (17,000 sockets) to 10.0.0.5:8080 at the highest; from 50% in 3 different hours") {
		t.Errorf("measured %q", got)
	}
	if s.Observation == nil || s.Observation.Signals["source_ports"].Hours != 3 || !s.Observation.Since.Equal(now.Add(-5*time.Hour)) {
		t.Errorf("observation %+v", s.Observation)
	}
	if v := liveValue(t, range_.Name); v != "32768 60999" {
		t.Errorf("a suggestion was applied: %s", v)
	}

	// A port the node listens on inside the suggested range: no suggestion.
	m = NewManager(true, fakeProbes{listening: []int64{9505, 10056, 32400}, open: 1000, mem: 1 << 30}, nil)
	m.Observe(o)
	if rec := paramState(t, m.Snapshot(), range_.Name).Recommendation; rec != nil {
		t.Errorf("a suggestion the node would refuse: %+v", rec)
	}
	// No observer: no suggestion, no observation.
	m = NewManager(true, quietProbes, nil)
	if s := m.Snapshot(); s.Observation != nil || paramState(t, s, range_.Name).Recommendation != nil {
		t.Error("suggestions without an observer")
	}
}

func paramState(t *testing.T, s Snapshot, name string) ParamState {
	t.Helper()
	for _, ps := range s.Params {
		if ps.Param.Name == name {
			return ps
		}
	}
	t.Fatalf("no %s", name)
	return ParamState{}
}

// TestObserverClockJumps: a machine that boots without a clock samples
// in 1970 until NTP sets it - the observation starts over then, the
// hours of 1970 go; a clock set back counts no hour after it.
func TestObserverClockJumps(t *testing.T) {
	fakeNode(t)
	proc := fakeProc(t)
	mustWrite(t, filepath.Join(proc, "sys/fs/file-nr"), "900\t0\t1000\n")
	now := time.Unix(600, 0)
	o := &Observer{Proc: proc, Now: func() time.Time { return now }, Logf: t.Logf}
	o.Sample()
	if got := o.Observation(); got.Signals["open_files"].Hours != 1 {
		t.Fatalf("1970's hour: %+v", got.Signals["open_files"])
	}

	now = time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)
	o.Sample()
	got := o.Observation()
	if !got.Since.Equal(now) || got.Signals["open_files"].Hours != 1 {
		t.Errorf("after the clock was set: since %v, %+v - want now, this hour only", got.Since, got.Signals["open_files"])
	}
	for _, b := range o.hours["open_files"] {
		if b.Hour < now.Add(-Keep).Unix()/3600 {
			t.Errorf("1970's hour kept: %+v", b)
		}
	}

	// Set back two hours: the hour "ahead" doesn't count, and buckets
	// stay in the order of their hours.
	now = now.Add(-2 * time.Hour)
	o.Sample()
	if got := o.Observation(); !got.Since.Equal(now) || got.Signals["open_files"].Hours != 1 {
		t.Errorf("set back: since %v, %+v", got.Since, got.Signals["open_files"])
	}
	o.finish()
	if bs := o.hours["open_files"]; !slices.IsSortedFunc(bs, func(a, b bucket) int { return int(a.Hour - b.Hour) }) || len(bs) != 2 {
		t.Errorf("buckets %+v, want two, in order", bs)
	}
}

// TestObserverFindsALaterListener: HAProxy starting doesn't listen yet -
// its listener is found at the next sample after it does.
func TestObserverFindsALaterListener(t *testing.T) {
	fakeNode(t)
	o := &Observer{Proc: fakeProc(t), Now: time.Now, Logf: t.Logf, HAProxyPid: os.Getpid}
	o.Sample()
	addr := listenWith(t, 5)
	for _, l := range o.Metrics().Listeners {
		if l.Addr == addr {
			t.Fatalf("%s found before it listened", addr)
		}
	}
	o.Sample()
	if !slices.Contains(o.Metrics().Listeners, Listener{Addr: addr, Backlog: 5}) {
		t.Errorf("listeners %v: want %s, opened after the first sample", o.Metrics().Listeners, addr)
	}
}

// TestObserverWarnsOnce: a problem is logged when it appears or changes,
// not at every sample.
func TestObserverWarnsOnce(t *testing.T) {
	var logged []string
	o := &Observer{Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }}
	failure := errors.New("permission denied")
	o.warn("sockets", failure)
	o.warn("sockets", failure)
	o.warn("counters", nil)
	o.warn("sockets", nil)
	o.warn("sockets", failure)
	o.warn("sockets", errors.New("other"))
	want := []string{"sysctl: observe: sockets: permission denied", "sysctl: observe: sockets: permission denied", "sysctl: observe: sockets: other"}
	if !slices.Equal(logged, want) {
		t.Errorf("logged %q, want %q", logged, want)
	}
}
