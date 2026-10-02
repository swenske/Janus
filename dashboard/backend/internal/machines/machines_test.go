package machines

import (
	"strings"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := &Machine{Spec: Spec{Name: "lb1", HypervisorID: "h1", VCPUs: 2, MemoryMiB: 1024}, Phase: PhasePending}
	if err := s.Add(m); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(m.ID, func(m *Machine) error {
		m.Phase = PhaseImage
		m.Log("downloading %s", "janus-kvm.qcow2")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get(m.ID)
	if !ok || got.Phase != PhaseImage || len(got.Events) != 1 || !strings.Contains(got.Events[0].Message, "janus-kvm") {
		t.Fatalf("reloaded %+v", got)
	}
	// Copies: changing one doesn't change the store.
	got.Spec.Name = "changed"
	if again, _ := s2.Get(m.ID); again.Spec.Name != "lb1" {
		t.Error("Get returned shared state")
	}
	if err := s2.Remove(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Get(m.ID); ok {
		t.Error("still there after Remove")
	}
}

func TestEventsAreCapped(t *testing.T) {
	m := &Machine{}
	for i := 0; i < maxEvents+10; i++ {
		m.Log("event %d", i)
	}
	if len(m.Events) != maxEvents || m.Events[0].Message != "event 10" {
		t.Errorf("%d events, first %q", len(m.Events), m.Events[0].Message)
	}
}

func TestClaimToken(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := &Machine{Spec: Spec{Name: "a"}}, &Machine{Spec: Spec{Name: "b"}}
	for _, m := range []*Machine{a, b} {
		if err := s.Add(m); err != nil {
			t.Fatal(err)
		}
	}
	tokA, err := s.NewToken(a.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tokB, err := s.NewToken(b.ID, -time.Second) // already expired
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := s.Get(a.ID); stored.TokenHash == tokA || stored.TokenHash == "" {
		t.Error("the token itself is stored, or nothing is")
	}

	if _, ok := s.ClaimToken("not-a-token"); ok {
		t.Error("an unknown token was claimed")
	}
	if _, ok := s.ClaimToken(""); ok {
		t.Error("an empty token was claimed")
	}
	got, ok := s.ClaimToken(tokA)
	if !ok || got.ID != a.ID {
		t.Fatalf("ClaimToken(a) = %v, %v", got, ok)
	}
	if _, ok := s.ClaimToken(tokA); ok {
		t.Error("a token was claimed twice")
	}
	if _, ok := s.ClaimToken(tokB); ok {
		t.Error("an expired token was claimed")
	}
	if stored, _ := s.Get(b.ID); stored.TokenHash != "" {
		t.Error("an expired token stays usable")
	}
}
