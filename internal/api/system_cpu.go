package api

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// parseCPUInfo reads /proc/cpuinfo - one block of "key\t: value" lines
// per logical CPU, blocks separated by a blank line. x86 names the
// model itself; arm64 only gives the core's implementer and part
// numbers, turned into a name here (what lscpu does too).
func parseCPUInfo(r io.Reader) []*janusv1alpha1.CPUInfo {
	var (
		cpus             []*janusv1alpha1.CPUInfo
		cur              *janusv1alpha1.CPUInfo
		implementer, prt string
	)
	finish := func() {
		if cur != nil && cur.ModelName == "" && implementer != "" {
			cur.ModelName = armCoreName(implementer, prt)
		}
		cur, implementer, prt = nil, "", ""
	}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			finish()
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if key == "processor" {
			n, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				continue
			}
			finish()
			cur = &janusv1alpha1.CPUInfo{Processor: uint32(n)}
			cpus = append(cpus, cur)
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "model name":
			cur.ModelName = value
		case "cpu MHz":
			if mhz, err := strconv.ParseFloat(value, 64); err == nil {
				cur.Mhz = mhz
			}
		case "CPU implementer":
			implementer = value
		case "CPU part":
			prt = value
		}
	}
	finish()
	return cpus
}

// armImplementers and armParts name the common arm64 cores (MIDR
// implementer and part numbers, as in the Arm TRMs and util-linux's
// lscpu). An unknown one is shown by its numbers rather than guessed.
var armImplementers = map[uint64]string{
	0x41: "ARM", 0x42: "Broadcom", 0x43: "Cavium", 0x46: "Fujitsu",
	0x48: "HiSilicon", 0x4e: "NVIDIA", 0x50: "APM", 0x51: "Qualcomm",
	0x61: "Apple", 0x6d: "Microsoft", 0xc0: "Ampere",
}

var armParts = map[uint64]string{
	0xd03: "Cortex-A53", 0xd04: "Cortex-A35", 0xd05: "Cortex-A55",
	0xd07: "Cortex-A57", 0xd08: "Cortex-A72", 0xd09: "Cortex-A73",
	0xd0a: "Cortex-A75", 0xd0b: "Cortex-A76", 0xd0c: "Neoverse-N1",
	0xd0d: "Cortex-A77", 0xd40: "Neoverse-V1", 0xd41: "Cortex-A78",
	0xd44: "Cortex-X1", 0xd46: "Cortex-A510", 0xd47: "Cortex-A710",
	0xd48: "Cortex-X2", 0xd49: "Neoverse-N2", 0xd4f: "Neoverse-V2",
}

func armCoreName(implementer, part string) string {
	impl, err := strconv.ParseUint(implementer, 0, 16)
	if err != nil {
		return ""
	}
	p, perr := strconv.ParseUint(part, 0, 16)
	vendor, known := armImplementers[impl]
	if !known {
		vendor = fmt.Sprintf("implementer %#x", impl)
	}
	if perr != nil {
		return vendor
	}
	if name, ok := armParts[p]; ok && impl == 0x41 {
		return vendor + " " + name
	}
	return fmt.Sprintf("%s part %#x", vendor, p)
}

// cpuTopology counts the physical packages and cores behind the
// logical CPUs from sysfs (/sys/devices/system/cpu as cpuDir) - the
// same files on x86 and arm64. 0 = the kernel doesn't say.
func cpuTopology(cpuDir fs.FS, cpus []*janusv1alpha1.CPUInfo) (sockets, cores uint32) {
	type core struct{ pkg, id string }
	pkgs := map[string]bool{}
	seen := map[core]bool{}
	for _, c := range cpus {
		dir := fmt.Sprintf("cpu%d/topology/", c.GetProcessor())
		pkg, err1 := fs.ReadFile(cpuDir, dir+"physical_package_id")
		id, err2 := fs.ReadFile(cpuDir, dir+"core_id")
		if err1 != nil || err2 != nil {
			return 0, 0
		}
		k := core{strings.TrimSpace(string(pkg)), strings.TrimSpace(string(id))}
		pkgs[k.pkg] = true
		seen[k] = true
	}
	return uint32(len(pkgs)), uint32(len(seen))
}

// fillMaxMHz gives CPUs /proc/cpuinfo has no frequency for (arm64)
// cpufreq's maximum, when the kernel has a cpufreq driver for them.
func fillMaxMHz(cpuDir fs.FS, cpus []*janusv1alpha1.CPUInfo) {
	for _, c := range cpus {
		if c.Mhz != 0 {
			continue
		}
		b, err := fs.ReadFile(cpuDir, fmt.Sprintf("cpu%d/cpufreq/cpuinfo_max_freq", c.GetProcessor()))
		if err != nil {
			continue
		}
		if khz, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			c.Mhz = khz / 1000
		}
	}
}
