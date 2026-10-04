package main

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/swenske/Janus/dashboard/backend/internal/machines"
)

// consoleHub shares each machine's serial console. A hypervisor hands a
// console to one client at a time (libvirt: without forcing, a second
// one is refused), and several read it: every page showing it, and the
// Controller itself while it waits for a node to register
// (machines_watch.go). One stream per machine, opened for its first
// reader and closed after its last; private keys are hidden before any
// reader gets a byte (keyRedactor).
type consoleHub struct {
	mu      sync.Mutex
	streams map[string]*consoleStream
}

type consoleStream struct {
	id     string
	cancel context.CancelFunc

	mu   sync.Mutex
	subs map[*consoleSub]struct{}
}

// consoleSub is one reader. done receives the console's end: nil when
// it closed, else why.
type consoleSub struct {
	s    *consoleStream
	w    io.Writer
	done chan error
}

// subscribe gives w what the machine id's console prints from now on,
// until unsubscribe or the console's end (sub.done). open streams the
// console; it's only called when no stream is open yet.
func (h *consoleHub) subscribe(id string, open func(ctx context.Context, w io.Writer) error, w io.Writer) *consoleSub {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams == nil {
		h.streams = map[string]*consoleStream{}
	}
	s, ok := h.streams[id]
	var ctx context.Context
	if !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		s = &consoleStream{id: id, cancel: cancel, subs: map[*consoleSub]struct{}{}}
		h.streams[id] = s
	}
	sub := &consoleSub{s: s, w: w, done: make(chan error, 1)}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	// Started once its first reader is in: what the console prints
	// first would otherwise go to nobody.
	if !ok {
		go h.run(ctx, s, open)
	}
	return sub
}

func (h *consoleHub) run(ctx context.Context, s *consoleStream, open func(ctx context.Context, w io.Writer) error) {
	red := newKeyRedactor(fanout{s})
	err := open(ctx, red)
	_ = red.Flush()
	if ctx.Err() != nil {
		err = nil // its last reader left
	}
	h.mu.Lock()
	if h.streams[s.id] == s {
		delete(h.streams, s.id)
	}
	h.mu.Unlock()
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		sub.done <- err
		delete(s.subs, sub)
	}
}

// unsubscribe stops giving sub's writer anything; the stream closes
// with its last reader.
func (h *consoleHub) unsubscribe(sub *consoleSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := sub.s
	s.mu.Lock()
	delete(s.subs, sub)
	empty := len(s.subs) == 0
	s.mu.Unlock()
	if empty {
		if h.streams[s.id] == s {
			delete(h.streams, s.id)
		}
		s.cancel()
	}
}

// fanout writes to every reader of a stream. Readers don't block: a
// page's coalescer only buffers, the registration watcher only scans.
type fanout struct{ s *consoleStream }

func (f fanout) Write(p []byte) (int, error) {
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	for sub := range f.s.subs {
		_, _ = sub.w.Write(p)
	}
	return len(p), nil
}

var errNoVM = errors.New("the machine has no virtual machine yet")

// machineConsole is what streams m's console, for the hub.
func (a *app) machineConsole(m *machines.Machine) (func(ctx context.Context, w io.Writer) error, error) {
	if m.Ref == nil || m.Ref.UUID == "" {
		return nil, errNoVM
	}
	h, ok := a.hypervisors.Get(m.Spec.HypervisorID)
	if !ok {
		return nil, errors.New("its hypervisor no longer exists")
	}
	ref := *m.Ref
	return func(ctx context.Context, w io.Writer) error {
		drv, err := newDriver(h, a.controllerID)
		if err != nil {
			return err
		}
		defer drv.Close()
		return drv.Console(ctx, ref, w)
	}, nil
}
