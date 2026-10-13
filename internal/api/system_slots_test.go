package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/schematic"
	"github.com/swenske/Janus/internal/slotimage"
)

// What Upgrade says of a bundle before writing it: its release, and how
// it stands against the node's.
func TestDescribeImage(t *testing.T) {
	img := &schematic.ImageInfo{Version: "v2026.10.12", SchematicID: "e61bbaad0a60f4688f8f1cfc1dacbb7efcf67de8"}
	for running, want := range map[string]string{
		"v2026.10.13-2":            "rootfs.squashfs: Janus v2026.10.12 (schematic e61bbaad), older than the v2026.10.13-2 this node runs",
		"v2026.10.12":              "rootfs.squashfs: Janus v2026.10.12 (schematic e61bbaad), the release this node runs",
		"v2026.10.11-2":            "rootfs.squashfs: Janus v2026.10.12 (schematic e61bbaad)",
		"v2026.10.12-3-gabc-dirty": "rootfs.squashfs: Janus v2026.10.12 (schematic e61bbaad), the release this node runs",
		"dev":                      "rootfs.squashfs: Janus v2026.10.12 (schematic e61bbaad)",
	} {
		if got := describeImage(img, nil, running); got != want {
			t.Errorf("running %s: %q, want %q", running, got, want)
		}
	}
	if got := describeImage(nil, slotimage.ErrNoImageInfo, "v2026.10.13-2"); !strings.Contains(got, "doesn't say what it is") {
		t.Errorf("no image.json: %q", got)
	}
	if got := describeImage(&schematic.ImageInfo{SchematicID: "a055fbb6aaaa"}, nil, "v2026.10.13-2"); got != "rootfs.squashfs: an image that doesn't say its version (schematic a055fbb6)" {
		t.Errorf("a local build: %q", got)
	}
}

// What an update wrote is what the other slot holds - until the next
// read, if the write failed.
func TestSlotWritten(t *testing.T) {
	t.Cleanup(func() { slotWritten("", nil, nil, true) })
	slotWriting("B")
	if !otherSlot.writing || otherSlot.slot != "B" {
		t.Fatal("not marked as being written")
	}
	img := &schematic.ImageInfo{Version: "v2026.10.13-4", SchematicID: "a055fbb6", HAProxy: schematic.ImageComponent{Variant: "3.4", Version: "3.4.6"}}
	slotWritten("B", img, nil, false)
	if got := otherSlot.img; otherSlot.writing || got.GetSlot() != "B" || got.GetVersion() != "v2026.10.13-4" || got.GetHaproxy().GetVersion() != "3.4.6" {
		t.Errorf("after the write: %v, writing %v", got, otherSlot.writing)
	}
	slotWritten("B", nil, errors.New("disk full"), true)
	if otherSlot.img != nil {
		t.Errorf("a failed write kept %v: the partition must be read again", otherSlot.img)
	}
	if got := slotImage("A", nil, slotimage.ErrNoImageInfo); got.GetVersion() != "" || !strings.Contains(got.GetError(), "v2026.10.06-3") {
		t.Errorf("an image without image.json: %v", got)
	}
}
