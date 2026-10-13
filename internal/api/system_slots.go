package api

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"google.golang.org/protobuf/proto"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/bootslot"
	"github.com/swenske/Janus/internal/calver"
	"github.com/swenske/Janus/internal/schematic"
	"github.com/swenske/Janus/internal/slotimage"
)

// The other A/B slot's image (VersionResponse.inactive_slot): read off
// its data partition the first time it's asked for, then kept - only an
// update writes there, and it says what it wrote (slotWriting,
// slotWritten).
var otherSlot struct {
	sync.Mutex
	slot    string
	img     *janusv1alpha1.SlotImage
	writing bool
}

// readSlotImage reads a slot's data partition - a variable for tests.
var readSlotImage = slotimage.ReadPath

// InactiveSlotImage is the image the slot this node didn't boot holds;
// nil when this boot isn't from an A/B slot.
func InactiveSlotImage() *janusv1alpha1.SlotImage {
	cmdline, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return nil
	}
	dataDev, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return nil
	}
	active, ok := bootslot.ActiveSlot(dataDev)
	if !ok {
		return nil
	}
	disk, ok := bootslot.Disk(dataDev)
	if !ok {
		return nil
	}
	slot := bootslot.OtherSlot(active)

	otherSlot.Lock()
	defer otherSlot.Unlock()
	if otherSlot.slot == slot {
		if otherSlot.writing {
			return &janusv1alpha1.SlotImage{Slot: slot, Error: "being written by an update"}
		}
		if otherSlot.img != nil {
			return proto.Clone(otherSlot.img).(*janusv1alpha1.SlotImage)
		}
	}
	dev, ok := bootslot.SlotDataDevice(disk, slot)
	if !ok {
		return &janusv1alpha1.SlotImage{Slot: slot, Error: "no data partition for slot " + slot}
	}
	info, err := readSlotImage(dev)
	img := slotImage(slot, info, err)
	// Kept unless the partition couldn't be read: that's tried again.
	if err == nil || errors.Is(err, slotimage.ErrNoImageInfo) {
		otherSlot.slot, otherSlot.img, otherSlot.writing = slot, proto.Clone(img).(*janusv1alpha1.SlotImage), false
	}
	return img
}

// slotImage is what info - or err - says of slot.
func slotImage(slot string, info *schematic.ImageInfo, err error) *janusv1alpha1.SlotImage {
	if err != nil {
		return &janusv1alpha1.SlotImage{Slot: slot, Error: err.Error()}
	}
	return &janusv1alpha1.SlotImage{
		Slot: slot, Version: info.Version, SchematicId: info.SchematicID,
		Haproxy: imageComponent(info.HAProxy), Kernel: imageComponent(info.Kernel),
	}
}

// slotWriting says an update is writing slot.
func slotWriting(slot string) {
	otherSlot.Lock()
	defer otherSlot.Unlock()
	otherSlot.slot, otherSlot.img, otherSlot.writing = slot, nil, true
}

// slotWritten says what an update wrote to slot - info, as read from
// the bundle; err when it can't be told, or when the write failed (then
// the partition is read again next time).
func slotWritten(slot string, info *schematic.ImageInfo, err error, writeFailed bool) {
	otherSlot.Lock()
	defer otherSlot.Unlock()
	otherSlot.slot, otherSlot.writing = slot, false
	otherSlot.img = nil
	if !writeFailed {
		otherSlot.img = slotImage(slot, info, err)
	}
}

// describeImage says what a bundle's rootfs is, against the version this
// node runs.
func describeImage(info *schematic.ImageInfo, err error, running string) string {
	if err != nil {
		return fmt.Sprintf("rootfs.squashfs: %v", err)
	}
	what := "Janus " + info.Version
	if info.Version == "" {
		what = "an image that doesn't say its version"
	}
	what += fmt.Sprintf(" (schematic %.8s)", info.SchematicID)
	if cmp, known := calver.Compare(info.Version, running); known {
		switch cmp {
		case -1:
			what += ", older than the " + running + " this node runs"
		case 0:
			what += ", the release this node runs"
		}
	}
	return "rootfs.squashfs: " + what
}
