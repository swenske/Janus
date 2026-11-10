package main

import (
	"log"
	"os"

	"github.com/swenske/Janus/internal/bootslot"
	"github.com/swenske/Janus/internal/hwsupport"
)

// logHardware says once a boot which drivers the boot disk and network
// interfaces are bound to: what an update's kernel must drive, Upgrade
// refuses one that doesn't (internal/hwsupport).
func logHardware() {
	disk := ""
	if cmdline, err := os.ReadFile("/proc/cmdline"); err == nil {
		if dev, ok := bootslot.DataDevice(string(cmdline)); ok {
			disk, _ = bootslot.WholeDisk(dev) // nvme0n1, not its partitions' nvme0n1p
		}
	}
	uses, err := hwsupport.InUse("/sys", disk)
	if err != nil {
		log.Printf("hardware: %v", err)
		return
	}
	log.Printf("hardware: %s", hwsupport.Summary(uses))
}
