// Package ratelimit bounds how often a client address may do something
// the Controller can't authenticate first - announce a node on the
// registration port, say: per address, so a stranger flooding it doesn't
// stop a node elsewhere from registering.
package ratelimit

import (
	"sync"
	"time"
)

// maxTracked bounds memory against a client cycling through addresses:
// past it, the buckets that are full again and then the least recently
// used ones are dropped.
const maxTracked = 10000

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a token bucket per client address: Burst at once, then one
// more every Every.
type Limiter struct {
	Burst int
	Every time.Duration

	now func() time.Time // time.Now, replaced in tests

	mu    sync.Mutex
	addrs map[string]*bucket
}

// New makes a Limiter letting each address through burst times at once,
// then once every every.
func New(burst int, every time.Duration) *Limiter {
	return &Limiter{Burst: burst, Every: every, now: time.Now, addrs: map[string]*bucket{}}
}

// Allow takes one token for addr: whether it had one, and if not, how
// long until it gets one.
func (l *Limiter) Allow(addr string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.addrs[addr]
	if !ok {
		if len(l.addrs) >= maxTracked {
			l.prune(now)
		}
		b = &bucket{tokens: float64(l.Burst)}
		l.addrs[addr] = b
	} else {
		b.tokens = min(float64(l.Burst), b.tokens+float64(now.Sub(b.last))/float64(l.Every))
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) * float64(l.Every))
}

// prune makes room once maxTracked addresses are held: it drops the
// buckets full again (the address hasn't been seen for Burst*Every),
// then the least recently seen ones while the table is still full. Only
// run when full, so an ordinary call costs O(1). Called with mu held.
func (l *Limiter) prune(now time.Time) {
	full := time.Duration(l.Burst) * l.Every
	for addr, b := range l.addrs {
		if now.Sub(b.last) >= full {
			delete(l.addrs, addr)
		}
	}
	for len(l.addrs) >= maxTracked {
		var oldest string
		for addr, b := range l.addrs {
			if oldest == "" || b.last.Before(l.addrs[oldest].last) {
				oldest = addr
			}
		}
		delete(l.addrs, oldest)
	}
}
