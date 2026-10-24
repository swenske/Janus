package cluster

import "sync"

// A drained member is out of service without being stopped: janusd
// reports HAProxy unhealthy to keepalived (the virtual IPs move to a
// peer) and BIRD (the haproxy_* routes are withdrawn) while HAProxy
// keeps answering what still arrives - what a recycle does to a member
// before stopping it (docs/cluster.md). In memory only: a reboot ends it.

// Drained reports whether the member is drained.
func (s *Store) Drained() bool {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	return s.drained
}

// SetDrained drains the member, or puts it back; it reports whether
// that changed anything.
func (s *Store) SetDrained(drained bool) bool {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	if s.drained == drained {
		return false
	}
	s.drained = drained
	if s.drainCh != nil {
		close(s.drainCh)
		s.drainCh = nil
	}
	return true
}

// DrainChanged is a channel closed at the next change of the drain: for
// the health gates to react at once (vrrp.KeepHealth, bgp.KeepGate).
func (s *Store) DrainChanged() <-chan struct{} {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	if s.drainCh == nil {
		s.drainCh = make(chan struct{})
	}
	return s.drainCh
}

// Either is a change signal that fires when either of two does.
func Either(a, b func() <-chan struct{}) func() <-chan struct{} {
	return func() <-chan struct{} {
		ca, cb := a(), b()
		out := make(chan struct{})
		var once sync.Once
		go func() {
			select {
			case <-ca:
			case <-cb:
			}
			once.Do(func() { close(out) })
		}()
		return out
	}
}
