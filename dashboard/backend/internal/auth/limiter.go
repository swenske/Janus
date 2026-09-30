package auth

import (
	"sync"
	"time"
)

// Login throttling: the admin password is the only thing between the
// network and full control of every registered node, and each attempt is
// only a bcrypt comparison away - nothing bounded how many an attacker
// could make. Throttled per client address rather than globally, so a
// stranger guessing can't lock the operator out from their own address.
const (
	// freeFailures is how many wrong passwords an address gets before
	// any delay - enough for a human's typos.
	freeFailures = 5
	// Each failure past freeFailures locks the address for twice as
	// long as the previous one, starting at minLockout, up to maxLockout.
	minLockout = time.Second
	maxLockout = 15 * time.Minute
	// forgetAfter is how long an address's failures are remembered after
	// its last one.
	forgetAfter = time.Hour
	// maxTracked bounds memory against an attacker cycling through
	// addresses: past it, the least recently failed address is dropped.
	maxTracked = 10000
)

type loginAttempts struct {
	failures    int
	lastFailure time.Time
	lockedUntil time.Time
}

// LoginLimiter throttles password attempts per client address.
type LoginLimiter struct {
	now func() time.Time // time.Now, replaced in tests

	mu    sync.Mutex
	addrs map[string]*loginAttempts
}

func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{now: time.Now, addrs: map[string]*loginAttempts{}}
}

// Allow reports whether addr may try a password now, and if not, how
// long until it may. Checked before the password is even compared, so a
// locked-out address costs no bcrypt work.
func (l *LoginLimiter) Allow(addr string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.addrs[addr]
	if !ok {
		return true, 0
	}
	if wait := a.lockedUntil.Sub(l.now()); wait > 0 {
		return false, wait
	}
	return true, 0
}

// Fail records a wrong password from addr.
func (l *LoginLimiter) Fail(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	a, ok := l.addrs[addr]
	if !ok {
		if len(l.addrs) >= maxTracked {
			l.prune(now)
		}
		a = &loginAttempts{}
		l.addrs[addr] = a
	}
	if now.Sub(a.lastFailure) > forgetAfter && !now.Before(a.lockedUntil) {
		a.failures = 0 // failures this old are forgotten
	}
	a.failures++
	a.lastFailure = now
	if over := a.failures - freeFailures; over > 0 {
		lock := maxLockout
		if over <= 20 { // 2^20 s is far past maxLockout already
			lock = min(minLockout<<(over-1), maxLockout)
		}
		a.lockedUntil = now.Add(lock)
	}
}

// Succeed forgets addr's failures after a correct password.
func (l *LoginLimiter) Succeed(addr string) {
	l.mu.Lock()
	delete(l.addrs, addr)
	l.mu.Unlock()
}

// prune makes room once maxTracked addresses are held: it drops those
// whose last failure is older than forgetAfter (and whose lockout, if
// any, is over), then the least recently failed ones while the table is
// still full. Only run when full, so an ordinary failure costs O(1).
// Called with mu held.
func (l *LoginLimiter) prune(now time.Time) {
	for addr, a := range l.addrs {
		if now.Sub(a.lastFailure) > forgetAfter && !now.Before(a.lockedUntil) {
			delete(l.addrs, addr)
		}
	}
	for len(l.addrs) >= maxTracked {
		var oldest string
		for addr, a := range l.addrs {
			if oldest == "" || a.lastFailure.Before(l.addrs[oldest].lastFailure) {
				oldest = addr
			}
		}
		delete(l.addrs, oldest)
	}
}
