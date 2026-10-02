package main

import (
	"bytes"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

// keyRedactor passes a console's output through, replacing every PEM
// private key block with a notice: a node's first boot prints its root
// admin credential on its console, and the Controller - which never
// holds it - mustn't relay it either. It holds back just enough output
// to recognize a marker cut across two writes.
type keyRedactor struct {
	w     io.Writer
	buf   []byte
	inKey bool
}

var (
	pemBegin = []byte("-----BEGIN ")
	pemEnd   = []byte("-----END ")
	pemDash  = []byte("-----")
)

// keyNotice replaces a private key.
const keyNotice = "[private key hidden by the Controller]"

// maxMarker bounds a PEM marker line's length ("-----BEGIN ENCRYPTED
// PRIVATE KEY-----" is the longest common one): past it, held output is
// not a marker after all.
const maxMarker = 64

func newKeyRedactor(w io.Writer) *keyRedactor { return &keyRedactor{w: w} }

func (r *keyRedactor) Write(p []byte) (int, error) {
	r.buf = append(r.buf, p...)
	var out []byte
	for {
		if r.inKey {
			i := bytes.Index(r.buf, pemEnd)
			if i < 0 {
				// Drop the key's body, keeping what may start the END marker.
				r.buf = tail(r.buf, len(pemEnd)-1)
				break
			}
			j := bytes.Index(r.buf[i+len(pemEnd):], pemDash)
			if j < 0 {
				r.buf = r.buf[i:]
				if len(r.buf) > maxMarker {
					r.buf = r.buf[len(pemEnd):] // not a marker: keep looking
				}
				break
			}
			r.buf = r.buf[i+len(pemEnd)+j+len(pemDash):]
			r.inKey = false
			out = append(out, keyNotice...)
			continue
		}
		i := bytes.Index(r.buf, pemBegin)
		if i < 0 {
			keep := partialPrefix(r.buf, pemBegin)
			out = append(out, r.buf[:len(r.buf)-keep]...)
			r.buf = r.buf[len(r.buf)-keep:]
			break
		}
		j := bytes.Index(r.buf[i+len(pemBegin):], pemDash)
		if j < 0 && len(r.buf)-i <= maxMarker {
			out = append(out, r.buf[:i]...)
			r.buf = r.buf[i:]
			break // the marker isn't complete yet
		}
		if j < 0 || j > maxMarker {
			// Not a marker after all.
			out = append(out, r.buf[:i+len(pemBegin)]...)
			r.buf = r.buf[i+len(pemBegin):]
			continue
		}
		label := r.buf[i+len(pemBegin) : i+len(pemBegin)+j]
		end := i + len(pemBegin) + j + len(pemDash)
		if bytes.Contains(label, []byte("PRIVATE KEY")) {
			out = append(out, r.buf[:i]...)
			r.buf = r.buf[end:]
			r.inKey = true
			continue
		}
		out = append(out, r.buf[:end]...)
		r.buf = r.buf[end:]
	}
	if len(out) > 0 {
		if _, err := r.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes what's held back once the console ends - never part of a
// key.
func (r *keyRedactor) Flush() error {
	if r.inKey || len(r.buf) == 0 {
		r.buf = nil
		return nil
	}
	_, err := r.w.Write(r.buf)
	r.buf = nil
	return err
}

// partialPrefix is how many bytes at the end of b could start marker.
func partialPrefix(b, marker []byte) int {
	for n := min(len(b), len(marker)-1); n > 0; n-- {
		if bytes.Equal(b[len(b)-n:], marker[:n]) {
			return n
		}
	}
	return 0
}

func tail(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append([]byte(nil), b[len(b)-n:]...)
}

// utf8Chunks sends a byte stream as text, holding back a UTF-8 sequence
// cut at the end of a write until the next one completes it - the
// console's packets split characters anywhere, and a half character
// would turn into U+FFFD on both sides of the cut.
type utf8Chunks struct {
	send  func(text string) error
	carry []byte
}

func (u *utf8Chunks) Write(p []byte) (int, error) {
	buf := append(u.carry, p...)
	n := completeUTF8(buf)
	u.carry = append([]byte(nil), buf[n:]...)
	if n > 0 {
		if err := u.send(string(buf[:n])); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush sends what's held back, complete or not.
func (u *utf8Chunks) Flush() error {
	if len(u.carry) == 0 {
		return nil
	}
	err := u.send(string(u.carry))
	u.carry = nil
	return err
}

// completeUTF8 is the length of b without a trailing, incomplete UTF-8
// sequence (invalid bytes count as complete: they can't get better).
func completeUTF8(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return len(b)
			}
			return i
		}
	}
	return len(b)
}

// coalescer gathers a console's output and passes it on at most every
// period (or once maxPending bytes are waiting): a serial console comes
// in packets of a few bytes, and one event per packet would flood the
// browser - and cut nearly every escape sequence in two.
type coalescer struct {
	w io.Writer

	mu      sync.Mutex
	pending []byte
	err     error
	stop    chan struct{}
	stopped sync.WaitGroup
}

const maxPending = 32 << 10

func newCoalescer(w io.Writer, period time.Duration) *coalescer {
	c := &coalescer{w: w, stop: make(chan struct{})}
	c.stopped.Add(1)
	go func() {
		defer c.stopped.Done()
		t := time.NewTicker(period)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-t.C:
				c.mu.Lock()
				c.flushLocked()
				c.mu.Unlock()
			}
		}
	}()
	return c
}

func (c *coalescer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	c.pending = append(c.pending, p...)
	if len(c.pending) >= maxPending {
		c.flushLocked()
	}
	return len(p), c.err
}

func (c *coalescer) flushLocked() {
	if len(c.pending) == 0 || c.err != nil {
		return
	}
	_, c.err = c.w.Write(c.pending)
	c.pending = c.pending[:0]
}

// Close stops the timer and passes on what's left.
func (c *coalescer) Close() error {
	close(c.stop)
	c.stopped.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushLocked()
	return c.err
}
