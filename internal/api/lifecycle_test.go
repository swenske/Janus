package api

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/releasetrust"
)

// TestCheckUKISignature: an untrusted UKI is refused unless the caller
// explicitly opts out; a trusted one passes. (Verification itself is
// internal/releasetrust's own tests.)
func TestCheckUKISignature(t *testing.T) {
	defer func(v func([]byte) error) { verifyUKI = v }(verifyUKI)
	src := &janusv1alpha1.ImageSource{Reference: "https://example.invalid/bundle"}

	// The real check, on something that isn't a signed UKI at all.
	if _, err := checkUKISignature(src, "uki-b.efi", []byte("not a UKI")); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "insecure_skip_signature_check") {
		t.Errorf("unsigned bundle: %v, want FailedPrecondition mentioning the opt-out", err)
	}

	verifyUKI = func([]byte) error { return releasetrust.ErrUntrusted }
	if _, err := checkUKISignature(src, "uki-b.efi", nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("untrusted: %v, want FailedPrecondition", err)
	}

	skip := &janusv1alpha1.ImageSource{Reference: src.Reference, InsecureSkipSignatureCheck: true}
	if msg, err := checkUKISignature(skip, "uki-b.efi", nil); err != nil || !strings.Contains(msg, "skipped") {
		t.Errorf("opted out: %q, %v", msg, err)
	}

	verifyUKI = func([]byte) error { return nil }
	if msg, err := checkUKISignature(src, "uki-b.efi", nil); err != nil || msg != "signature verified" {
		t.Errorf("trusted: %q, %v", msg, err)
	}
}

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
