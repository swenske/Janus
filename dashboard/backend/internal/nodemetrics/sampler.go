// Package nodemetrics keeps the last minutes of each node's figures - its
// CPU, HAProxy's requests, its traffic - for the Controller's node list,
// whose cards draw them: one sampler however many pages look, sampling a
// node only while a page shows it (Watch renews a lease on it) and
// keeping what it read for a while after, so a page opened again finds
// its curves already drawn.
package nodemetrics

import (
	"context"
	"sync"
	"time"
)

// Counters is one reading of a node: cumulative counters, turned into
// rates between two readings.
type Counters struct {
	TotalTicks, IdleTicks uint64 // the CPUs' ticks, all and idle
	RxBytes, TxBytes      uint64 // every interface but the loopback
	Requests              uint64 // HAProxy's requests since it started
	HAProxy               bool   // HAProxy answered (Requests is its)
}

// Reader reads node id's counters.
type Reader func(ctx context.Context, id string) (*Counters, error)

// Options: zero values take the defaults.
type Options struct {
	Read      Reader
	Interval  time.Duration // between two readings of a node: 5 s
	Retention time.Duration // what's kept: 10 min
	Lease     time.Duration // a node sampled after its last Watch: 45 s
	Timeout   time.Duration // a reading's limit: 4 s
	Parallel  int           // readings at once: 16
	Now       func() time.Time
}

// Series is a node's figures, a column each, oldest first: the time of
// each reading (the Controller's, Unix ms) and its rates - CPU busy %,
// HAProxy requests a second, bytes a second received and sent - null
// where there's none (the first reading, a failed one, a counter that
// went back, HAProxy not answering).
type Series struct {
	T   []int64    `json:"t"`
	CPU []*float64 `json:"cpu"`
	Req []*float64 `json:"req"`
	Rx  []*float64 `json:"rx"`
	Tx  []*float64 `json:"tx"`
}

type point struct {
	t                int64
	cpu, req, rx, tx float64
	hasCPU, hasReq   bool
	hasRx, hasTx     bool
}

type node struct {
	lease  time.Time
	busy   bool // a reading in flight: a slow node isn't read twice at once
	prev   *Counters
	prevAt time.Time
	ring   []point
}

// Sampler samples the nodes watched; New starts nothing until the first
// Watch, and its loop stops once every lease has lapsed.
type Sampler struct {
	o       Options
	ctx     context.Context
	cancel  context.CancelFunc
	sem     chan struct{}
	mu      sync.Mutex
	nodes   map[string]*node
	running bool
	closed  bool
	loops   sync.WaitGroup
}

func New(o Options) *Sampler {
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	if o.Retention <= 0 {
		o.Retention = 10 * time.Minute
	}
	if o.Lease <= 0 {
		o.Lease = 45 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 4 * time.Second
	}
	if o.Parallel <= 0 {
		o.Parallel = 16
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Sampler{o: o, ctx: ctx, cancel: cancel, sem: make(chan struct{}, o.Parallel), nodes: map[string]*node{}}
}

// Interval is the time between two readings of a node.
func (s *Sampler) Interval() time.Duration { return s.o.Interval }

// Watch keeps the nodes ids sampled for another lease.
func (s *Sampler) Watch(ids []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	until := s.o.Now().Add(s.o.Lease)
	for _, id := range ids {
		n := s.nodes[id]
		if n == nil {
			n = &node{}
			s.nodes[id] = n
		}
		n.lease = until
	}
	if !s.running && len(ids) > 0 {
		s.running = true
		s.loops.Add(1)
		go s.loop()
	}
}

// Snapshot is the series of the nodes ids, their readings after since
// (Unix ms; 0: all) - a node never read has none.
func (s *Sampler) Snapshot(ids []string, since int64) map[string]Series {
	out := map[string]Series{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		n := s.nodes[id]
		if n == nil {
			continue
		}
		var sr Series
		for _, p := range n.ring {
			if p.t <= since {
				continue
			}
			sr.T = append(sr.T, p.t)
			sr.CPU = append(sr.CPU, opt(p.cpu, p.hasCPU))
			sr.Req = append(sr.Req, opt(p.req, p.hasReq))
			sr.Rx = append(sr.Rx, opt(p.rx, p.hasRx))
			sr.Tx = append(sr.Tx, opt(p.tx, p.hasTx))
		}
		if sr.T != nil {
			out[id] = sr
		}
	}
	return out
}

func opt(v float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return &v
}

// Forget drops a node's series: it was removed.
func (s *Sampler) Forget(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, id)
}

// Close stops sampling, the readings in flight cancelled, and waits.
func (s *Sampler) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.loops.Wait()
}

func (s *Sampler) loop() {
	defer s.loops.Done()
	var reads sync.WaitGroup
	defer reads.Wait()
	tick := time.NewTicker(s.o.Interval)
	defer tick.Stop()
	for s.round(&reads) {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// round starts a reading of every node with a lease that isn't being
// read already, and forgets what's old; false when no lease is left -
// the loop ends (a Watch starts another).
func (s *Sampler) round(reads *sync.WaitGroup) bool {
	now := s.o.Now()
	var due []string
	s.mu.Lock()
	live := false
	for id, n := range s.nodes {
		if !now.Before(n.lease) {
			if len(n.ring) == 0 || now.Sub(time.UnixMilli(n.ring[len(n.ring)-1].t)) > s.o.Retention {
				delete(s.nodes, id)
			}
			continue
		}
		live = true
		if !n.busy {
			n.busy = true
			due = append(due, id)
		}
	}
	if !live || s.closed {
		s.running = false
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	for _, id := range due {
		reads.Add(1)
		go func() {
			defer reads.Done()
			select {
			case s.sem <- struct{}{}:
			case <-s.ctx.Done():
				s.record(id, now, nil, s.ctx.Err())
				return
			}
			defer func() { <-s.sem }()
			at := s.o.Now()
			ctx, cancel := context.WithTimeout(s.ctx, s.o.Timeout)
			c, err := s.o.Read(ctx, id)
			cancel()
			s.record(id, at, c, err)
		}()
	}
	return true
}

func (s *Sampler) record(id string, at time.Time, c *Counters, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	if n == nil {
		return // forgotten meanwhile
	}
	n.busy = false
	p := point{t: at.UnixMilli()}
	if err != nil || c == nil {
		// No rate across a failure: the next reading starts afresh.
		n.prev = nil
	} else {
		if b := n.prev; b != nil {
			if dt := at.Sub(n.prevAt).Seconds(); dt > 0 {
				if c.TotalTicks > b.TotalTicks && c.IdleTicks >= b.IdleTicks {
					busy := 1 - float64(c.IdleTicks-b.IdleTicks)/float64(c.TotalTicks-b.TotalTicks)
					p.cpu, p.hasCPU = 100*min(1, max(0, busy)), true
				}
				if c.RxBytes >= b.RxBytes {
					p.rx, p.hasRx = float64(c.RxBytes-b.RxBytes)/dt, true
				}
				if c.TxBytes >= b.TxBytes {
					p.tx, p.hasTx = float64(c.TxBytes-b.TxBytes)/dt, true
				}
				// A reload starts HAProxy's count again: no rate then.
				if c.HAProxy && b.HAProxy && c.Requests >= b.Requests {
					p.req, p.hasReq = float64(c.Requests-b.Requests)/dt, true
				}
			}
		}
		n.prev, n.prevAt = c, at
	}
	n.ring = append(n.ring, p)
	if keep := int(s.o.Retention / s.o.Interval); len(n.ring) > keep {
		n.ring = append(n.ring[:0:0], n.ring[len(n.ring)-keep:]...)
	}
}
