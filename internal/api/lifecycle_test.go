package api

import (
	"bytes"
	"encoding/binary"
	"os"
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

// minimalPE builds a PE image with a single .cmdline section - enough for
// debug/pe, standing in for a UKI.
func minimalPE(t *testing.T, cmdline string) []byte {
	t.Helper()
	var b bytes.Buffer
	dos := make([]byte, 0x40)
	copy(dos, "MZ")
	binary.LittleEndian.PutUint32(dos[0x3c:], 0x40)
	b.Write(dos)
	b.WriteString("PE\x00\x00")
	data := []byte(cmdline + "\x00\x00\x00")
	const dataOff = 0x40 + 4 + 20 + 40
	coff := make([]byte, 20)
	binary.LittleEndian.PutUint16(coff[0:], 0x8664) // AMD64
	binary.LittleEndian.PutUint16(coff[2:], 1)      // one section
	b.Write(coff)
	sec := make([]byte, 40)
	copy(sec, ".cmdline")
	binary.LittleEndian.PutUint32(sec[8:], uint32(len(data)))  // VirtualSize
	binary.LittleEndian.PutUint32(sec[16:], uint32(len(data))) // SizeOfRawData
	binary.LittleEndian.PutUint32(sec[20:], dataOff)           // PointerToRawData
	b.Write(sec)
	b.Write(data)
	return b.Bytes()
}

func TestCheckSchematic(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	withA := minimalPE(t, "console=ttyS0 ro janus.schematic="+a)
	noParam := minimalPE(t, "console=ttyS0 ro")

	if got, err := ukiCmdline(withA); err != nil || got != "console=ttyS0 ro janus.schematic="+a {
		t.Fatalf("ukiCmdline = %q, %v", got, err)
	}
	if _, err := checkSchematic(&janusv1alpha1.ImageSource{}, "ro janus.schematic="+a, withA); err != nil {
		t.Errorf("same schematic refused: %v", err)
	}
	if _, err := checkSchematic(&janusv1alpha1.ImageSource{}, "ro", noParam); err != nil {
		t.Errorf("default to default refused: %v", err)
	}
	_, err := checkSchematic(&janusv1alpha1.ImageSource{}, "ro janus.schematic="+b, withA)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), b) {
		t.Errorf("a different schematic: %v", err)
	}
	// A node with extensions offered a default-schematic update: refused.
	if _, err := checkSchematic(&janusv1alpha1.ImageSource{}, "ro janus.schematic="+a, noParam); err == nil {
		t.Error("an update dropping the node's extensions was accepted")
	}
	if _, err := checkSchematic(&janusv1alpha1.ImageSource{AllowSchematicChange: true}, "ro janus.schematic="+b, withA); err != nil {
		t.Errorf("an allowed change refused: %v", err)
	}
	if _, err := checkSchematic(&janusv1alpha1.ImageSource{}, "ro", []byte("not a PE")); err == nil {
		t.Error("garbage accepted as a UKI")
	}
	if _, err := checkSchematic(&janusv1alpha1.ImageSource{}, "ro", minimalPE(t, "ro janus.schematic=zz")); err == nil {
		t.Error("a malformed schematic ID accepted")
	}
}

// TestUKICmdlineOfARealUKI reads the command line of a UKI built by
// make uki-image, when there is one.
func TestUKICmdlineOfARealUKI(t *testing.T) {
	uki, err := os.ReadFile("../../build/rootfs/janus.efi")
	if err != nil {
		t.Skip("no build/rootfs/janus.efi (make uki-image)")
	}
	cmdline, err := ukiCmdline(uki)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmdline, "dm-mod.create=") {
		t.Errorf("cmdline %q", cmdline)
	}
}
