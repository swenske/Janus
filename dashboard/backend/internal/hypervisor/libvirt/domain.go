package libvirt

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// metadataNS is the namespace of the ownership tag every virtual
// machine the Controller creates carries in its <metadata>.
const metadataNS = "https://janus.sw-servers.net/xmlns/libvirt/machine/1"

// ownerTag is that tag: the Controller's ID and the machine's.
type ownerTag struct {
	XMLName    xml.Name `xml:"machine"`
	Controller string   `xml:"controller,attr"`
	ID         string   `xml:"id,attr"`
}

// tagMatches reports whether a domain's ownership tag (its <metadata>
// element in metadataNS, as libvirt returns it) names this Controller
// and this machine. Both IDs must be non-empty.
func tagMatches(meta, controllerID, machineID string) bool {
	var t ownerTag
	if err := xml.Unmarshal([]byte(meta), &t); err != nil {
		return false
	}
	return controllerID != "" && machineID != "" && t.Controller == controllerID && t.ID == machineID
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// diskVolumeName and ciDataVolumeName carry the machine's ID, so a
// volume is only ever recognized as a machine's when its name says so.
func diskVolumeName(spec hypervisor.MachineSpec) string {
	return fmt.Sprintf("%s-%s.qcow2", spec.Name, shortID(spec.MachineID))
}

func ciDataVolumeName(spec hypervisor.MachineSpec) string {
	return fmt.Sprintf("%s-%s-cidata.iso", spec.Name, shortID(spec.MachineID))
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ownsVolumeName reports whether a volume name is one CreateMachine
// made for machineID.
func ownsVolumeName(name, machineID string) bool {
	return strings.Contains(name, "-"+shortID(machineID)+".") || strings.Contains(name, "-"+shortID(machineID)+"-cidata.")
}

func volumeXML(name string, capacity uint64, format string) string {
	return fmt.Sprintf(`<volume>
  <name>%s</name>
  <capacity unit='bytes'>%d</capacity>
  <target>
    <format type='%s'/>
  </target>
</volume>
`, esc(name), capacity, format)
}

// domainXML is a Janus node's virtual machine, its disks at diskPath and
// ciPath (the pool's volumes - as plain files: libvirt hands a file
// disk's ownership to QEMU at start, which it didn't do on Debian 13's
// libvirt 11.3 for a type='volume' disk): q35 with UEFI (Secure
// Boot off - the VM images' boot entries aren't signed, see
// image/kvm/README.md), the disk on virtio, the NoCloud volume as a
// SATA CD-ROM (the node scans sr*), virtio NICs, and a serial console
// on a pty only - deliberately never logged to a file on the host: a
// node's first boot prints its root admin credential there. The
// qemu-guest-agent channel is harmless without that extension.
func domainXML(spec hypervisor.MachineSpec, diskPath, ciPath, controllerID string) string {
	var nics strings.Builder
	for _, n := range spec.NICs {
		fmt.Fprintf(&nics, `    <interface type='network'>
      <source network='%s'/>
      <mac address='%s'/>
      <model type='virtio'/>
    </interface>
`, esc(n.Network), esc(n.MAC))
	}
	return fmt.Sprintf(`<domain type='kvm'>
  <name>%s</name>
  <metadata>
    <janus:machine xmlns:janus='%s' controller='%s' id='%s'/>
  </metadata>
  <memory unit='MiB'>%d</memory>
  <vcpu>%d</vcpu>
  <os firmware='efi'>
    <type arch='x86_64' machine='q35'>hvm</type>
    <firmware>
      <feature enabled='no' name='secure-boot'/>
      <feature enabled='no' name='enrolled-keys'/>
    </firmware>
    <boot dev='hd'/>
  </os>
  <features>
    <acpi/>
    <apic/>
  </features>
  <cpu mode='host-passthrough' check='none'/>
  <clock offset='utc'/>
  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>restart</on_crash>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' discard='unmap'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='%s'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
%s    <serial type='pty'>
      <target port='0'/>
    </serial>
    <console type='pty'>
      <target type='serial' port='0'/>
    </console>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
    </channel>
    <video>
      <model type='vga'/>
    </video>
    <memballoon model='none'/>
  </devices>
</domain>
`, esc(spec.Name), metadataNS, esc(controllerID), esc(spec.MachineID),
		spec.MemoryMiB, spec.VCPUs,
		esc(diskPath), esc(ciPath),
		nics.String())
}

// domainDiskFiles lists the files a domain's disks use - read back from
// the hypervisor, so destroying a machine only deletes volumes it
// really has.
func domainDiskFiles(domXML string) ([]string, error) {
	var d struct {
		Disks []struct {
			Source struct {
				File string `xml:"file,attr"`
			} `xml:"source"`
		} `xml:"devices>disk"`
	}
	if err := xml.Unmarshal([]byte(domXML), &d); err != nil {
		return nil, err
	}
	var out []string
	for _, disk := range d.Disks {
		if disk.Source.File != "" {
			out = append(out, disk.Source.File)
		}
	}
	return out, nil
}

var (
	memoryRe        = regexp.MustCompile(`<memory\b[^>]*>\s*\d+\s*</memory>`)
	currentMemoryRe = regexp.MustCompile(`<currentMemory\b[^>]*>\s*\d+\s*</currentMemory>`)
	vcpuRe          = regexp.MustCompile(`<vcpu\b([^>]*)>\s*\d+\s*</vcpu>`)
)

var (
	interfaceRe = regexp.MustCompile(`(?s)[ \t]*<interface type='network'>.*?</interface>\n?`)
	ifaceMACRe  = regexp.MustCompile(`<mac address='([^']+)'`)
	ifaceSrcRe  = regexp.MustCompile(`<source network='[^']*'`)
)

// reconfigureXML sets a domain definition's memory, vCPUs and network
// interfaces, leaving the rest - the ownership tag, the devices' PCI
// addresses - as libvirt wrote it. An added interface gets its address
// when libvirt defines the domain.
func reconfigureXML(domXML string, vcpus, memoryMiB int, nics []hypervisor.NIC) (string, error) {
	out, err := resizeXML(domXML, vcpus, memoryMiB)
	if err != nil {
		return "", err
	}
	want := map[string]hypervisor.NIC{}
	for _, n := range nics {
		want[strings.ToLower(n.MAC)] = n
	}
	present := map[string]bool{}
	out = interfaceRe.ReplaceAllStringFunc(out, func(block string) string {
		m := ifaceMACRe.FindStringSubmatch(block)
		if m == nil {
			return block
		}
		mac := strings.ToLower(m[1])
		n, ok := want[mac]
		if !ok {
			return "" // removed
		}
		present[mac] = true
		return ifaceSrcRe.ReplaceAllString(block, "<source network='"+esc(n.Network)+"'")
	})
	var added strings.Builder
	for _, n := range nics {
		if present[strings.ToLower(n.MAC)] {
			continue
		}
		fmt.Fprintf(&added, "    <interface type='network'>\n      <mac address='%s'/>\n      <source network='%s'/>\n      <model type='virtio'/>\n    </interface>\n", esc(n.MAC), esc(n.Network))
	}
	if added.Len() > 0 {
		i := strings.LastIndex(out, "</devices>")
		if i < 0 {
			return "", fmt.Errorf("no <devices> in the domain's definition")
		}
		out = out[:i] + added.String() + "  " + out[i:]
	}
	return out, nil
}

// resizeXML sets a domain definition's memory and vCPUs, leaving the rest
// - the ownership tag included - as libvirt wrote it.
func resizeXML(domXML string, vcpus, memoryMiB int) (string, error) {
	if !memoryRe.MatchString(domXML) || !vcpuRe.MatchString(domXML) {
		return "", fmt.Errorf("no <memory> or <vcpu> in the domain's definition")
	}
	kib := memoryMiB * 1024
	out := memoryRe.ReplaceAllString(domXML, fmt.Sprintf("<memory unit='KiB'>%d</memory>", kib))
	out = currentMemoryRe.ReplaceAllString(out, fmt.Sprintf("<currentMemory unit='KiB'>%d</currentMemory>", kib))
	out = vcpuRe.ReplaceAllString(out, fmt.Sprintf("<vcpu${1}>%d</vcpu>", vcpus))
	return out, nil
}
