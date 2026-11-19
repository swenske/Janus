package cluster

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// signal hands out one channel until it fires, as haproxy.Manager.Changed
// and Store.DrainChanged do.
type signal struct {
	mu sync.Mutex
	ch chan struct{}
}

func (s *signal) get() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ch == nil {
		s.ch = make(chan struct{})
	}
	return s.ch
}

func (s *signal) fire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ch != nil {
		close(s.ch)
		s.ch = nil
	}
}

// goroutinesSettle waits for the goroutine count to come down to at most
// want, and returns the count.
func goroutinesSettle(want int) int {
	n := runtime.NumGoroutine()
	for deadline := time.Now().Add(2 * time.Second); n > want && time.Now().Before(deadline); n = runtime.NumGoroutine() {
		time.Sleep(10 * time.Millisecond)
	}
	return n
}

func closed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	case <-time.After(time.Second):
		return false
	}
}

// The health gates take Either's signal at every tick, and nothing
// changes for days: it must not start a goroutine each time - janusd
// grew 10 MiB an hour on kluster-prod's BGP members (a goroutine a
// second), 5 MiB on keepalived's (one every two).
func TestEitherOneGoroutinePerChange(t *testing.T) {
	var a, b signal
	base := runtime.NumGoroutine()
	changed := Either(a.get, b.get)

	first := changed()
	for range 10000 {
		if c := changed(); c != first {
			t.Fatal("nothing changed, yet another channel")
		}
	}
	if n := goroutinesSettle(base + 1); n > base+1 {
		t.Fatalf("%d goroutines after 10000 calls without a change, want at most %d", n, base+1)
	}

	// Either side still wakes the gate, then a new signal is handed out
	// and the old goroutine is gone.
	for i, s := range []*signal{&a, &b, &a} {
		cur := changed()
		s.fire()
		if !closed(cur) {
			t.Fatalf("change %d: the signal didn't fire", i)
		}
		next := changed()
		if next == cur {
			t.Fatalf("change %d: the fired signal handed out again", i)
		}
		for range 1000 {
			changed()
		}
		if n := goroutinesSettle(base + 1); n > base+1 {
			t.Fatalf("change %d: %d goroutines, want at most %d", i, n, base+1)
		}
	}

	// Several callers (keepalived's gate and BIRD's) share one signal.
	x, y := changed(), changed()
	b.fire()
	if !closed(x) || !closed(y) {
		t.Fatal("a caller's signal didn't fire")
	}
}
