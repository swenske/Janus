package kexec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitRefusesWhatIsntAUKI(t *testing.T) {
	if _, err := Split([]byte("MZ not really")); err == nil {
		t.Fatal("garbage: no error")
	}
	// A real PE without a .linux section: the test binary itself on
	// Windows would do; on Linux, any ELF isn't a PE either.
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Skip(err)
	}
	if _, err := Split(self); err == nil {
		t.Fatal("an ELF: no error")
	}
}

// A UKI built by this tree (make uki-image or disk-image): its kernel
// starts with the bzImage boot sector, its command line is the one
// image/uki/assemble.sh wrote.
func TestSplitABuiltUKI(t *testing.T) {
	var uki []byte
	for _, p := range []string{"../../build/release/uki-a.efi", "../../build/rootfs/janus.efi"} {
		if data, err := os.ReadFile(filepath.Clean(p)); err == nil {
			uki = data
			break
		}
	}
	if uki == nil {
		t.Skip("no built UKI")
	}
	parts, err := Split(uki)
	if err != nil {
		t.Fatal(err)
	}
	// bzImage: "HdrS" at 0x202 of the boot sector.
	if len(parts.Kernel) < 0x206 || string(parts.Kernel[0x202:0x206]) != "HdrS" {
		t.Fatalf("the .linux section isn't a bzImage (%d bytes)", len(parts.Kernel))
	}
	for _, want := range []string{"dm-mod.create=", "root=/dev/dm-0", "rootfstype=squashfs"} {
		if !contains(parts.Cmdline, want) {
			t.Fatalf("cmdline %q lacks %q", parts.Cmdline, want)
		}
	}
	if len(parts.Initrd) != 0 {
		t.Fatalf("a Janus UKI has no initrd, got %d bytes", len(parts.Initrd))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// The empty initramfs: a newc archive of one trailer entry, padded.
func TestEmptyInitramfs(t *testing.T) {
	a := emptyInitramfs()
	if len(a) != 512 || string(a[:6]) != "070701" || !contains(string(a), "TRAILER!!!\x00") {
		t.Fatalf("empty initramfs: %d bytes, %q", len(a), a[:124])
	}
	// namesize is the 12th field: 11 for "TRAILER!!!" and its NUL.
	if string(a[6+11*8:6+12*8]) != "0000000b" {
		t.Fatalf("namesize field: %q", a[6+11*8:6+12*8])
	}
	for _, b := range a[124:] {
		if b != 0 {
			t.Fatal("padding isn't zero")
		}
	}
}
