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
	items   []T
	first   uint64 // sequence number of items[0]
	cap     int
	changed chan struct{}
}

func New[T any](capacity int) *Ring[T] {
	return &Ring[T]{cap: capacity, changed: make(chan struct{})}
}

// Append adds v and returns its sequence number.
func (r *Ring[T]) Append(v T) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	seq := r.first + uint64(len(r.items))
	r.items = append(r.items, v)
	if len(r.items) > r.cap {
		drop := len(r.items) - r.cap
		r.items = append(r.items[:0:0], r.items[drop:]...)
		r.first += uint64(drop)
	}
	close(r.changed)
	r.changed = make(chan struct{})
	return seq
}

// Since returns the items with a sequence number >= seq still held, and
// the sequence number to pass next time.
func (r *Ring[T]) Since(seq uint64) ([]T, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	end := r.first + uint64(len(r.items))
	if seq < r.first {
		seq = r.first
	}
	if seq >= end {
		return nil, end
	}
	return append([]T(nil), r.items[seq-r.first:]...), end
}

// Last returns up to n of the most recent items (all of them if n <= 0),
// and the sequence number to follow from.
func (r *Ring[T]) Last(n int) ([]T, uint64) {
	r.mu.Lock()
	end := r.first + uint64(len(r.items))
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
	for i := range r.items {
		fn(&r.items[i])
	}
}

// Changed returns a channel closed on the next Append. Take it before
// calling Since, then wait on it only if Since returned nothing, so an
// Append in between is never missed.
func (r *Ring[T]) Changed() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.changed
}

// LineWriter is an io.Writer that appends each complete line written to
// it (without its trailing newline) to a Ring. Safe for concurrent use.
type LineWriter struct {
	Ring *Ring[string]

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
			w.Ring.Append(string(w.partial))
			w.partial = w.partial[:0]
			continue
		}
		w.partial = append(w.partial, c)
		if len(w.partial) >= maxLine {
			w.Ring.Append(string(w.partial))
			w.partial = w.partial[:0]
		}
	}
	return len(p), nil
}
