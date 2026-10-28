package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// A drained member is out of service without being stopped: janusd
// reports HAProxy unhealthy to keepalived (the virtual IPs move to a
// peer) and BIRD (the haproxy_* routes are withdrawn) while HAProxy
// keeps answering what still arrives - what a recycle does to a member
// before stopping it, what an operator does before a maintenance
// (docs/cluster.md). Drain keeps it on STATE (DrainedFile): a reboot, a
// crash, a janusd restart find the member drained again, from janusd's
// first instant - Open reads it before the health gates exist. Undrain
// and Leave clear it. Stopping is a janusd's last seconds, never kept.

// DrainedFile marks a drained member on STATE, next to the membership.
const DrainedFile = "drained"

// Drained reports whether the member is drained - what it says
// (ClusterInfo).
func (s *Store) Drained() bool {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	return s.drained
}

// OutOfService reports whether keepalived and BIRD must read HAProxy as
// unhealthy: the member is drained, or janusd is stopping.
func (s *Store) OutOfService() bool {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	return s.drained || s.stopping
}

// UndrainedAt is when Undrain last put the member back in service since
// the store was opened; zero: not since.
func (s *Store) UndrainedAt() time.Time {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	return s.undrainedAt
}

// Drain drains the member and keeps it so across reboots; it reports
// whether that changed anything. The drain holds this run even when it
// can't be kept: err says so.
func (s *Store) Drain() (changed bool, err error) {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	if err = s.keepDrainLocked(true); err != nil {
		err = errors.New("drained, but not across a reboot: " + err.Error())
	}
	return s.setDrainedLocked(true), err
}

// Undrain puts the member back in service, the drain kept on STATE
// gone first: a member that can't forget it stays drained (err).
func (s *Store) Undrain() (changed bool, err error) {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	if err := s.keepDrainLocked(false); err != nil {
		return false, errors.New("the drain kept on STATE can't go, the member stays drained: " + err.Error())
	}
	changed = s.setDrainedLocked(false)
	if changed {
		s.undrainedAt = time.Now()
	}
	return changed, nil
}

// Stopping has the member read as out of service for the rest of this
// run without draining it: a janusd about to restart, whose keepalived
// and BIRD stop - the next janusd starts as STATE says, and the member
// doesn't say it is drained meanwhile.
func (s *Store) Stopping() {
	s.drainMu.Lock()
	defer s.drainMu.Unlock()
	if !s.stopping {
		s.stopping = true
		s.wakeLocked()
	}
}

func (s *Store) setDrainedLocked(drained bool) bool {
	if s.drained == drained {
		return false
	}
	s.drained = drained
	s.wakeLocked()
	return true
}

// wakeLocked closes the channel DrainChanged gave.
func (s *Store) wakeLocked() {
	if s.drainCh != nil {
		close(s.drainCh)
		s.drainCh = nil
	}
}

// keepDrainLocked writes DrainedFile - when, for whoever reads it - or
// removes it, durably either way.
func (s *Store) keepDrainLocked(drained bool) error {
	if s.Dir == "" {
		return errors.New("no directory to keep it in")
	}
	if drained {
		if err := os.MkdirAll(s.Dir, 0o700); err != nil {
			return err
		}
		return writeDurable(s.Dir, DrainedFile, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	}
	err := os.Remove(filepath.Join(s.Dir, DrainedFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(s.Dir)
}

// keptDrained reports whether dir holds DrainedFile.
func keptDrained(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, DrainedFile))
	return err == nil
}

// DrainChanged is a channel closed at the next change of OutOfService:
// for the health gates to react at once (vrrp.KeepHealth,
// bgp.KeepGate).
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
