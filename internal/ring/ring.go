// Package ring is a bounded, in-memory, append-only buffer that readers
// can both page through and follow - the storage behind janusd's event
// log (internal/events) and per-service logs (SystemService.Logs).
package ring

import (
	"sync"
)

// Ring keeps the last Cap items appended. Every item gets a sequence
// number (0, 1, 2, ... over the Ring's whole life), so a reader can
// resume exactly where it stopped even after older items fall off.
type Ring[T any] struct {
	mu      sync.Mutex
	buf     []T    // up to cap items; once full, a circle starting at start
	start   int    // index in buf of the oldest item
	first   uint64 // sequence number of the oldest item
	cap     int
	changed chan struct{}
	watched bool // changed was handed out since the last Append
}

func New[T any](capacity int) *Ring[T] {
	return &Ring[T]{cap: capacity, changed: make(chan struct{})}
}

// Append adds v and returns its sequence number. It takes the same time
// however full the Ring is: once full, v takes the oldest item's place
// (a log ring takes a line per HAProxy request).
func (r *Ring[T]) Append(v T) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	seq := r.first + uint64(len(r.buf))
	switch {
	case len(r.buf) < r.cap:
		r.buf = append(r.buf, v)
	case r.cap > 0:
		r.buf[r.start] = v
		r.start = (r.start + 1) % r.cap
		r.first++
	default:
		r.first++
	}
	if r.watched {
		close(r.changed)
		r.changed = make(chan struct{})
		r.watched = false
	}
	return seq
}

// Since returns the items with a sequence number >= seq still held, and
// the sequence number to pass next time.
func (r *Ring[T]) Since(seq uint64) ([]T, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	end := r.first + uint64(len(r.buf))
	if seq < r.first {
		seq = r.first
	}
	if seq >= end {
		return nil, end
	}
	n := int(end - seq)
	i := (r.start + int(seq-r.first)) % len(r.buf)
	out := make([]T, 0, n)
	if i+n <= len(r.buf) {
		return append(out, r.buf[i:i+n]...), end
	}
	out = append(out, r.buf[i:]...)
	return append(out, r.buf[:n-(len(r.buf)-i)]...), end
}

// Last returns up to n of the most recent items (all of them if n <= 0),
// and the sequence number to follow from.
func (r *Ring[T]) Last(n int) ([]T, uint64) {
	r.mu.Lock()
	end := r.first + uint64(len(r.buf))
	r.mu.Unlock()
	start := uint64(0)
	if n > 0 && end > uint64(n) {
		start = end - uint64(n)
	}
	return r.Since(start)
}

// Each calls fn with every item held, oldest first, under the Ring's
// lock - nothing is copied, so fn must neither keep the pointer nor
// call back into the Ring.
func (r *Ring[T]) Each(fn func(*T)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.buf {
		fn(&r.buf[(r.start+i)%len(r.buf)])
	}
}

// Changed returns a channel closed on the next Append. Take it before
// calling Since, then wait on it only if Since returned nothing, so an
// Append in between is never missed.
func (r *Ring[T]) Changed() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watched = true
	return r.changed
}

// LineWriter is an io.Writer that appends each complete line written to
// it (without its trailing newline) to a Ring. Safe for concurrent use.
type LineWriter struct {
	Ring *Ring[string]
	// Tee, when set, is also given each line (internal/logvol keeps
	// them across reboots). It must not wait.
	Tee func(string)

	mu      sync.Mutex
	partial []byte
}

// maxLine caps a line with no newline in sight, so a misbehaving writer
// can't grow the pending buffer without bound.
const maxLine = 64 * 1024

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range p {
		if c == '\n' {
			w.line()
			continue
		}
		w.partial = append(w.partial, c)
		if len(w.partial) >= maxLine {
			w.line()
		}
	}
	return len(p), nil
}

func (w *LineWriter) line() {
	line := string(w.partial)
	w.partial = w.partial[:0]
	w.Ring.Append(line)
	if w.Tee != nil {
		w.Tee(line)
	}
}
