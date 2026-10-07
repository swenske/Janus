package sysctl

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/swenske/Janus/internal/sockdiag"
)

// How often the Observer samples: the counters, gauges and listeners
// every SampleEvery, the source ports and the outgoing rate every
// PortsEvery samples; what it keeps goes to STATE every SaveEvery.
const (
	SampleEvery = 15 * time.Second
	PortsEvery  = 4
	SaveEvery   = 10 * time.Minute
)

// observationsName is the Observer's file in Dir.
const observationsName = "sysctl-observations.json"

// Observer watches the Signals suggestions rest on: kernel counters
// summed by hour, gauges maxed by hour, HAProxy's listening sockets -
// on the node's own /proc and sock_diag. It keeps Keep of hours on
// STATE, so a reboot loses at most SaveEvery of them.
type Observer struct {
	Proc string // /proc
	// HAProxyPid is the running HAProxy's pid, 0 when none runs: its
	// listeners are told from the others by their inodes - root creates
	// them, before HAProxy turns into uid 1000.
	HAProxyPid func() int
	Now        func() time.Time
	Logf       Logf

	mu      sync.Mutex
	since   time.Time
	hours   map[string][]bucket // finished hours (and saved parts), oldest first
	cur     map[string]*bucket  // the hour in progress
	curHour int64
	prev    *counters
	samples int
	opens   struct {
		at time.Time
		n  int64
	}
	listeners []Listener
	listening []int64
	conntrack bool
	inodes    struct {
		pid       int
		listening string // the system's listening sockets the set was read for
		set       map[uint32]bool
	}
	savedAt time.Time
	last    time.Time // the latest sample's time
	closed  bool      // saved for good: the machine is going down

	warnMu sync.Mutex
	warned map[string]string // what -> the problem last logged
}

// bucket is a signal's hour: a Counter's increase, a Gauge's highest
// value - and what that was in absolute, and what peaked.
type bucket struct {
	Hour   int64   `json:"h"` // Unix time / 3600
	Value  float64 `json:"v"`
	Abs    int64   `json:"a,omitempty"`
	Detail string  `json:"d,omitempty"`
}

// fold adds a sample to b: a Counter's increase adds up, a Gauge keeps
// its highest value and its highest absolute.
func (b *bucket) fold(kind SignalKind, value float64, abs int64, detail string) {
	if kind == Counter {
		b.Value += value
		return
	}
	if value > b.Value {
		b.Value, b.Detail = value, detail
	}
	b.Abs = max(b.Abs, abs)
}

// NewObserver is an Observer of the node's /proc.
func NewObserver(haproxyPid func() int) *Observer {
	return &Observer{Proc: "/proc", HAProxyPid: haproxyPid, Now: time.Now, Logf: func(format string, args ...any) {}}
}

// Run loads what's kept, then samples until ctx ends - and saves.
func (o *Observer) Run(ctx context.Context) {
	o.load()
	t := time.NewTicker(SampleEvery)
	defer t.Stop()
	for {
		o.Sample()
		select {
		case <-ctx.Done():
			o.save()
			return
		case <-t.C:
		}
	}
}

// counters are the kernel's cumulative counters a sample reads.
type counters struct {
	listenOverflows, synCookies, tcpMemory, activeOpens int64
	backlogDrops, conntrackDrops                        int64
}

// Sample reads the kernel once: the counters, gauges and listeners, and
// every PortsEvery-th time the source ports and the outgoing rate too.
func (o *Observer) Sample() {
	now := o.Now()
	o.mu.Lock()
	o.samples++
	withPorts := o.samples%PortsEvery == 1
	o.mu.Unlock()

	c, err := o.readCounters()
	o.warn("counters", err)
	listeners, listening, lerr := o.readListeners()
	o.warn("listening sockets", lerr)
	var ports struct {
		ratio  float64
		count  int64
		detail string
		ok     bool
	}
	if withPorts {
		ports.ratio, ports.count, ports.detail, ports.ok = o.sourcePorts()
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	o.roll(now)
	if err == nil {
		if o.prev != nil {
			d := func(now, before int64) float64 { return float64(max(now-before, 0)) } // a counter that went back: reset
			o.fold("accept_overflows", d(c.listenOverflows, o.prev.listenOverflows), 0, "")
			o.fold("syn_cookies", d(c.synCookies, o.prev.synCookies), 0, "")
			o.fold("tcp_memory", d(c.tcpMemory, o.prev.tcpMemory), 0, "")
			o.fold("backlog_drops", d(c.backlogDrops, o.prev.backlogDrops), 0, "")
			o.fold("conntrack_drops", d(c.conntrackDrops, o.prev.conntrackDrops), 0, "")
		}
		o.prev = &c
		if withPorts {
			if !o.opens.at.IsZero() && now.Sub(o.opens.at) >= SampleEvery {
				o.fold("outgoing_rate", float64(max(c.activeOpens-o.opens.n, 0))/now.Sub(o.opens.at).Seconds(), 0, "")
			}
			o.opens.at, o.opens.n = now, c.activeOpens
		}
	}
	if f, err := readInts(o.path("sys/fs/file-nr")); err == nil && len(f) == 3 && f[2] > 0 {
		o.fold("open_files", float64(f[0])/float64(f[2]), f[0], "")
	}
	if n, err := readInt64(o.path("sys/net/netfilter/nf_conntrack_count")); err == nil {
		o.conntrack = o.conntrack || n > 0
		if limit, err := readInt64(o.path("sys/net/netfilter/nf_conntrack_max")); err == nil && limit > 0 {
			o.fold("conntrack_usage", float64(n)/float64(limit), n, "")
		}
	}
	if ports.ok {
		o.fold("source_ports", ports.ratio, ports.count, ports.detail)
	}
	if lerr == nil {
		o.listeners, o.listening = listeners, listening
	}
	if now.Sub(o.savedAt) >= SaveEvery {
		o.saveLocked(now)
	}
}

// warn logs a problem with what once, and again only when it changes -
// err nil: none any more. A sample every 15 seconds mustn't fill janusd's
// log with one line.
func (o *Observer) warn(what string, err error) {
	o.warnMu.Lock()
	defer o.warnMu.Unlock()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if o.warned[what] == msg {
		return
	}
	if o.warned == nil {
		o.warned = map[string]string{}
	}
	o.warned[what] = msg
	if err != nil {
		o.Logf("sysctl: observe: %s: %v", what, err)
	}
}

// fold adds a sample to signal id's hour in progress - a Counter that
// didn't increase and a Gauge at 0 leave no hour behind.
func (o *Observer) fold(id string, value float64, abs int64, detail string) {
	def := LookupSignal(id)
	if value <= 0 && abs <= 0 {
		return
	}
	b := o.cur[id]
	if b == nil {
		b = &bucket{Hour: o.curHour}
		o.cur[id] = b
	}
	b.fold(def.Kind, value, abs, detail)
}

// roll starts the hour of now, the one in progress joining the finished
// ones; and forgets those older than Keep. A clock that jumped - set at
// last, on a machine that boots without one - starts the observation
// over at now; the hours kept stay in theirs.
func (o *Observer) roll(now time.Time) {
	hour := now.Unix() / 3600
	if o.hours == nil {
		o.hours = map[string][]bucket{}
	}
	if o.since.IsZero() || o.since.After(now) || !o.last.IsZero() && (now.Sub(o.last) > Keep || o.last.Sub(now) > time.Hour) {
		o.since = now
	}
	o.last = now
	if o.cur != nil && hour == o.curHour {
		return
	}
	o.finish()
	o.cur, o.curHour = map[string]*bucket{}, hour
	oldest := (now.Add(-Keep).Unix())/3600 + 1
	for id, bs := range o.hours {
		i := 0
		for i < len(bs) && bs[i].Hour < oldest {
			i++
		}
		if i == len(bs) {
			delete(o.hours, id)
		} else {
			o.hours[id] = bs[i:]
		}
	}
}

// finish puts the hour in progress with the finished ones.
func (o *Observer) finish() {
	for id, b := range o.cur {
		o.hours[id] = merged(o.hours[id], *b, LookupSignal(id).Kind)
	}
	o.cur = nil
}

// merged is bs with b, in the order of their hours - folded into the
// bucket of the same hour (one saved before a restart).
func merged(bs []bucket, b bucket, kind SignalKind) []bucket {
	i, found := slices.BinarySearchFunc(bs, b.Hour, func(x bucket, hour int64) int { return cmp.Compare(x.Hour, hour) })
	if !found {
		return slices.Insert(bs, i, b)
	}
	if kind == Counter {
		bs[i].Value += b.Value
	} else {
		bs[i].fold(kind, b.Value, b.Abs, b.Detail)
	}
	return bs
}

// states are the signals over Window - the hour in progress included,
// none after it (a clock set back).
func (o *Observer) states(now time.Time) map[string]SignalState {
	from, to := now.Add(-Window).Unix()/3600+1, now.Unix()/3600
	out := map[string]SignalState{}
	for _, def := range Signals {
		bs := slices.Clone(o.hours[def.ID])
		if b := o.cur[def.ID]; b != nil {
			bs = merged(bs, *b, def.Kind)
		}
		var st SignalState
		for _, b := range bs {
			if b.Hour < from || b.Hour > to {
				continue
			}
			seen := def.Kind == Counter && b.Value > 0 || def.Kind == Gauge && b.Value >= def.Threshold
			if seen {
				st.Hours++
				st.LastSeen = time.Unix(b.Hour*3600, 0)
			}
			if def.Kind == Counter {
				st.Total += b.Value
			}
			if b.Value > st.Peak {
				st.Peak, st.Detail = b.Value, b.Detail
			}
			st.PeakAbs = max(st.PeakAbs, b.Abs)
		}
		out[def.ID] = st
	}
	return out
}

// Metrics is what rules read: the machine, HAProxy's listeners and the
// signals over Window - Live is the Manager's to fill.
func (o *Observer) Metrics() Metrics {
	mem, _ := ProcProbes{Proc: o.Proc}.MemTotal()
	o.mu.Lock()
	defer o.mu.Unlock()
	return Metrics{
		CPUs:           runtime.NumCPU(),
		MemTotal:       mem,
		Signals:        o.states(o.Now()),
		Listeners:      slices.Clone(o.listeners),
		ListeningPorts: slices.Clone(o.listening),
		Conntrack:      o.conntrack,
	}
}

// Observation is what SysctlList shows of the Observer: since when -
// the oldest hour kept, if older than the observation's start - and the
// signals.
func (o *Observer) Observation() Observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.Now()
	since := o.since
	for _, bs := range o.hours {
		if len(bs) > 0 {
			if t := time.Unix(bs[0].Hour*3600, 0); t.Before(since.Truncate(time.Hour)) {
				since = t
			}
		}
	}
	if oldest := now.Add(-Keep); since.Before(oldest) {
		since = oldest
	}
	return Observation{Since: since, Signals: o.states(now)}
}

func (o *Observer) path(rel string) string { return filepath.Join(o.Proc, rel) }

// readCounters reads the kernel's counters: /proc/net/netstat and snmp,
// softnet_stat, and conntrack's statistics when it's there.
func (o *Observer) readCounters() (counters, error) {
	var c counters
	netstat, err := readTable(o.path("net/netstat"))
	if err != nil {
		return c, err
	}
	snmp, err := readTable(o.path("net/snmp"))
	if err != nil {
		return c, err
	}
	ext := netstat["TcpExt"]
	c.listenOverflows = ext["ListenOverflows"]
	c.synCookies = ext["SyncookiesSent"]
	c.tcpMemory = ext["TCPMemoryPressures"] + ext["TCPAbortOnMemory"]
	c.activeOpens = snmp["Tcp"]["ActiveOpens"]
	if c.backlogDrops, err = softnetDropped(o.path("net/softnet_stat")); err != nil {
		return c, err
	}
	if cols, err := statColumns(o.path("net/stat/nf_conntrack")); err == nil {
		c.conntrackDrops = cols["drop"] + cols["early_drop"] + cols["insert_failed"]
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	return c, nil
}

// readListeners lists the node's listening TCP sockets: HAProxy's, and
// every port listened on.
func (o *Observer) readListeners() ([]Listener, []int64, error) {
	pid := 0
	if o.HAProxyPid != nil {
		pid = o.HAProxyPid()
	}
	var all []sockdiag.Socket
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := sockdiag.TCP(family, sockdiag.States(sockdiag.Listen), func(s sockdiag.Socket) { all = append(all, s) }); err != nil {
			return nil, nil, err
		}
	}
	inodes := o.haproxyInodes(pid, all)
	var listeners []Listener
	var ports []int64
	for _, s := range all {
		ports = append(ports, int64(s.Src.Port()))
		if inodes[s.Inode] {
			listeners = append(listeners, Listener{Addr: s.Src, Backlog: s.WQueue})
		}
	}
	slices.Sort(ports)
	return listeners, slices.Compact(ports), nil
}

// haproxyInodes are the listening sockets' inodes that HAProxy pid holds:
// read from its file descriptors again only when HAProxy or the system's
// listening sockets change - a starting HAProxy doesn't listen yet.
func (o *Observer) haproxyInodes(pid int, listening []sockdiag.Socket) map[uint32]bool {
	if pid == 0 {
		return nil
	}
	want := map[uint32]bool{}
	var inodes []string
	for _, s := range listening {
		want[s.Inode] = true
		inodes = append(inodes, strconv.FormatUint(uint64(s.Inode), 10))
	}
	slices.Sort(inodes)
	key := strings.Join(inodes, ",")
	o.mu.Lock()
	cached := o.inodes
	o.mu.Unlock()
	if cached.pid == pid && cached.listening == key {
		return cached.set
	}
	set := map[uint32]bool{}
	dir := o.path(strconv.Itoa(pid) + "/fd")
	entries, err := os.ReadDir(dir)
	o.warn("HAProxy's descriptors", err)
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]"), 10, 32); err == nil && want[uint32(n)] {
			set[uint32(n)] = true
		}
	}
	o.mu.Lock()
	o.inodes.pid, o.inodes.listening, o.inodes.set = pid, key, set
	o.mu.Unlock()
	return set
}

// sourcePorts is the highest share of the source port range one
// destination's sockets take - TIME_WAIT ones included -, their count
// and the destination.
func (o *Observer) sourcePorts() (ratio float64, count int64, dst string, ok bool) {
	rng, err := readInts(o.path("sys/net/ipv4/ip_local_port_range"))
	if err != nil || len(rng) != 2 {
		return 0, 0, "", false
	}
	reservedText, _ := os.ReadFile(o.path("sys/net/ipv4/ip_local_reserved_ports"))
	reserved := portSet(strings.TrimSpace(string(reservedText)))
	usable := rng[1] - rng[0] + 1
	for p := range reserved {
		if p >= rng[0] && p <= rng[1] {
			usable--
		}
	}
	counts := map[netip.AddrPort]int64{}
	for _, family := range []uint8{unix.AF_INET, unix.AF_INET6} {
		if err := sockdiag.TCP(family, sockdiag.AllButListen, func(s sockdiag.Socket) {
			if p := int64(s.Src.Port()); p >= rng[0] && p <= rng[1] && !reserved[p] {
				counts[s.Dst]++
			}
		}); err != nil {
			o.warn("sockets", err)
			return 0, 0, "", false
		}
	}
	o.warn("sockets", nil)
	var top netip.AddrPort
	for d, n := range counts {
		if n > count || n == count && d.String() < top.String() {
			top, count = d, n
		}
	}
	if usable <= 0 || count == 0 {
		return 0, 0, "", true
	}
	return float64(count) / float64(usable), count, top.String(), true
}

// observations is the Observer's file.
type observations struct {
	Version int                 `json:"version"`
	Since   int64               `json:"since"`
	Hours   map[string][]bucket `json:"hours"`
}

func observationsPath() string { return filepath.Join(Dir, observationsName) }

// load reads what's kept; a file it can't read starts over, said.
func (o *Observer) load() {
	data, err := os.ReadFile(observationsPath())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var f observations
	if err == nil {
		err = json.Unmarshal(data, &f)
	}
	if err == nil && f.Version != 1 {
		err = fmt.Errorf("version %d", f.Version)
	}
	if err != nil {
		o.Logf("sysctl: observe: %s: %v - starting over", observationsName, err)
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.hours = map[string][]bucket{}
	for id, bs := range f.Hours {
		if def := LookupSignal(id); def != nil {
			slices.SortFunc(bs, func(a, b bucket) int { return cmp.Compare(a.Hour, b.Hour) })
			o.hours[id] = bs
		}
	}
	if f.Since > 0 {
		o.since = time.Unix(f.Since, 0)
	}
}

func (o *Observer) save() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.saveLocked(o.Now())
}

// Close saves what's kept, for the last time: the machine is going down
// (internal/shutdown) - or STATE is about to be wiped, and nothing must
// be written there again.
func (o *Observer) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.saveLocked(o.Now())
	o.closed = true
}

// saveLocked writes the finished hours and the one in progress.
func (o *Observer) saveLocked(now time.Time) {
	if o.closed {
		return
	}
	o.savedAt = now
	f := observations{Version: 1, Since: o.since.Unix(), Hours: map[string][]bucket{}}
	for id, bs := range o.hours {
		f.Hours[id] = slices.Clone(bs)
	}
	for id, b := range o.cur {
		f.Hours[id] = merged(f.Hours[id], *b, LookupSignal(id).Kind)
	}
	data, err := json.Marshal(f)
	if err == nil {
		err = os.MkdirAll(Dir, 0o755)
	}
	if err == nil {
		err = writeAtomic(observationsPath(), data)
	}
	o.warn("saving", err)
}
