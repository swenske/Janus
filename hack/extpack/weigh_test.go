package main

import (
	"archive/tar"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/diskimage"
	"github.com/swenske/Janus/internal/schematic"
)

// TestSquashFlagsAreTheImages: a part is weighed squashed the way the
// image is.
func TestSquashFlagsAreTheImages(t *testing.T) {
	script, err := os.ReadFile("../../rootfs/layer-and-squash.sh")
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(squashFlags, " "); !strings.Contains(string(script), want) {
		t.Errorf("rootfs/layer-and-squash.sh doesn't squash with %q", want)
	}
}

// TestSlotBytesAreTheDiskScripts: the catalog's slot size is the one the
// images are made with.
func TestSlotBytesAreTheDiskScripts(t *testing.T) {
	for file, want := range map[string]string{
		"../../image/disk/assemble.sh":     fmt.Sprintf(`DATA_MB="${JANUS_SLOT_MB:-%d}"`, diskimage.DataMB),
		"../../image/rpi-uefi/assemble.sh": fmt.Sprintf("DATA_MB=%d\n", diskimage.DataMB),
	} {
		script, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(script), want) {
			t.Errorf("%s doesn't say %q", file, strings.TrimSpace(want))
		}
	}
}

// writeTar writes a tar holding one file of n random bytes.
func writeTar(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	data := make([]byte, n)
	_, _ = rand.Read(data)
	if err := tw.WriteHeader(&tar.Header{Name: "usr/lib/part.bin", Mode: 0o644, Size: int64(n), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestWeighCatalog: the base, each HAProxy branch and each extension
// built for amd64 get what they take squashed - random bytes, about
// their size -, the rest nothing; a missing input fails.
func TestWeighCatalog(t *testing.T) {
	if _, err := lookTool("mksquashfs"); err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	writeTar(t, filepath.Join(dir, "rootfs-base-amd64.tar"), 3<<20)
	writeTar(t, filepath.Join(dir, "haproxy-3.4-amd64.tar"), 1<<20)
	writeTar(t, filepath.Join(dir, "extension-big-amd64.tar"), 2<<20)
	c := &schematic.Catalog{
		HAProxy: []schematic.Variant{{Name: "3.4", Default: true, Arches: []string{"amd64"}}},
		Extensions: []schematic.CatalogEntry{
			{Name: "big", Arches: []string{"amd64", "arm64"}},
			{Name: "pi-only", Arches: []string{"arm64"}},
		},
	}
	if err := weighCatalog(c, dir); err != nil {
		t.Fatal(err)
	}
	near := func(what string, got, want uint64) {
		if got < want || got > want+want/20+64<<10 {
			t.Errorf("%s: %d bytes, want about %d", what, got, want)
		}
	}
	near("base", c.BaseBytes, 3<<20)
	near("HAProxy 3.4", c.HAProxy[0].Bytes, 1<<20)
	near("big", c.Extensions[0].Bytes, 2<<20)
	if c.Extensions[1].Bytes != 0 {
		t.Errorf("an arm64-only extension weighed %d", c.Extensions[1].Bytes)
	}
	if c.SlotBytes != diskimage.DataMB<<20 {
		t.Errorf("slots %d", c.SlotBytes)
	}

	c.Extensions = append(c.Extensions, schematic.CatalogEntry{Name: "missing", Arches: []string{"amd64"}})
	if err := weighCatalog(c, dir); err == nil {
		t.Error("a missing extension tar weighed")
	}
}
