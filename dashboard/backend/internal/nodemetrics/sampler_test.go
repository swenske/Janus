package nodemetrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 5 s for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A node whose readings the test scripts, on a clock that moves 5 s a
// reading: the rates are exact.
type scripted struct {
	mu    sync.Mutex
	reads int
	steps []func(*Counters) error
	c     Counters
}

func (s *scripted) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.UnixMilli(1_000_000).Add(time.Duration(s.reads) * 5 * time.Second)
}

func (s *scripted) read(context.Context, string) (*Counters, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.reads
	s.reads++
	if i >= len(s.steps) {
		return nil, errors.New("no more")
	}
	if err := s.steps[i](&s.c); err != nil {
		return nil, err
	}
	c := s.c
	return &c, nil
}

func val(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%g", math.Round(*p*1000)/1000)
}

func TestRates(t *testing.T) {
	grow := func(c *Counters) {
		c.TotalTicks += 400
		c.IdleTicks += 100
		c.RxBytes += 5000
		c.TxBytes += 10000
		c.Requests += 50
		c.HAProxy = true
	}
	busy := func(c *Counters) error {
		grow(c)
		return nil
	}
	sc := &scripted{steps: []func(*Counters) error{
		busy, // first reading: no rate yet
		busy,
		func(c *Counters) error { // HAProxy reloaded: its count starts again
			grow(c)
			c.Requests = 3
			return nil
		},
		busy,
		func(*Counters) error { return errors.New("unreachable") },
		busy, // after a failure: no rate across it
		busy,
		func(c *Counters) error { // HAProxy down
			grow(c)
			c.HAProxy = false
			return nil
		},
	}}
	s := New(Options{Read: sc.read, Now: sc.now, Interval: 2 * time.Millisecond, Lease: time.Hour, Retention: time.Hour})
	defer s.Close()
	s.Watch([]string{"n1"})
	waitFor(t, "8 readings", func() bool { return len(s.Snapshot([]string{"n1"}, 0)["n1"].T) >= 8 })
	sr := s.Snapshot([]string{"n1"}, 0)["n1"]
	var got []string
	for i := range 8 {
		got = append(got, fmt.Sprintf("%d cpu=%s req=%s rx=%s tx=%s", sr.T[i]-1_000_000, val(sr.CPU[i]), val(sr.Req[i]), val(sr.Rx[i]), val(sr.Tx[i])))
	}
	want := []string{
		"0 cpu=- req=- rx=- tx=-",
		"5000 cpu=75 req=10 rx=1000 tx=2000",
		"10000 cpu=75 req=- rx=1000 tx=2000",
		"15000 cpu=75 req=10 rx=1000 tx=2000",
		"20000 cpu=- req=- rx=- tx=-",
		"25000 cpu=- req=- rx=- tx=-",
		"30000 cpu=75 req=10 rx=1000 tx=2000",
		"35000 cpu=75 req=- rx=1000 tx=2000",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reading %d: %s, want %s", i, got[i], want[i])
		}
	}
	// since: only what's newer.
	if later := s.Snapshot([]string{"n1"}, 1_000_000+25000)["n1"]; len(later.T) == 0 || later.T[0] != 1_000_000+30000 {
		t.Errorf("since: %v", later.T)
	}
}

// counting reads every node it's asked, counting by node.
type counting struct {
	mu     sync.Mutex
	by     map[string]int
	in     int
	maxIn  int
	block  map[string]bool
	ticks  uint64
	called atomic.Int64
}

func (c *counting) read(ctx context.Context, id string) (*Counters, error) {
	c.called.Add(1)
	c.mu.Lock()
	if c.by == nil {
		c.by = map[string]int{}
	}
	c.by[id]++
	c.in++
	c.maxIn = max(c.maxIn, c.in)
	block := c.block[id]
	c.ticks += 100
	out := &Counters{TotalTicks: c.ticks}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.in--
		c.mu.Unlock()
	}()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	time.Sleep(time.Millisecond)
	return out, nil
}

func (c *counting) reads(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.by[id]
}

func TestOnlyWatchedNodesWhileLeased(t *testing.T) {
	c := &counting{}
	s := New(Options{Read: c.read, Interval: 5 * time.Millisecond, Lease: 60 * time.Millisecond, Retention: time.Hour})
	defer s.Close()
	s.Watch([]string{"a"})
	waitFor(t, "a read", func() bool { return c.reads("a") >= 3 })
	if c.reads("b") != 0 {
		t.Errorf("b read without a watch")
	}
	// The lease lapses: the readings stop.
	waitFor(t, "the loop to stop", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.running
	})
	n := c.called.Load()
	time.Sleep(30 * time.Millisecond)
	if got := c.called.Load(); got != n {
		t.Errorf("read %d times after the lease", got-n)
	}
	// Kept: a page opened again draws them at once.
	if len(s.Snapshot([]string{"a"}, 0)["a"].T) < 3 {
		t.Errorf("history gone with the lease")
	}
	// A watch starts it again.
	s.Watch([]string{"a", "b"})
	waitFor(t, "b read", func() bool { return c.reads("b") >= 2 })
}

func TestForgottenAfterRetention(t *testing.T) {
	c := &counting{}
	s := New(Options{Read: c.read, Interval: 5 * time.Millisecond, Lease: 20 * time.Millisecond, Retention: 40 * time.Millisecond})
	defer s.Close()
	s.Watch([]string{"a", "b"})
	waitFor(t, "readings", func() bool { return c.reads("a") >= 2 })
	// b keeps its lease; a's lapses, then its history.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				s.Watch([]string{"b"})
			}
		}
	}()
	defer close(stop)
	waitFor(t, "a forgotten", func() bool { return len(s.Snapshot([]string{"a"}, 0)) == 0 })
	if len(s.Snapshot([]string{"b"}, 0)["b"].T) == 0 {
		t.Errorf("b forgotten")
	}
	// The ring holds Retention / Interval readings.
	waitFor(t, "b's ring full", func() bool { return c.reads("b") > 20 })
	if n := len(s.Snapshot([]string{"b"}, 0)["b"].T); n > 8 {
		t.Errorf("b's ring: %d readings", n)
	}
	s.Forget("b")
	if len(s.Snapshot([]string{"b"}, 0)) != 0 {
		t.Errorf("Forget kept b")
	}
}

// A node that doesn't answer holds back neither the others nor itself
// twice, and the readings at once never exceed the bound.
func TestSlowNodesAndBound(t *testing.T) {
	c := &counting{block: map[string]bool{"slow": true}}
	s := New(Options{Read: c.read, Interval: 5 * time.Millisecond, Lease: time.Hour, Timeout: 200 * time.Millisecond, Parallel: 4})
	defer s.Close()
	ids := []string{"slow"}
	for i := range 20 {
		ids = append(ids, fmt.Sprintf("n%d", i))
	}
	s.Watch(ids)
	waitFor(t, "every node read 5 times", func() bool {
		for _, id := range ids[1:] {
			if c.reads(id) < 5 {
				return false
			}
		}
		return true
	})
	c.mu.Lock()
	maxIn, slow := c.maxIn, c.by["slow"]
	c.mu.Unlock()
	if maxIn > 4 {
		t.Errorf("%d readings at once, bound 4", maxIn)
	}
	if slow > 2 {
		t.Errorf("the slow node read %d times while it hung", slow)
	}
}

func TestConcurrentUse(t *testing.T) {
	c := &counting{}
	s := New(Options{Read: c.read, Interval: time.Millisecond, Lease: 10 * time.Millisecond, Retention: 20 * time.Millisecond, Parallel: 3})
	var wg sync.WaitGroup
	for g := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids := []string{fmt.Sprintf("n%d", g%7), fmt.Sprintf("n%d", (g+3)%7)}
			for i := range 40 {
				switch i % 4 {
				case 0, 1:
					s.Watch(ids)
				case 2:
					_ = s.Snapshot(ids, int64(i))
				case 3:
					s.Forget(ids[1])
				}
				time.Sleep(200 * time.Microsecond)
			}
		}()
	}
	wg.Wait()
	s.Close()
	n := c.called.Load()
	s.Watch([]string{"n1"})
	time.Sleep(10 * time.Millisecond)
	if got := c.called.Load(); got != n {
		t.Errorf("read %d times after Close", got-n)
	}
}

func TestNilSampler(t *testing.T) {
	var s *Sampler
	s.Watch([]string{"a"})
	s.Forget("a")
	if got := s.Snapshot([]string{"a"}, 0); len(got) != 0 {
		t.Errorf("nil sampler: %v", got)
	}
}
