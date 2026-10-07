package main

import (
	"testing"
	"time"
)

// TestLocalTimes: a node writes UTC timestamps into a history entry's
// detail; the command prints them in this machine's zone, like every
// other time it prints, and leaves the rest alone.
func TestLocalTimes(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("CEST", 2*3600)
	t.Cleanup(func() { time.Local = saved })
	got := localTimes("reverts at 2026-10-07T14:01:02Z unless confirmed")
	if want := "reverts at 2026-10-07T16:01:02+02:00 unless confirmed"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := localTimes("confirmed over a new connection"); got != "confirmed over a new connection" {
		t.Fatalf("changed a detail without a timestamp: %q", got)
	}
}
