package api

import "testing"

func TestReleaseFileMaxBytes(t *testing.T) {
	cases := []struct {
		filename string
		wantZero bool
	}{
		{"rootfs.squashfs", false},
		{"rootfs.verity", false},
		{"uki-a.efi", false},
		{"uki-b.efi", false},
		{"", true},
		{"rootfs.squashfs.sha256", true},
		{"../../etc/passwd", true},
		{"uki-c.efi", true},
		{"UKI-A.EFI", true}, // exact match only, no case-insensitivity
	}
	for _, c := range cases {
		got := releaseFileMaxBytes(c.filename)
		if c.wantZero && got != 0 {
			t.Errorf("releaseFileMaxBytes(%q) = %d, want 0 (rejected)", c.filename, got)
		}
		if !c.wantZero && got <= 0 {
			t.Errorf("releaseFileMaxBytes(%q) = %d, want a positive byte cap", c.filename, got)
		}
	}
}
