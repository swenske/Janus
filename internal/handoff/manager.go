package handoff

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// The readings' rhythm: every FastEvery for FastFor after the handoff
// was asked for - what an orchestrator does right then matters (a drain,
// a member coming back) -, every SlowEvery after.
var (
	FastEvery = 2 * time.Second
	FastFor   = 2 * time.Minute
	SlowEvery = 30 * time.Second
	// ReadTimeout bounds one reading of a member's connections.
	ReadTimeout = 20 * time.Second
	// MaxDuration bounds a handoff.
	MaxDuration = 24 * time.Hour
)

// ProbeDocument is what a kernel without nft's dup statement refuses
// (nft --check): it tells whether this node can hand connections over.
const ProbeDocument = `table ip janus_handoff_probe {
	chain c {
		type filter hook prerouting priority raw; policy accept;
		ip daddr 127.0.0.2 dup to 127.0.0.1
	}
}
table ip6 janus_handoff_probe {
	chain c {
		type filter hook prerouting priority raw; policy accept;
		ip6 daddr ::2 dup to ::1
	}
}
`

// Reading is a member's connections, and where to copy them.
type Reading struct {
	Flows          []Flow
	Reach4, Reach6 netip.Addr
}

// Info is a handoff in effect.
type Info struct {
	Owner   string
	Flows   int
	Until   time.Time
	Read    time.Time
	Error   string
	Packets uint64
}

// Manager keeps the node's handoffs - one per member whose connections
// it copies - and their nftables tables.
type Manager struct {
	// Read reads member owner's connections on addrs (ClusterFlows).
	Read func(ctx context.Context, owner string, addrs []netip.Addr) (Reading, error)
	// Device is this node's interface toward a reach address.
	Device func(netip.Addr) (string, error)
	// Apply replaces the handoff tables with doc (firewall.ApplyReserved).
	Apply func(doc string) error
	// Counters reads the packets copied so far, by owner; nil: unknown.
	Counters func() (map[string]uint64, error)
	Now      func() time.Time

	mu      sync.Mutex
	owners  map[string]*owned
	refuse  guard
	hold    guard
	running bool
	stop    chan struct{}
	doc     atomic.Pointer[string]
	manual  bool // tests call tick themselves
}

// guard is a refusal or a hold: its addresses, until when.
type guard struct {
	addrs []netip.Addr
	until time.Time
}

func (g guard) active() bool { return len(g.addrs) > 0 }

// MaxGuard bounds a refusal or a hold.
const MaxGuard = time.Hour

// Guard starts - or moves the end of - the refusal (refuse) or the hold
// on addrs for d from now (Guards).
func (m *Manager) Guard(refuse bool, addrs []netip.Addr, d time.Duration) (time.Time, error) {
	if d <= 0 || d > MaxGuard {
		return time.Time{}, fmt.Errorf("a hold or a refusal lasts 1 s to %s", MaxGuard)
	}
	if len(addrs) == 0 {
		return time.Time{}, errors.New("no address to hold or refuse connections on")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g := &m.hold
	if refuse {
		g = &m.refuse
	}
	prev := *g
	*g = guard{addrs: slices.Clone(addrs), until: m.now().Add(d)}
	if err := m.install(); err != nil {
		*g = prev
		return time.Time{}, err
	}
	m.ensureLoop()
	return g.until, nil
}

// EndGuards ends the refusal and the hold.
func (m *Manager) EndGuards() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.refuse.active() && !m.hold.active() {
		return nil
	}
	m.refuse, m.hold = guard{}, guard{}
	return m.install()
}

// Guards' ends; zero: not in effect.
func (m *Manager) GuardsUntil() (refuse, hold time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refuse.until, m.hold.until
}

// ensureLoop starts the loop when it isn't running; m.mu is held.
func (m *Manager) ensureLoop() {
	if !m.running && !m.manual {
		m.running, m.stop = true, make(chan struct{})
		go m.loop(m.stop)
	}
}

type owned struct {
	addrs   []netip.Addr
	until   time.Time
	fast    time.Time // read every FastEvery until then
	next    time.Time
	reading Reading
	dev4    string
	dev6    string
	read    time.Time
	err     string
	busy    bool // a reading runs
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Document is the tables' document as last applied - for the firewall,
// which flushes every table when it replaces its ruleset.
func (m *Manager) Document() string {
	if d := m.doc.Load(); d != nil {
		return *d
	}
	return ""
}

// Start hands member owner's connections on addrs over for d from now:
// they are read at once - a handoff that starts can't do without - and
// copied to it; a handoff in effect is read again at once and lasts d
// from now (a failed reading then keeps the one before).
func (m *Manager) Start(ctx context.Context, owner string, addrs []netip.Addr, d time.Duration) (Info, error) {
	if d <= 0 || d > MaxDuration {
		return Info{}, fmt.Errorf("a handoff lasts 1 s to %s", MaxDuration)
	}
	if len(addrs) == 0 {
		return Info{}, errors.New("no address to hand the connections of over")
	}
	rctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	r, rerr := m.Read(rctx, owner, addrs)
	cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.owners == nil {
		m.owners = map[string]*owned{}
	}
	o, had := m.owners[owner]
	if rerr != nil && !had {
		return Info{}, fmt.Errorf("reading %s's connections: %w", owner, rerr)
	}
	if !had {
		o = &owned{}
		m.owners[owner] = o
	}
	now := m.now()
	o.addrs, o.until, o.fast = slices.Clone(addrs), now.Add(d), now.Add(FastFor)
	o.next = now.Add(FastEvery)
	if rerr != nil {
		o.err = rerr.Error()
	} else {
		m.take(o, r, now)
		// Connections it can't copy: a handoff that starts so would copy
		// nothing, unknown to whoever asked.
		if err := o.unreachable(); err != nil && !had {
			delete(m.owners, owner)
			return Info{}, fmt.Errorf("%s's connections: %v", owner, err)
		}
	}
	if err := m.install(); err != nil {
		if !had {
			delete(m.owners, owner)
		}
		return Info{}, err
	}
	m.ensureLoop()
	return m.info(owner, o, nil), nil
}

// take records reading r of o, taken at now.
func (m *Manager) take(o *owned, r Reading, now time.Time) {
	r.Flows = Sorted(r.Flows)
	o.reading, o.read, o.err = r, now, ""
	o.dev4, o.dev6 = "", ""
	if r.Reach4.IsValid() && m.Device != nil {
		if dev, err := m.Device(r.Reach4); err == nil {
			o.dev4 = dev
		} else {
			o.err = fmt.Sprintf("no route to %s: %v", r.Reach4, err)
		}
	}
	if r.Reach6.IsValid() && m.Device != nil {
		if dev, err := m.Device(r.Reach6); err == nil {
			o.dev6 = dev
		} else {
			o.err = fmt.Sprintf("no route to %s: %v", r.Reach6, err)
		}
	}
}

// unreachable says when o's reading has connections in a family it can't
// copy them in: no address of its member there, or no route to it.
func (o *owned) unreachable() error {
	v4, v6 := false, false
	for _, f := range o.reading.Flows {
		if f.Is6() {
			v6 = true
		} else {
			v4 = true
		}
	}
	switch {
	case v4 && o.dev4 == "":
		return fmt.Errorf("IPv4 connections, but no IPv4 address of its own on their link to copy them to (%s)", cmp.Or(o.err, "none given"))
	case v6 && o.dev6 == "":
		return fmt.Errorf("IPv6 connections, but no IPv6 address of its own on their link to copy them to (%s)", cmp.Or(o.err, "none given"))
	}
	return nil
}

// Stop ends the handoff to owner; none: nothing to do.
func (m *Manager) Stop(owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.owners[owner]; !ok {
		return nil
	}
	delete(m.owners, owner)
	return m.install()
}

// List is the handoffs in effect, by owner.
func (m *Manager) List() []Info {
	m.mu.Lock()
	none := len(m.owners) == 0
	m.mu.Unlock()
	if none {
		return nil
	}
	var counters map[string]uint64
	if m.Counters != nil {
		counters, _ = m.Counters()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.owners))
	for name, o := range m.owners {
		out = append(out, m.info(name, o, counters))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Owner < out[j].Owner })
	return out
}

func (m *Manager) info(name string, o *owned, counters map[string]uint64) Info {
	return Info{Owner: name, Flows: len(o.reading.Flows), Until: o.until, Read: o.read, Error: o.err, Packets: counters[name]}
}

// install applies the owners' document; m.mu is held. A document that
// fails isn't kept for the firewall's next ruleset.
func (m *Manager) install() error {
	var owners []Owner
	for name, o := range m.owners {
		owners = append(owners, Owner{Name: name, Reach4: o.reading.Reach4, Reach6: o.reading.Reach6, Dev4: o.dev4, Dev6: o.dev6, Flows: o.reading.Flows})
	}
	doc := Document(owners, Guards{Refuse: m.refuse.addrs, Hold: m.hold.addrs})
	prev := m.doc.Swap(&doc)
	if err := m.Apply(doc); err != nil {
		m.doc.Store(prev)
		return fmt.Errorf("the handoff's nftables tables: %w", err)
	}
	return nil
}

// loop reads the owners' connections again when due and ends the
// handoffs that expired, until there is none left.
func (m *Manager) loop(stop chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if !m.tick() {
			return
		}
	}
}

// tick does what is due; false: no handoff left, the loop ends.
func (m *Manager) tick() bool {
	m.mu.Lock()
	now := m.now()
	changed := false
	var due []string
	for name, o := range m.owners {
		if !now.Before(o.until) {
			delete(m.owners, name)
			log.Printf("cluster: handoff to %s over", name)
			changed = true
			continue
		}
		if !now.Before(o.next) && !o.busy {
			o.busy = true
			due = append(due, name)
		}
	}
	for _, g := range []*guard{&m.refuse, &m.hold} {
		if g.active() && !now.Before(g.until) {
			*g = guard{}
			changed = true
		}
	}
	if changed {
		if err := m.install(); err != nil {
			log.Printf("cluster: %v", err)
		}
	}
	if len(m.owners) == 0 && !m.refuse.active() && !m.hold.active() {
		m.running = false
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	for _, name := range due {
		m.refresh(name)
	}
	return true
}

// refresh reads owner name's connections again and installs them when
// they changed.
func (m *Manager) refresh(name string) {
	m.mu.Lock()
	o, ok := m.owners[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	addrs := slices.Clone(o.addrs)
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), ReadTimeout)
	r, err := m.Read(ctx, name, addrs)
	cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok = m.owners[name]
	if !ok {
		return
	}
	o.busy = false
	now := m.now()
	if now.Before(o.fast) {
		o.next = now.Add(FastEvery)
	} else {
		o.next = now.Add(SlowEvery)
	}
	if err != nil {
		// Its member may be rebooting: the copies follow the last reading.
		o.err = err.Error()
		return
	}
	r.Flows = Sorted(r.Flows)
	same := slices.Equal(o.reading.Flows, r.Flows) && o.reading.Reach4 == r.Reach4 && o.reading.Reach6 == r.Reach6
	m.take(o, r, now)
	if err := o.unreachable(); err != nil {
		o.err = err.Error()
	}
	if same {
		return
	}
	if err := m.install(); err != nil {
		o.err = err.Error()
		log.Printf("cluster: %v", err)
	}
}

// Close ends every handoff and removes the tables.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		close(m.stop)
		m.running = false
	}
	if len(m.owners) == 0 && !m.refuse.active() && !m.hold.active() {
		return nil
	}
	m.owners, m.refuse, m.hold = nil, guard{}, guard{}
	return m.install()
}

// Sorted is flows in Compare's order - a reading's, so two readings
// compare by value.
func Sorted(flows []Flow) []Flow {
	flows = slices.Clone(flows)
	slices.SortFunc(flows, Compare)
	return flows
}

// ParseCounters reads the packets each owner's rules copied from
// `nft --json list table` outputs (one per family).
func ParseCounters(outputs ...[]byte) (map[string]uint64, error) {
	out := map[string]uint64{}
	for _, b := range outputs {
		var doc struct {
			Nftables []struct {
				Rule *struct {
					Comment string           `json:"comment"`
					Expr    []map[string]any `json:"expr"`
				} `json:"rule"`
			} `json:"nftables"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, err
		}
		for _, item := range doc.Nftables {
			if item.Rule == nil || item.Rule.Comment == "" {
				continue
			}
			for _, e := range item.Rule.Expr {
				c, ok := e["counter"].(map[string]any)
				if !ok {
					continue
				}
				if p, ok := c["packets"].(float64); ok {
					out[item.Rule.Comment] += uint64(p)
				}
			}
		}
	}
	return out, nil
}
