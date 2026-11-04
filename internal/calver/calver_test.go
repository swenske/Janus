package calver

import (
	"slices"
	"testing"
)

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		release, running string
		cmp              int
		known            bool
	}{
		{"v2026.10.13-4", "v2026.10.12", 1, true},
		{"v2026.10.12", "v2026.10.13-2", -1, true},
		{"v2026.10.13", "v2026.10.13-2", -1, true},
		{"v2026.10.13-10", "v2026.10.13-9", 1, true},
		{"v2026.10.13-2", "v2026.10.13-2", 0, true},
		{"v2026.10.13-4", "v2026.10.13-4-12-g550e69a-dirty", 0, true},
		{"v2026.10.13-4", "dev", 0, false},
		{"v2026.10.13-4-3-gabc", "v2026.10.12", 0, false},
	} {
		if cmp, known := Compare(c.release, c.running); cmp != c.cmp || known != c.known {
			t.Errorf("Compare(%s, %s) = %d, %v", c.release, c.running, cmp, known)
		}
	}
	if !IsRelease("v2026.10.13-4") || IsRelease("v2026.10.13-4-1-gabc") {
		t.Error("IsRelease")
	}
}

func TestSortNewestFirst(t *testing.T) {
	// GitHub's order: by the date of each tag's commit - the maintenance
	// release v2026.11.04-3, tagged last, comes first.
	tags := []string{"v2026.11.04-3", "dev", "v2026.11.10", "v2026.11.10-2", "v2026.11.04", "v2026.10.31-4", "v2026.11.04-2"}
	SortNewestFirst(tags, func(s string) string { return s })
	want := []string{"v2026.11.10-2", "v2026.11.10", "v2026.11.04-3", "v2026.11.04-2", "v2026.11.04", "v2026.10.31-4", "dev"}
	if !slices.Equal(tags, want) {
		t.Errorf("SortNewestFirst = %v, want %v", tags, want)
	}
}

func TestSameDay(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"v2026.11.04", "v2026.11.04-3", true},
		{"v2026.11.04-2", "v2026.11.04-3", true},
		{"v2026.11.04", "v2026.11.05", false},
		{"v2026.11.04", "v2026.11.04-3-2-gabc", false},
		{"dev", "dev", false},
	} {
		if got := SameDay(c.a, c.b); got != c.same {
			t.Errorf("SameDay(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
