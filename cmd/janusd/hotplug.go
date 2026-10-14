package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/haproxy"
	"github.com/swenske/Janus/internal/hotplug"
)

// followHotplug tells, in the event log, what vCPUs and memory a
// hypervisor plugged in or took out (the Controller's auto-scaling -
// rootfs/init onlined the CPUs, the kernel the memory), and reloads
// HAProxy when the number of CPUs changed: it runs a thread per CPU it
// may use, counted when it starts - unless its configuration fixes them.
// Runs until ctx ends.
func followHotplug(ctx context.Context, h *haproxy.Manager) {
	w := &hotplug.Watcher{
		Logf: log.Printf,
		Changed: func(before, after hotplug.Sizes) {
			if !hotplug.SameMemory(before.MemoryKiB, after.MemoryKiB) {
				log.Printf("hotplug: memory %d MiB -> %d MiB", before.MemoryMiB(), after.MemoryMiB())
				events.Publish("system.memory_changed", map[string]uint64{
					"before_mib": before.MemoryMiB(), "after_mib": after.MemoryMiB(),
				})
			}
			if before.CPUs != after.CPUs {
				outcome := reloadForCPUs(h)
				log.Printf("hotplug: %d CPUs -> %d - haproxy: %s", before.CPUs, after.CPUs, outcome)
				events.Publish("system.cpus_changed", map[string]any{
					"before": before.CPUs, "after": after.CPUs, "haproxy": outcome,
				})
			}
		},
	}
	w.Run(ctx)
}

// reloadForCPUs reloads HAProxy so that it runs a thread per CPU the
// machine has now, and says what was done.
func reloadForCPUs(h *haproxy.Manager) string {
	if !h.Serving() {
		return "not running"
	}
	cfg, err := os.ReadFile(h.ConfigPath)
	if err != nil {
		return fmt.Sprintf("not reloaded: %v", err)
	}
	if kw := haproxy.PinnedThreads(cfg); kw != "" {
		return fmt.Sprintf("not reloaded: its configuration's %q fixes its threads", kw)
	}
	if err := h.Reload(); err != nil {
		return fmt.Sprintf("reload failed: %v", err)
	}
	return "reloaded"
}
