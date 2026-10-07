package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

// TestLimiter: the burst goes through at once, the next call waits for
// a refill, each address has its own bucket, and the table stays
// bounded.
func TestLimiter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(3, 10*time.Second)
	l.now = func() time.Time { return now }

	for i := range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("call %d of the burst refused", i+1)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait != 10*time.Second {
		t.Fatalf("after the burst: ok=%v wait=%v, want refused for 10s", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("another address shares a's bucket")
	}
	now = now.Add(5 * time.Second)
	if ok, wait := l.Allow("a"); ok || wait != 5*time.Second {
		t.Fatalf("half a refill later: ok=%v wait=%v", ok, wait)
	}
	now = now.Add(5 * time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a full refill later: still refused")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("the refilled token was given twice")
	}
	// A bucket never fills past the burst, however long it rests.
	now = now.Add(time.Hour)
	for i := range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("after an hour, call %d refused", i+1)
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("an hour's rest gave more than the burst")
	}

	// Many addresses: the table is pruned at maxTracked, the full buckets
	// first.
	for i := range maxTracked {
		l.Allow(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	if len(l.addrs) > maxTracked {
		t.Fatalf("%d addresses tracked, want at most %d", len(l.addrs), maxTracked)
	}
	now = now.Add(time.Hour)
	l.Allow("fresh")
	if len(l.addrs) != 1 {
		t.Fatalf("after an hour every full bucket should be gone: %d tracked", len(l.addrs))
	}
}
