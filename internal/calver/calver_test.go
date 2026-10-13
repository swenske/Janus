package calver

import "testing"

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
