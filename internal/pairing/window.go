package pairing

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"sync"
	"time"
)

// MaxFailures is how many wrong secrets close a window: the secret's 36
// bits then leave a guesser 5 in 2^36.
const MaxFailures = 5

// ErrClosed is a call to a window that's closed - paired, given a fleet
// otherwise, out of tries or of time.
type ErrClosed struct{ Why string }

func (e *ErrClosed) Error() string { return "the pairing window is closed: " + e.Why }

// ErrWrong is a wrong secret.
type ErrWrong struct{ Left int }

func (e *ErrWrong) Error() string {
	if e.Left == 0 {
		return fmt.Sprintf("a wrong pairing code - %d wrong ones: the window is closed until the node restarts", MaxFailures)
	}
	return fmt.Sprintf("a wrong pairing code - %d tries left before the window closes", e.Left)
}

// Window is a pairing window: open until closed, a secret good once.
type Window struct {
	mu       sync.Mutex
	secret   []byte
	failures int
	why      string // closed: why; "" while open
	until    time.Time
	done     chan struct{}
	now      func() time.Time
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

// Check takes secret: nil when it's the window's and the window is open;
// an *ErrClosed, or an *ErrWrong that counts against the window.
func (w *Window) Check(secret []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.why == "" && !w.until.IsZero() && w.now().After(w.until) {
		w.closeLocked("its time is up")
	}
	if w.why != "" {
		return &ErrClosed{Why: w.why}
	}
	if subtle.ConstantTimeCompare(secret, w.secret) == 1 {
		return nil
	}
	w.failures++
	left := MaxFailures - w.failures
	if left <= 0 {
		left = 0
		w.closeLocked(fmt.Sprintf("%d wrong codes", MaxFailures))
	}
	return &ErrWrong{Left: left}
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
