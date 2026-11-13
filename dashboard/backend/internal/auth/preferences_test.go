package auth

import (
	"slices"
	"testing"
)

// An account's preferences: the defaults until it chooses, kept across a
// restart, an interval not offered refused, and a copy never the store's.
func TestPreferences(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Setup("root", "long-enough", ""); err != nil {
		t.Fatal(err)
	}
	u, _ := s.User("root")
	e := u.Preferences.Effective()
	if e.NodesRefreshSeconds != 15 || e.HypervisorsRefreshSeconds != 15 || !*e.NodeStats || !*e.NodeCharts {
		t.Fatalf("defaults: %+v", e)
	}
	off := false
	if _, err := s.SetPreferences("root", Preferences{NodesRefreshSeconds: 30, NodeStats: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPreferences("root", Preferences{NodesRefreshSeconds: 1}); err == nil {
		t.Error("a 1 s refresh was accepted")
	}
	if _, err := s.SetPreferences("root", Preferences{HypervisorsRefreshSeconds: 7}); err == nil {
		t.Error("a 7 s hypervisors refresh was accepted")
	}
	if _, err := s.SetPreferences("nobody", Preferences{}); err != ErrNoUser {
		t.Errorf("an unknown account: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, _ = s2.User("root")
	e = u.Preferences.Effective()
	if e.NodesRefreshSeconds != 30 || e.HypervisorsRefreshSeconds != 15 || *e.NodeStats || !*e.NodeCharts {
		t.Fatalf("after a restart: %+v", e)
	}
	*u.Preferences.NodeStats = true // a copy
	u, _ = s2.User("root")
	if *u.Preferences.NodeStats {
		t.Error("changing a view changed the store")
	}
}

// The Read-only switch's preferences (#527): pages open read-only and go
// back to it after 15 minutes by default; "never" is 0, kept as chosen;
// anything else refused.
func TestReadOnlyPreferences(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Setup("root", "long-enough", ""); err != nil {
		t.Fatal(err)
	}
	u, _ := s.User("root")
	if e := u.Preferences.Effective(); e.PagesOpen != "read_only" || *e.ReadOnlyAfterMinutes != 15 {
		t.Fatalf("defaults: %q %d", e.PagesOpen, *e.ReadOnlyAfterMinutes)
	}
	never := 0
	if _, err := s.SetPreferences("root", Preferences{PagesOpen: "read_write", ReadOnlyAfterMinutes: &never}); err != nil {
		t.Fatal(err)
	}
	u, _ = s.User("root")
	if e := u.Preferences.Effective(); e.PagesOpen != "read_write" || *e.ReadOnlyAfterMinutes != 0 {
		t.Errorf("chosen: %q %d", e.PagesOpen, *e.ReadOnlyAfterMinutes)
	}
	*u.Preferences.ReadOnlyAfterMinutes = 30 // a copy
	if u, _ = s.User("root"); *u.Preferences.ReadOnlyAfterMinutes != 0 {
		t.Error("changing a view changed the store")
	}
	seven := 7
	for _, p := range []Preferences{{PagesOpen: "rw"}, {ReadOnlyAfterMinutes: &seven}} {
		if _, err := s.SetPreferences("root", p); err == nil {
			t.Errorf("%+v was accepted", p)
		}
	}
	if !slices.Equal(Choices(Settings{})["read_only_after"], ReadOnlyAfterChoices) {
		t.Errorf("choices: %v", Choices(Settings{}))
	}
}
