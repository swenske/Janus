package proxmox

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// A scalable machine on Proxmox VE (docs/hypervisors.md, auto-scaling):
//
//   - vCPUs: cores is the maximum, vcpus how many are plugged - "qm set
//     --vcpus" plugs and unplugs them while it runs;
//   - memory: Proxmox VE 9.2 has no virtio-mem: above a static 1024 MiB,
//     memory is DIMMs of 512 MiB (1 GiB past 32 of them), plugged and
//     unplugged one by one when the memory setting changes (qemu-server's
//     Memory.pm). There's no maximum in the configuration: the
//     Controller keeps its own.
//
// Both need the hotplug option and NUMA. Proven on PVE 9.2.21
// (2026-10-14).

const (
	staticMemoryMiB = 1024 // what memory hot-plug keeps out of DIMMs
	dimmMiB         = 512
	smallDIMMs      = 32 // then 1024 MiB DIMMs
	defaultHotplug  = "network,disk,usb"
)

// hotplugMemoryAligned reports whether Proxmox can reach memoryMiB with
// its DIMMs.
func hotplugMemoryAligned(memoryMiB int) bool {
	if memoryMiB < staticMemoryMiB {
		return false
	}
	if big := staticMemoryMiB + smallDIMMs*dimmMiB; memoryMiB > big {
		return (memoryMiB-big)%(2*dimmMiB) == 0
	}
	return (memoryMiB-staticMemoryMiB)%dimmMiB == 0
}

func checkHardware(hw hypervisor.Hardware) error {
	if hw.VCPUs < 1 || hw.MemoryMiB < 1 {
		return errors.New("a machine needs at least one vCPU and some memory")
	}
	if hw.ScalableMemory() {
		for _, m := range []int{hw.MemoryMiB, hw.MaxMemoryMiB} {
			if !hotplugMemoryAligned(m) {
				return fmt.Errorf("%d MiB: with memory hot-plug, Proxmox VE has %d MiB plus %d MiB modules (%d MiB past %d of them): 1024, 1536, 2048 and so on",
					m, staticMemoryMiB, dimmMiB, 2*dimmMiB, smallDIMMs)
			}
		}
	}
	return nil
}

// hardwareParams is the configuration of hw: fixed, or scalable - and
// the keys a fixed machine no longer needs (cfg: its configuration),
// to delete.
func hardwareParams(cfg map[string]any, hw hypervisor.Hardware) (url.Values, []string) {
	p := url.Values{"sockets": {"1"}, "memory": {strconv.Itoa(hw.MemoryMiB)}}
	var deletes []string
	unset := func(key string) {
		if _, ok := cfg[key]; ok {
			deletes = append(deletes, key)
		}
	}
	hotplug := defaultHotplug
	if hw.ScalableCPUs() {
		p.Set("cores", strconv.Itoa(hw.MaxVCPUs))
		p.Set("vcpus", strconv.Itoa(hw.VCPUs))
		hotplug += ",cpu"
	} else {
		p.Set("cores", strconv.Itoa(hw.VCPUs))
		unset("vcpus")
	}
	if hw.ScalableMemory() {
		hotplug += ",memory"
	}
	if hw.Scalable() {
		p.Set("numa", "1")
		p.Set("hotplug", hotplug)
	} else {
		unset("numa")
		unset("hotplug")
	}
	return p, deletes
}

// statusOf reads a machine's size from its configuration.
func statusOf(c *vmConfig) (vcpus, maxVCPUs, memMiB, maxMemMiB, stepMiB int) {
	maxVCPUs = max(c.Cores, 1) * max(c.Sockets, 1)
	vcpus = maxVCPUs
	if c.Vcpus > 0 && c.Vcpus < maxVCPUs {
		vcpus = c.Vcpus
	}
	memMiB = c.memoryMiB()
	maxMemMiB = memMiB
	if c.hotplugs("memory") && c.Numa == 1 {
		maxMemMiB, stepMiB = 0, dimmMiB
	}
	return
}

// hotplugs reports whether the hotplug option has feature.
func (c *vmConfig) hotplugs(feature string) bool {
	h := c.Hotplug
	if h == "" || h == "1" {
		h = defaultHotplug
	}
	for _, f := range strings.Split(h, ",") {
		if strings.TrimSpace(f) == feature {
			return true
		}
	}
	return false
}
