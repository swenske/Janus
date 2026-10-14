// Package hotplug follows the vCPUs and memory a hypervisor plugs into, or
// takes out of, the running virtual machine - the Controller's
// auto-scaling. The kernel leaves a hot-added CPU offline, and a node has
// no udev: rootfs/init onlines it (OnlineCPUs). The kernel onlines
// hot-added memory itself (MHP_DEFAULT_ONLINE_TYPE_ONLINE_AUTO). janusd's
// Watcher tells what changed once it has settled.
//
// No dependency beyond the standard library: rootfs/init imports it.
package hotplug

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CPUDir and Meminfo are where the kernel says what the machine has.
const (
	CPUDir  = "/sys/devices/system/cpu"
	Meminfo = "/proc/meminfo"
)

// ParseList parses a kernel CPU list, as in /sys/devices/system/cpu/online:
// "0-3,8,10-11" - sorted, without duplicates. Empty is no CPU.
func ParseList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(lo)
		if err != nil || first < 0 {
			return nil, fmt.Errorf("CPU list %q: bad %q", s, part)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(hi); err != nil || last < first {
				return nil, fmt.Errorf("CPU list %q: bad %q", s, part)
			}
		}
		// NR_CPUS is 512: a list going further isn't the kernel's.
		if last >= 8192 {
			return nil, fmt.Errorf("CPU list %q: %d is out of range", s, last)
		}
		for c := first; c <= last; c++ {
			seen[c] = true
		}
	}
	out := make([]int, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Ints(out)
	return out, nil
}

func readList(path string) ([]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseList(string(data))
}

// OnlineCPUs onlines every CPU under dir (CPUDir) that is present but
// offline, and returns those it onlined, and why the others couldn't be.
// A CPU being unplugged is offlined by the kernel before it goes: the
// write waits for the unplug to finish (the kernel holds its hotplug lock
// throughout), then finds the CPU gone - not reported.
func OnlineCPUs(dir string) (onlined []int, failed map[int]error) {
	present, err := readList(filepath.Join(dir, "present"))
	if err != nil {
		return nil, map[int]error{-1: err}
	}
	online, err := readList(filepath.Join(dir, "online"))
	if err != nil {
		return nil, map[int]error{-1: err}
	}
	up := map[int]bool{}
	for _, c := range online {
		up[c] = true
	}
	for _, c := range present {
		if up[c] {
			continue
		}
		f := filepath.Join(dir, fmt.Sprintf("cpu%d", c), "online")
		// Opened without O_CREATE: sysfs files exist or don't.
		err := writeExisting(f, "1")
		switch {
		case err == nil:
			onlined = append(onlined, c)
		case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENODEV):
			// Unplugged meanwhile, or a CPU without an online switch.
		default:
			if failed == nil {
				failed = map[int]error{}
			}
			failed[c] = err
		}
	}
	return onlined, failed
}

func writeExisting(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(value); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Sizes is what the machine has in use.
type Sizes struct {
	CPUs      int    // online CPUs
	MemoryKiB uint64 // MemTotal
}

// MemoryMiB is MemoryKiB in MiB, rounded down.
func (s Sizes) MemoryMiB() uint64 { return s.MemoryKiB / 1024 }

// memoryNoiseKiB is a MemTotal change too small to be a plug: virtio-mem's
// smallest block is 2 MiB.
const memoryNoiseKiB = 2048

// Same reports whether a and b are the same sizes, memory changes below
// virtio-mem's smallest block aside.
func Same(a, b Sizes) bool {
	return a.CPUs == b.CPUs && SameMemory(a.MemoryKiB, b.MemoryKiB)
}

// SameMemory reports whether two MemTotal readings are the same memory,
// changes below virtio-mem's smallest block aside.
func SameMemory(aKiB, bKiB uint64) bool {
	d := int64(aKiB) - int64(bKiB)
	return d > -memoryNoiseKiB && d < memoryNoiseKiB
}

// Read reads the machine's sizes: the CPUs online under cpuDir (CPUDir),
// MemTotal from meminfo (Meminfo).
func Read(cpuDir, meminfo string) (Sizes, error) {
	online, err := readList(filepath.Join(cpuDir, "online"))
	if err != nil {
		return Sizes{}, err
	}
	data, err := os.ReadFile(meminfo)
	if err != nil {
		return Sizes{}, err
	}
	kib, err := memTotal(data)
	if err != nil {
		return Sizes{}, err
	}
	return Sizes{CPUs: len(online), MemoryKiB: kib}, nil
}

func memTotal(meminfo []byte) (uint64, error) {
	sc := bufio.NewScanner(bytes.NewReader(meminfo))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || key != "MemTotal" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			break
		}
		return strconv.ParseUint(fields[0], 10, 64)
	}
	return 0, errors.New("meminfo: no MemTotal")
}

// SettleFor is how long new sizes must hold before they're a change: a
// hypervisor adds several vCPUs one after the other, Proxmox takes its
// memory modules out one every 3 s - one change, not one per step.
const SettleFor = 5 * time.Second

// settle reports a change of sizes once it held for SettleFor.
type settle struct {
	reported, pending Sizes
	since             time.Time // when pending was first read
	waiting           bool
}

// next takes a read made at now and returns the change it settles, if
// any.
func (s *settle) next(cur Sizes, now time.Time) (before, after Sizes, changed bool) {
	switch {
	case Same(cur, s.reported):
		s.waiting = false
	case s.waiting && Same(cur, s.pending):
		if now.Sub(s.since) < SettleFor {
			break
		}
		before, after = s.reported, cur
		s.reported, s.waiting = cur, false
		return before, after, true
	default:
		s.pending, s.since, s.waiting = cur, now, true
	}
	return Sizes{}, Sizes{}, false
}

// Watcher calls Changed when the machine's sizes changed and held for
// SettleFor.
type Watcher struct {
	CPUDir, Meminfo string        // CPUDir, Meminfo when empty
	Every           time.Duration // between reads; 2 s when zero
	Changed         func(before, after Sizes)
	// Logf, if set, tells why the sizes couldn't be read (once).
	Logf func(format string, args ...any)
}

// Run reads the sizes until ctx ends. The first read is the reference:
// what the machine had when janusd started isn't a change.
func (w *Watcher) Run(ctx context.Context) {
	cpuDir, meminfo, every := w.CPUDir, w.Meminfo, w.Every
	if cpuDir == "" {
		cpuDir = CPUDir
	}
	if meminfo == "" {
		meminfo = Meminfo
	}
	if every <= 0 {
		every = 2 * time.Second
	}
	var s settle
	started, complained := false, false
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		cur, err := Read(cpuDir, meminfo)
		switch {
		case err != nil:
			if !complained && w.Logf != nil {
				w.Logf("hotplug: %v - vCPU and memory changes aren't followed", err)
			}
			complained = true
		case !started:
			s.reported, started = cur, true
		default:
			if before, after, ok := s.next(cur, time.Now()); ok && w.Changed != nil {
				w.Changed(before, after)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
