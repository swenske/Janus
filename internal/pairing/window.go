package pairing

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"
)

// MaxFailures is how many wrong secrets from one source address pause it:
// the secret's 36 bits then leave a guesser 5 in 2^36 per source, and the
// window stays open for everyone else (security review 2026-11-05 #17).
const MaxFailures = 5

// FailurePause is how long a source that sent MaxFailures wrong codes is
// turned away before it may try again.
const FailurePause = time.Minute

// maxSources bounds the per-source failure table.
const maxSources = 1024

// A rescue window (a rescue volume at boot) stays open RescueLifetime,
// and the certificate it gives is good for RescueValidity.
const (
	RescueLifetime = 30 * time.Minute
	RescueValidity = time.Hour
)

// ErrClosed is a call to a window that's closed - paired, given a fleet
// otherwise, out of tries or of time.
type ErrClosed struct{ Why string }

func (e *ErrClosed) Error() string { return "the pairing window is closed: " + e.Why }

// ErrWrong is a wrong secret. Blocked: this source sent MaxFailures wrong
// codes and is paused for FailurePause - its further calls aren't audited.
type ErrWrong struct {
	Left    int
	Blocked bool
}

func (e *ErrWrong) Error() string {
	if e.Blocked {
		return fmt.Sprintf("%d wrong pairing codes from your address: wait %s and try again", MaxFailures, FailurePause)
	}
	return fmt.Sprintf("a wrong pairing code - %d tries left from your address before a pause", e.Left)
}

// IsBlocked reports whether err is a paused source's answer (not worth
// auditing again).
func IsBlocked(err error) bool {
	var w *ErrWrong
	return errors.As(err, &w) && w.Blocked
}

// Window is a pairing window: open until closed, a secret good once.
type Window struct {
	mu     sync.Mutex
	secret []byte
	fails  map[string]*sourceFails // wrong codes per source address
	why    string                  // closed: why; "" while open
	until  time.Time
	done   chan struct{}
	now    func() time.Time
}

// sourceFails counts one source address's wrong codes and, past
// MaxFailures, pauses it until block.
type sourceFails struct {
	n     int
	block time.Time
	seen  time.Time
}

// NewWindow opens a window for secret (Code.SecretBytes), for lifetime -
// 0: until it's closed.
func NewWindow(secret []byte, lifetime time.Duration) *Window {
	w := &Window{secret: append([]byte(nil), secret...), done: make(chan struct{}), now: time.Now}
	if lifetime > 0 {
		w.until = w.now().Add(lifetime)
	}
	return w
}

// Check takes a call's source address and secret: nil when it's the
// window's and the window is open; an *ErrClosed, or an *ErrWrong. Wrong
// codes are counted per source, so one source can't shut the window for
// everyone - past MaxFailures a source is paused (Blocked), the window
// stays open (security review 2026-11-05 #17).
func (w *Window) Check(source string, secret []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if w.why == "" && !w.until.IsZero() && now.After(w.until) {
		w.closeLocked("its time is up")
	}
	if w.why != "" {
		return &ErrClosed{Why: w.why}
	}
	if sf := w.fails[source]; sf != nil && !sf.block.IsZero() && now.Before(sf.block) {
		return &ErrWrong{Left: 0, Blocked: true}
	}
	if subtle.ConstantTimeCompare(secret, w.secret) == 1 {
		return nil
	}
	return w.failLocked(source, now)
}

// Fail records a wrong attempt from source without a secret to check - a
// call that reached the window but can't succeed (the wrong window kind):
// it is rate-limited the same way. w.mu is taken here.
func (w *Window) Fail(source string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.why != "" {
		return &ErrClosed{Why: w.why}
	}
	now := w.now()
	if sf := w.fails[source]; sf != nil && !sf.block.IsZero() && now.Before(sf.block) {
		return &ErrWrong{Left: 0, Blocked: true}
	}
	return w.failLocked(source, now)
}

// failLocked counts a wrong attempt from source and pauses it past
// MaxFailures. w.mu held.
func (w *Window) failLocked(source string, now time.Time) error {
	if w.fails == nil {
		w.fails = map[string]*sourceFails{}
	}
	sf := w.fails[source]
	if sf == nil {
		if len(w.fails) >= maxSources {
			w.evictOldestLocked()
		}
		sf = &sourceFails{}
		w.fails[source] = sf
	}
	if !sf.block.IsZero() && now.After(sf.block) {
		sf.n, sf.block = 0, time.Time{} // the pause passed: a fresh start
	}
	sf.n++
	sf.seen = now
	left := MaxFailures - sf.n
	if left <= 0 {
		sf.block = now.Add(FailurePause)
		return &ErrWrong{Left: 0, Blocked: true}
	}
	return &ErrWrong{Left: left}
}

// evictOldestLocked drops the least-recently-seen source to bound the
// table. w.mu held.
func (w *Window) evictOldestLocked() {
	var oldest string
	var at time.Time
	for s, sf := range w.fails {
		if oldest == "" || sf.seen.Before(at) {
			oldest, at = s, sf.seen
		}
	}
	delete(w.fails, oldest)
}

// Close closes the window, saying why - once.
func (w *Window) Close(why string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeLocked(why)
}

func (w *Window) closeLocked(why string) {
	if w.why != "" {
		return
	}
	w.why = why
	close(w.done)
}

// Done is closed once the window is.
func (w *Window) Done() <-chan struct{} { return w.done }

// Closed is why the window is closed - "" while it's open.
func (w *Window) Closed() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.why
}

// IsClosed reports whether err is a closed window's answer.
func IsClosed(err error) bool {
	var c *ErrClosed
	return errors.As(err, &c)
}
