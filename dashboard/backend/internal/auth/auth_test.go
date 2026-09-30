package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestLoginLimiter(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewLoginLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < freeFailures; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("attempt %d refused within the free failures", i+1)
		}
		l.Fail("a")
	}
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("the free failures alone must not lock")
	}

	// Each failure past the free ones doubles the lockout.
	for _, want := range []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		l.Fail("a")
		ok, wait := l.Allow("a")
		if ok || wait != want {
			t.Fatalf("after a failure: allowed=%v wait=%v, want locked for %v", ok, wait, want)
		}
		if ok, _ := l.Allow("b"); !ok {
			t.Fatal("another address must not be locked out")
		}
		now = now.Add(want)
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("still locked once %v passed", want)
		}
	}

	// Capped.
	for i := 0; i < 40; i++ {
		l.Fail("a")
	}
	if _, wait := l.Allow("a"); wait != maxLockout {
		t.Fatalf("lockout = %v, want the %v cap", wait, maxLockout)
	}

	// A success forgets everything.
	now = now.Add(maxLockout)
	l.Succeed("a")
	l.Fail("a")
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a success must reset the failure count")
	}

	// Old failures are forgotten: an address that exhausted its free
	// failures long ago starts over.
	for i := 0; i < freeFailures; i++ {
		l.Fail("d")
	}
	now = now.Add(forgetAfter + time.Second)
	l.Fail("d")
	if ok, _ := l.Allow("d"); !ok {
		t.Fatal("failures older than forgetAfter should be forgotten")
	}
}

func TestLoginLimiterBounded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewLoginLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i < maxTracked+50; i++ {
		now = now.Add(time.Millisecond)
		l.Fail(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	l.mu.Lock()
	n := len(l.addrs)
	_, newest := l.addrs[fmt.Sprintf("10.0.%d.%d", (maxTracked+49)/256, (maxTracked+49)%256)]
	l.mu.Unlock()
	if n > maxTracked || !newest {
		t.Fatalf("tracked %d addresses (max %d), newest kept: %v", n, maxTracked, newest)
	}
}

func TestSessionsSweptOnCreate(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.sessions["stale"] = time.Now().Add(-time.Minute)
	live, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.sessions["stale"]; ok {
		t.Error("an expired session survived NewSession")
	}
	if !s.ValidSession(live) {
		t.Error("the new session isn't valid")
	}
}
