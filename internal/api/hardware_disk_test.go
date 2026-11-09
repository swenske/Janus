package api

import (
	"errors"
	"testing"

	"github.com/swenske/Janus/internal/bootslot"
)

// TestBootContextNVMe: the hardware check is given the disk itself -
// nvme0n1, not its partitions' prefix nvme0n1p (v2026.11.09's lab
// campaign: an NVMe machine refused with "can't tell whether this
// release's kernel drives this machine: disk nvme0n1p: lstat
// /sys/block/nvme0n1p").
func TestBootContextNVMe(t *testing.T) {
	prev := bootslot.ResolvePartLabel
	bootslot.ResolvePartLabel = func(label string) (string, error) {
		if dev, ok := map[string]string{"BOOT-B-DATA": "/dev/nvme0n1p4", "BOOT-B-HASH": "/dev/nvme0n1p5"}[label]; ok {
			return dev, nil
		}
		return "", errors.New("no such partition")
	}
	t.Cleanup(func() { bootslot.ResolvePartLabel = prev })
	bc, err := bootContextOf(`dm-mod.create="vroot,,,ro,0 131072 verity 1 PARTLABEL=BOOT-B-DATA PARTLABEL=BOOT-B-HASH 4096 4096 16384 1 sha256 aa bb" root=/dev/dm-0`)
	if err != nil {
		t.Fatal(err)
	}
	if bc.wholeDisk != "/dev/nvme0n1" || bc.disk != "/dev/nvme0n1p" || bc.currentSlot != "B" || bc.targetSlot != "A" || bc.espDevice != "/dev/nvme0n1p1" {
		t.Fatalf("%+v", bc)
	}
}
