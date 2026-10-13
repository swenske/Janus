package slotimage

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// testdata: an image's root filesystem with its image.json, as zstd
// (releases since 2026-10-07) and xz (before) build it, and one from
// before image.json - made with mksquashfs.
func TestRead(t *testing.T) {
	for _, name := range []string{"testdata/zstd.squashfs", "testdata/xz.squashfs"} {
		info, err := ReadPath(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Version != "v2026.10.13-2" || info.SchematicID != "4e5a3b62bd93cbe8d731ca9e51276cf283f0e2bf7230eb1d23a5bfa995c3be40" ||
			info.HAProxy.Version != "3.4.6" || info.Kernel.Variant != "stable" || string(info.Schematic) != `{"customization":{"extensions":["keepalived"]}}` {
			t.Errorf("%s: %+v", name, info)
		}
	}

	// From memory, as Upgrade has a bundle's rootfs.squashfs.
	data, err := os.ReadFile("testdata/zstd.squashfs")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := Read(bytes.NewReader(data), int64(len(data))); err != nil || info.Version != "v2026.10.13-2" {
		t.Errorf("from memory: %+v, %v", info, err)
	}

	if _, err := ReadPath("testdata/no-image-info.squashfs"); !errors.Is(err, ErrNoImageInfo) {
		t.Errorf("an image without image.json: %v", err)
	}
	// Not a squashfs: an error, not a version.
	if info, err := Read(bytes.NewReader(make([]byte, 8192)), 8192); err == nil {
		t.Errorf("zeros read as %+v", info)
	}
}
