package libvirt

import (
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// A machine's size in its domain definition. A fixed one: <memory> and
// <vcpu>. A scalable one (hypervisor.Hardware's maxima) is started with
// room to grow without a restart (docs/hypervisors.md, auto-scaling):
//
//   - vCPUs: <vcpu current='N'>MAX</vcpu> - setvcpus --live plugs and
//     unplugs them, the node onlines them;
//   - memory: one NUMA cell of the nominal memory, plus a virtio-mem
//     device of the difference up to the maximum, nothing plugged at
//     start - its "requested" size is what Resize changes (the kernel
//     onlines what it gets). virtio-mem needs <maxMemory> and a NUMA
//     node to plug into.
//
// libvirt reports a scalable domain's <memory> and <currentMemory> -
// and DomainGetInfo's memory - as the maximum: what the machine has is
// read from the cell and the device instead (sizes).

// virtioMemBlockKiB is the virtio-mem device's block, the smallest
// change it makes - what QEMU uses by default on x86.
const virtioMemBlockKiB = 2048

// memoryStepMiB is what Resize changes a scalable machine's memory by:
// the kernel's memory block on x86, a whole number of virtio-mem
// blocks.
const memoryStepMiB = 128

// maxVCPUs is the most vCPUs a machine may have: the Controller's
// bound (machines_update.go).
const maxVCPUs = 64

func checkHardware(hw hypervisor.Hardware) error {
	if hw.VCPUs < 1 || hw.MemoryMiB < 1 {
		return errors.New("a machine needs at least one vCPU and some memory")
	}
	if hw.MaxVCPUs > maxVCPUs {
		return fmt.Errorf("at most %d vCPUs", maxVCPUs)
	}
	if hw.ScalableMemory() && (hw.MemoryMiB%2 != 0 || hw.MaxMemoryMiB%2 != 0) {
		return fmt.Errorf("a scalable machine's memory goes by %d KiB blocks: its memory and maximum must be even numbers of MiB", virtioMemBlockKiB)
	}
	return nil
}

// sizeXML is a domain's <maxMemory>, <memory> and <vcpu>.
func sizeXML(hw hypervisor.Hardware) string {
	var b strings.Builder
	if hw.ScalableMemory() {
		fmt.Fprintf(&b, "  <maxMemory slots='4' unit='MiB'>%d</maxMemory>\n  <memory unit='MiB'>%d</memory>\n", hw.MaxMemoryMiB, hw.MaxMemoryMiB)
	} else {
		fmt.Fprintf(&b, "  <memory unit='MiB'>%d</memory>\n", hw.MemoryMiB)
	}
	if hw.ScalableCPUs() {
		fmt.Fprintf(&b, "  <vcpu placement='static' current='%d'>%d</vcpu>\n", hw.VCPUs, hw.MaxVCPUs)
	} else {
		fmt.Fprintf(&b, "  <vcpu placement='static'>%d</vcpu>\n", hw.VCPUs)
	}
	return b.String()
}

// numaXML is a scalable machine's NUMA cell, inside <cpu>: every vCPU it
// may have, its nominal memory.
func numaXML(hw hypervisor.Hardware) string {
	return fmt.Sprintf("    <numa>\n      <cell id='0' cpus='0-%d' memory='%d' unit='MiB'/>\n    </numa>\n",
		max(hw.MaxVCPUs, hw.VCPUs)-1, hw.MemoryMiB)
}

// cpuXML is a domain's <cpu>.
func cpuXML(hw hypervisor.Hardware) string {
	if !hw.ScalableMemory() {
		return "  <cpu mode='host-passthrough' check='none'/>\n"
	}
	return "  <cpu mode='host-passthrough' check='none'>\n" + numaXML(hw) + "  </cpu>\n"
}

// virtioMemXML is a scalable machine's virtio-mem device, nothing
// plugged: it starts at its nominal memory.
func virtioMemXML(hw hypervisor.Hardware) string {
	if !hw.ScalableMemory() {
		return ""
	}
	return fmt.Sprintf("    <memory model='virtio-mem'>\n      <target>\n        <size unit='MiB'>%d</size>\n        <node>0</node>\n        <block unit='KiB'>%d</block>\n        <requested unit='MiB'>0</requested>\n      </target>\n    </memory>\n",
		hw.MaxMemoryMiB-hw.MemoryMiB, virtioMemBlockKiB)
}

var (
	maxMemoryRe = regexp.MustCompile(`(?s)[ \t]*<maxMemory\b[^>]*?(?:/>|>.*?</maxMemory>)\n?`)
	vcpusRe     = regexp.MustCompile(`(?s)[ \t]*<vcpus>.*?</vcpus>\n?`)
	numaRe      = regexp.MustCompile(`(?s)[ \t]*<numa>.*?</numa>\n?`)
	virtioMemRe = regexp.MustCompile(`(?s)[ \t]*<memory model=['"]virtio-mem['"].*?</memory>\n?`)
	cpuRe       = regexp.MustCompile(`(?s)[ \t]*<cpu\b[^>]*?(?:/>|>.*?</cpu>)\n?`)
	sizeLineRe  = regexp.MustCompile(`[ \t]*<(?:memory|currentMemory)\b[^>]*>\s*\d+\s*</(?:memory|currentMemory)>\n?`)
	vcpuLineRe  = regexp.MustCompile(`[ \t]*<vcpu\b[^>]*>\s*\d+\s*</vcpu>\n?`)
)

// hardwareXML sets a domain definition's size and maxima, leaving the
// rest - the ownership tag, the devices' PCI addresses - as libvirt
// wrote it: whatever made it scalable before goes (<maxMemory>, the
// <vcpus> list libvirt keeps once vCPUs were plugged, the NUMA cell, the
// virtio-mem device), then hw's is written. A virtio-mem device comes
// back with nothing plugged: the machine starts at its nominal size.
func hardwareXML(domXML string, hw hypervisor.Hardware) (string, error) {
	if !sizeLineRe.MatchString(domXML) || !vcpuLineRe.MatchString(domXML) {
		return "", fmt.Errorf("no <memory> or <vcpu> in the domain's definition")
	}
	out := maxMemoryRe.ReplaceAllString(domXML, "")
	out = vcpusRe.ReplaceAllString(out, "")
	out = virtioMemRe.ReplaceAllString(out, "")
	out = numaRe.ReplaceAllString(out, "")

	// <memory>, <currentMemory> and <vcpu>: one block where <memory> was.
	first := true
	out = sizeLineRe.ReplaceAllStringFunc(out, func(string) string {
		if first {
			first = false
			return "\x00SIZE\x00"
		}
		return ""
	})
	out = vcpuLineRe.ReplaceAllString(out, "")
	out = strings.Replace(out, "\x00SIZE\x00", sizeXML(hw), 1)

	if hw.ScalableMemory() {
		loc := cpuRe.FindStringIndex(out)
		if loc == nil {
			// No <cpu> (a definition libvirt has no CPU model for): one
			// after the features, or the OS - libvirt puts it in its
			// place.
			after := strings.Index(out, "</features>\n")
			if after >= 0 {
				after += len("</features>\n")
			} else if after = strings.Index(out, "</os>\n"); after >= 0 {
				after += len("</os>\n")
			} else {
				return "", fmt.Errorf("no <cpu> nor <os> in the domain's definition to put a NUMA cell after")
			}
			out = out[:after] + "  <cpu mode='host-passthrough' check='none'/>\n" + out[after:]
			loc = cpuRe.FindStringIndex(out)
		}
		cpu := strings.TrimRight(out[loc[0]:loc[1]], "\n")
		var next string
		if strings.HasSuffix(cpu, "/>") {
			next = strings.TrimRight(strings.TrimSuffix(cpu, "/>"), " ") + ">\n" + numaXML(hw) + "  </cpu>\n"
		} else {
			i := strings.LastIndex(cpu, "</cpu>")
			next = strings.TrimRight(cpu[:i], " \t") + numaXML(hw) + "  </cpu>\n"
		}
		out = out[:loc[0]] + next + out[loc[1]:]
		i := strings.LastIndex(out, "</devices>")
		if i < 0 {
			return "", fmt.Errorf("no <devices> in the domain's definition")
		}
		line := strings.LastIndex(out[:i], "\n") + 1
		out = out[:line] + virtioMemXML(hw) + out[line:]
	}
	return out, nil
}

// sizes is a domain's size as its live definition says it: what it has,
// and the most it can have without a restart.
type sizes struct {
	vcpus, maxVCPUs   int
	memMiB, maxMemMiB int
	// virtio-mem: the device's nominal base (the NUMA cells), its size,
	// what's requested and plugged - 0 when the domain has none.
	baseMiB, devSizeMiB, requestedMiB, pluggedMiB int
	hasVirtioMem                                  bool
}

type sizeElem struct {
	Value string `xml:",chardata"`
	Unit  string `xml:"unit,attr"`
}

type domainSizes struct {
	Memory sizeElem `xml:"memory"`
	VCPU   struct {
		Current string `xml:"current,attr"`
		Value   string `xml:",chardata"`
	} `xml:"vcpu"`
	Cells []struct {
		Memory string `xml:"memory,attr"`
		Unit   string `xml:"unit,attr"`
	} `xml:"cpu>numa>cell"`
	MemoryDevices []struct {
		Model     string   `xml:"model,attr"`
		Size      sizeElem `xml:"target>size"`
		Requested sizeElem `xml:"target>requested"`
		Current   sizeElem `xml:"target>current"`
	} `xml:"devices>memory"`
}

// toMiB converts a libvirt size to MiB, its unit as libvirt names it
// (KiB when unset).
func toMiB(value, unit string) (int, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", value, err)
	}
	mult := map[string]uint64{
		"b": 1, "bytes": 1,
		"": 1 << 10, "k": 1 << 10, "KiB": 1 << 10, "KB": 1000,
		"M": 1 << 20, "MiB": 1 << 20, "MB": 1000 * 1000,
		"G": 1 << 30, "GiB": 1 << 30, "GB": 1000 * 1000 * 1000,
		"T": 1 << 40, "TiB": 1 << 40,
	}[unit]
	if mult == 0 {
		return 0, fmt.Errorf("unknown size unit %q", unit)
	}
	return int(n * mult >> 20), nil
}

// parseSizes reads a domain definition's size.
func parseSizes(domXML string) (sizes, error) {
	var d domainSizes
	if err := xml.Unmarshal([]byte(domXML), &d); err != nil {
		return sizes{}, err
	}
	var s sizes
	var err error
	if s.maxVCPUs, err = strconv.Atoi(strings.TrimSpace(d.VCPU.Value)); err != nil {
		return sizes{}, fmt.Errorf("<vcpu>: %w", err)
	}
	s.vcpus = s.maxVCPUs
	if d.VCPU.Current != "" {
		if s.vcpus, err = strconv.Atoi(d.VCPU.Current); err != nil {
			return sizes{}, fmt.Errorf("<vcpu current>: %w", err)
		}
	}
	for _, c := range d.Cells {
		mib, err := toMiB(c.Memory, c.Unit)
		if err != nil {
			return sizes{}, err
		}
		s.baseMiB += mib
	}
	for _, dev := range d.MemoryDevices {
		if dev.Model != "virtio-mem" {
			continue
		}
		s.hasVirtioMem = true
		if s.devSizeMiB, err = toMiB(dev.Size.Value, dev.Size.Unit); err != nil {
			return sizes{}, err
		}
		if dev.Requested.Value != "" {
			if s.requestedMiB, err = toMiB(dev.Requested.Value, dev.Requested.Unit); err != nil {
				return sizes{}, err
			}
		}
		// A stopped domain's definition has no <current>: what's
		// requested is what it will have.
		s.pluggedMiB = s.requestedMiB
		if dev.Current.Value != "" {
			if s.pluggedMiB, err = toMiB(dev.Current.Value, dev.Current.Unit); err != nil {
				return sizes{}, err
			}
		}
		break
	}
	switch {
	case s.hasVirtioMem:
		s.memMiB, s.maxMemMiB = s.baseMiB+s.pluggedMiB, s.baseMiB+s.devSizeMiB
	default:
		if s.memMiB, err = toMiB(d.Memory.Value, d.Memory.Unit); err != nil {
			return sizes{}, fmt.Errorf("<memory>: %w", err)
		}
		s.maxMemMiB = s.memMiB
	}
	return s, nil
}

// requestedRe is a virtio-mem device's requested size.
var requestedRe = regexp.MustCompile(`<requested\b[^>]*>\s*\d+\s*</requested>`)

// virtioMemRequestXML is the domain's virtio-mem device - from domXML,
// its alias and address included, as libvirt matches it - asking for
// requestedMiB.
func virtioMemRequestXML(domXML string, requestedMiB int) (string, error) {
	dev := virtioMemRe.FindString(domXML)
	if dev == "" {
		return "", errors.New("the virtual machine has no virtio-mem device: its memory can't change while it runs")
	}
	if !requestedRe.MatchString(dev) {
		return "", errors.New("the virtio-mem device has no requested size")
	}
	return strings.TrimSpace(requestedRe.ReplaceAllString(dev, fmt.Sprintf("<requested unit='KiB'>%d</requested>", requestedMiB*1024))), nil
}
