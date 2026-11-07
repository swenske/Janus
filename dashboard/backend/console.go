package main

import (
	"bytes"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/swenske/Janus/internal/pairing"
)

// keyRedactor passes a console's output through, replacing every PEM
// private key block with a notice: a node from before the pairing code
// prints its root admin credential on its console at its first boot, and
// the Controller - which never holds it - mustn't relay it either. It holds back just enough output
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

// codeRedactor passes a console's output through, replacing every
// pairing or rescue code (internal/pairing: five groups of four symbols,
// "XXXX-XXXX-XXXX-XXXX-XXXX") with a notice. janusd shows the code of
// its pairing or rescue window on the console only - a rescue code is an
// os:admin certificate of the node's own CA for whoever types it first -
// and the Controller, which relays a machine's console to the machine's
// operators, mustn't hand it to them (security review 2026-11-05 #12).
// Whoever opens a rescue window attached its volume at the hypervisor,
// whose own console shows the code.
//
// A code is a whole word of letters, digits and dashes: one that is
// part of a longer word ("8400-e29b-41d4-a716-446655440000") isn't. An
// escape sequence ends a word - the motd prints the code in bold,
// "\x1b[1mXXXX-...\x1b[0m", and the sequence's final letter would
// otherwise stick to it. The word at the end of a write is held back
// while it may still become a code: 24 bytes at most.
type codeRedactor struct {
	w    io.Writer
	word []byte // the word so far, while it may be a code
	dead bool   // the current word can't be one: passed through
	esc  int    // 1 after ESC, 2 inside a CSI sequence
}

// codeNotice replaces a code.
const codeNotice = "[hidden by the Controller - read it on the hypervisor's console]"

// codeLen is a code's length as shown: its symbols and a dash between
// each group of four.
const codeLen = pairing.Symbols + pairing.Symbols/4 - 1

func newCodeRedactor(w io.Writer) *codeRedactor { return &codeRedactor{w: w} }

func isWordByte(b byte) bool {
	return b == '-' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

// mayBeCode: word is the start of a code - or a whole one, at codeLen.
func mayBeCode(word []byte) bool {
	if len(word) > codeLen {
		return false
	}
	for i, b := range word {
		if (i%5 == 4) != (b == '-') {
			return false
		}
	}
	return true
}

func (r *codeRedactor) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(r.word)+len(p))
	for _, b := range p {
		switch r.esc {
		case 1:
			r.esc = 0
			if b == '[' {
				r.esc = 2
			}
			out = append(out, b)
			continue
		case 2:
			if b >= 0x40 && b <= 0x7e {
				r.esc = 0
			}
			out = append(out, b)
			continue
		}
		if isWordByte(b) {
			if r.dead {
				out = append(out, b)
				continue
			}
			r.word = append(r.word, b)
			if !mayBeCode(r.word) {
				out = append(out, r.word...)
				r.word, r.dead = r.word[:0], true
			}
			continue
		}
		out = r.endWord(out)
		if b == 0x1b {
			r.esc = 1
		}
		out = append(out, b)
	}
	if len(out) > 0 {
		if _, err := r.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// endWord appends the word that just ended to out: the notice for a code.
func (r *codeRedactor) endWord(out []byte) []byte {
	if len(r.word) == codeLen {
		out = append(out, codeNotice...)
	} else {
		out = append(out, r.word...)
	}
	r.word, r.dead = r.word[:0], false
	return out
}

// Flush writes the word held back once the console ends - hidden if it's
// a whole code.
func (r *codeRedactor) Flush() error {
	out := r.endWord(nil)
	if len(out) == 0 {
		return nil
	}
	_, err := r.w.Write(out)
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
