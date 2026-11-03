package api

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/releasetrust"
	"github.com/swenske/Janus/internal/verity"
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

// TestCheckVerity: a bundle's squashfs and hash tree pass only when
// they're the ones its UKI's dm-verity table pins - each refusal names
// the file and says nothing was written (security audit 2026-09-30 #16).
func TestCheckVerity(t *testing.T) {
	squashfs := bytes.Repeat([]byte("janus-rootfs-block"), 4096*40/18+1)[:4096*40]
	table := &verity.Table{Version: 1, DataBlockSize: 4096, HashBlockSize: 4096, DataBlocks: 40,
		HashStartBlock: 1, Algorithm: "sha256", Salt: []byte{1, 2, 3}}
	tree, root, err := table.Build(squashfs)
	if err != nil {
		t.Fatal(err)
	}
	hashTree := append(make([]byte, 4096), tree...)
	uki := minimalPE(t, fmt.Sprintf(`console=ttyS0 dm-mod.create="vroot,,,ro,0 320 verity 1 PARTLABEL=BOOT-B-DATA PARTLABEL=BOOT-B-HASH 4096 4096 40 1 sha256 %x 010203" root=/dev/dm-0 ro`, root))

	got, err := checkVerity("uki-b.efi", uki, squashfs, hashTree)
	if err != nil || got != fmt.Sprintf("%x…", root[:6]) {
		t.Fatalf("its own bundle: %q, %v", got, err)
	}

	other := bytes.Clone(squashfs)
	other[5000] ^= 1
	for _, c := range []struct {
		name            string
		uki, data, tree []byte
		want            string
	}{
		{"another squashfs", uki, other, hashTree, "rootfs.squashfs: "},
		{"another tree", uki, squashfs, append(make([]byte, 4096), bytes.Repeat([]byte{7}, len(tree))...), "rootfs.verity: "},
		{"a UKI without a verity table", minimalPE(t, "console=ttyS0 ro"), squashfs, hashTree, "uki-b.efi: "},
		{"not a UKI", []byte("MZ"), squashfs, hashTree, "uki-b.efi: "},
	} {
		_, err := checkVerity("uki-b.efi", c.uki, c.data, c.tree)
		if status.Code(err) != codes.FailedPrecondition || !strings.HasPrefix(status.Convert(err).Message(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.want != "uki-b.efi: " && !strings.Contains(fmt.Sprint(err), "nothing was written") {
			t.Errorf("%s: %v doesn't say nothing was written", c.name, err)
		}
	}
}
