package cluster

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/pki"
)

// reopen is the store as the next janusd finds it: after a reboot, a
// crash, a restart.
func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	n, err := Open(s.Dir, s.Hostname)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// join makes s member lb-1 of the test cluster.
func join(t *testing.T, s *Store) {
	t.Helper()
	ca, err := pki.NewCA("Janus cluster edge")
	if err != nil {
		t.Fatal(err)
	}
	csr, err := s.NewCSR()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Set(SetRequest{Membership: membership("lb-1"), CAPEM: ca.CertPEM, MemberPEM: signMember(t, ca, csr, testID, "lb-1")}); err != nil {
		t.Fatal(err)
	}
}

// A drain outlives a reboot (#356): kept on STATE, read back by Open -
// the next janusd's health gates see it from their first check - and
// gone with an undrain or a leave. A drain for this run only (a
// restarting janusd's) isn't kept.
func TestDrainKeptAcrossReboots(t *testing.T) {
	s := newStore(t)
	join(t, s)
	marker := filepath.Join(s.Dir, DrainedFile)

	ch := s.DrainChanged()
	changed, err := s.Drain()
	if err != nil || !changed || !s.Drained() {
		t.Fatalf("drain: %v %v %v", changed, err, s.Drained())
	}
	select {
	case <-ch:
	default:
		t.Fatal("the gates weren't woken by the drain")
	}
	if st, err := os.Stat(marker); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the drain isn't kept: %v", err)
	}
	if changed, err := s.Drain(); err != nil || changed {
		t.Fatalf("drained again: %v %v", changed, err)
	}

	// The reboot: drained from the start, still a member.
	s = reopen(t, s)
	if !s.Drained() || !s.OutOfService() || !s.IsMember() {
		t.Fatalf("after the reboot: drained %v, member %v", s.Drained(), s.IsMember())
	}
	if !s.UndrainedAt().IsZero() {
		t.Fatal("undrained since the reboot")
	}

	// Undrained: the marker goes, the time is said; the next boot is in
	// service.
	before := time.Now()
	changed, err = s.Undrain()
	if err != nil || !changed || s.Drained() {
		t.Fatalf("undrain: %v %v %v", changed, err, s.Drained())
	}
	if at := s.UndrainedAt(); at.Before(before) {
		t.Fatalf("undrained at %v", at)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the marker stayed: %v", err)
	}
	if reopen(t, s).Drained() {
		t.Fatal("drained again after an undrain and a reboot")
	}

	// A janusd about to restart reads as out of service to keepalived
	// and BIRD, without the member saying it is drained - nor being so
	// at the next start.
	ch = s.DrainChanged()
	s.Stopping()
	select {
	case <-ch:
	default:
		t.Fatal("the gates weren't woken by the stop")
	}
	if !s.OutOfService() || s.Drained() {
		t.Fatalf("stopping: out of service %v, drained %v", s.OutOfService(), s.Drained())
	}
	if n := reopen(t, s); n.Drained() || n.OutOfService() {
		t.Fatal("a stop was kept as a drain")
	}
	s = reopen(t, s)

	// Leaving clears it: drained until undrained or rebooted.
	if _, err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	if err := s.Leave(); err != nil {
		t.Fatal(err)
	}
	if !s.Drained() || !s.OutOfService() {
		t.Fatal("a node leaving drained was put back in service at once")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the marker stayed after the leave: %v", err)
	}
	if n := reopen(t, s); n.Drained() || n.IsMember() {
		t.Fatalf("after the leave and a reboot: drained %v, member %v", n.Drained(), n.IsMember())
	}
}

// A node in no cluster drained stays so too: Open reads the drain
// whatever the membership, even one it can't read.
func TestDrainKeptWithoutMembership(t *testing.T) {
	s := newStore(t)
	if _, err := s.Drain(); err != nil {
		t.Fatal(err)
	}
	if !reopen(t, s).Drained() {
		t.Fatal("a standalone node's drain wasn't kept")
	}
	if err := os.WriteFile(filepath.Join(s.Dir, MembershipFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := Open(s.Dir, s.Hostname)
	if err == nil || !n.Drained() {
		t.Fatalf("a broken membership: %v, drained %v", err, n.Drained())
	}
}

// A drain that can't be kept still holds this run, and says so; an
// undrain that can't clear it leaves the member drained.
func TestDrainNotKept(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A directory where the marker goes: neither written nor removed.
	if err := os.MkdirAll(filepath.Join(s.Dir, "."+DrainedFile+".tmp", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	changed, err := s.Drain()
	if err == nil || !changed || !s.Drained() {
		t.Fatalf("drain not kept: %v %v %v", changed, err, s.Drained())
	}
	if err := os.RemoveAll(filepath.Join(s.Dir, "."+DrainedFile+".tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.Dir, DrainedFile, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.Undrain(); err == nil || changed || !s.Drained() {
		t.Fatalf("undrain without clearing: %v %v %v", changed, err, s.Drained())
	}
}
