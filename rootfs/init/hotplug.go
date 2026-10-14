package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/hotplug"
)

// cpuPollEvery is how often onlineHotAddedCPUs looks for a new vCPU: a
// second at most between the hypervisor's plug and the CPU in use.
const cpuPollEvery = time.Second

// onlineHotAddedCPUs brings into use each vCPU a hypervisor plugs into
// the running machine (the Controller's auto-scaling): the kernel leaves
// one offline, and what onlines it elsewhere - a udev rule - doesn't
// exist on a node. Polled: a kernel uevent socket for an event in a
// while isn't worth it. janusd (internal/hotplug's Watcher) tells what
// changed and reloads HAProxy, which runs a thread per CPU. Never
// returns.
func onlineHotAddedCPUs() {
	reported := map[int]bool{} // CPUs whose failure was printed
	for {
		onlined, failed := hotplug.OnlineCPUs(hotplug.CPUDir)
		for _, c := range onlined {
			fmt.Printf("init: cpu%d online (plugged in)\n", c)
			delete(reported, c)
		}
		for c, err := range failed {
			if c < 0 {
				// No CPU list: nothing to follow on this kernel.
				fmt.Printf("init: hot-plugged CPUs won't be brought online: %v\n", err)
				return
			}
			if !reported[c] {
				fmt.Printf("init: cpu%d: %v\n", c, err)
				reported[c] = true
			}
		}
		time.Sleep(cpuPollEvery)
	}
}

// memoryPolicyPath is the kernel's choice of zone for memory onlined
// without one - what hot-plugged memory gets (MHP_DEFAULT_ONLINE_TYPE_
// ONLINE_AUTO).
const memoryPolicyPath = "/sys/module/memory_hotplug/parameters/online_policy"

// movableHotplugMemory has the kernel put plugged-in memory where it can
// be taken back - ZONE_MOVABLE, as far as the kernel's own memory
// (auto_movable_ratio: 301 % of it by default) allows: the Controller's
// auto-scaling unplugs it again when the load is gone, and the default
// policy (contig-zones) mostly can't. The kernel's own allocations -
// socket buffers among them - can't use ZONE_MOVABLE: past that ratio,
// memory goes where they can. A kernel without memory hot-plug has no
// such file: nothing to do.
func movableHotplugMemory() {
	cur, err := os.ReadFile(memoryPolicyPath)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(cur)) == "auto-movable" {
		return
	}
	f, err := os.OpenFile(memoryPolicyPath, os.O_WRONLY, 0)
	if err == nil {
		_, err = f.WriteString("auto-movable")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		fmt.Printf("init: memory hot-plug policy: %v\n", err)
		return
	}
	fmt.Println("init: plugged-in memory goes to ZONE_MOVABLE (auto-movable)")
}
