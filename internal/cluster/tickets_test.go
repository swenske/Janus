package cluster

import (
	"slices"
	"testing"
)

// A rotation keeps two of the ring's keys; a renewal none - what a node
// leaving a cluster, or a cluster a member left, takes.
func TestTicketKeysRotateAndRenew(t *testing.T) {
	s := newStore(t)
	if created, err := s.EnsureTicketKeys(); err != nil || !created {
		t.Fatalf("ensure: %v %v", created, err)
	}
	first, err := s.TicketKeysRing()
	if err != nil || len(first) != TicketKeys {
		t.Fatalf("ring: %v %v", first, err)
	}
	key, err := s.RotateTicketKeys()
	if err != nil {
		t.Fatal(err)
	}
	rotated, _ := s.TicketKeysRing()
	if !slices.Equal(rotated[:TicketKeys-1], first[1:]) || rotated[TicketKeys-1] != key {
		t.Errorf("a rotation: %v from %v", rotated, first)
	}
	renewed, err := s.RenewTicketKeys()
	if err != nil {
		t.Fatal(err)
	}
	ring, _ := s.TicketKeysRing()
	if !slices.Equal(ring, renewed) || len(ring) != TicketKeys {
		t.Fatalf("the ring %v isn't the keys renewed %v", ring, renewed)
	}
	for _, k := range ring {
		if slices.Contains(rotated, k) {
			t.Errorf("a renewal kept %s", k)
		}
	}
}
