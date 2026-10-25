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
// qemu-guest-agent channel is harmless without that extension. A
// scalable machine's size is in size.go.
//
// loader is the UEFI firmware's code (statelessLoader): given, the
// machine runs it alone, read-only and **without a variable store** -
// Janus never uses UEFI variables, and OVMF's store fills up with the
// console variables it writes at every boot, about 6 KB, then is
// compacted through QEMU's emulated flash: one firmware reboot in about
// 40 took 30-40 s more (docs/guide/updates.md). Empty: libvirt picks the
// firmware (firmware='efi') and gives it a store, as before.
func domainXML(spec hypervisor.MachineSpec, diskPath, ciPath, controllerID, loader string) string {
	var nics strings.Builder
	for _, n := range spec.NICs {
		fmt.Fprintf(&nics, `    <interface type='network'>
      <source network='%s'/>
      <mac address='%s'/>
      <model type='virtio'/>
    </interface>
`, esc(n.Network), esc(n.MAC))
	}
	os := `  <os firmware='efi'>
    <type arch='x86_64' machine='q35'>hvm</type>
    <firmware>
      <feature enabled='no' name='secure-boot'/>
      <feature enabled='no' name='enrolled-keys'/>
    </firmware>
    <boot dev='hd'/>
  </os>`
	if loader != "" {
		os = fmt.Sprintf(`  <os>
    <type arch='x86_64' machine='q35'>hvm</type>
    <loader readonly='yes' type='pflash' stateless='yes'>%s</loader>
    <boot dev='hd'/>
  </os>`, esc(loader))
	}
	return fmt.Sprintf(`<domain type='kvm'>
  <name>%s</name>
  <metadata>
    <janus:machine xmlns:janus='%s' controller='%s' id='%s'/>
  </metadata>
%s%s
  <features>
    <acpi/>
    <apic/>
  </features>
%s  <clock offset='utc'/>
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
%s  </devices>
</domain>
`, esc(spec.Name), metadataNS, esc(controllerID), esc(spec.MachineID),
		sizeXML(spec.Hardware), os, cpuXML(spec.Hardware),
		esc(diskPath), esc(ciPath),
		nics.String(), virtioMemXML(spec.Hardware))
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
	interfaceRe = regexp.MustCompile(`(?s)[ \t]*<interface type='network'>.*?</interface>\n?`)
	ifaceMACRe  = regexp.MustCompile(`<mac address='([^']+)'`)
	ifaceSrcRe  = regexp.MustCompile(`<source network='[^']*'`)
)

// reconfigureXML sets a domain definition's size, maxima and network
// interfaces, leaving the rest - the ownership tag, the devices' PCI
// addresses - as libvirt wrote it. An added interface gets its address
// when libvirt defines the domain.
func reconfigureXML(domXML string, hw hypervisor.Hardware, nics []hypervisor.NIC) (string, error) {
	out, err := hardwareXML(domXML, hw)
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

// statelessLoader picks, among the UEFI firmware images a host's
// libvirt offers for q35 (its domain capabilities' loader values), the
// plain OVMF code: no Secure Boot build (secboot, ms, snakeoil,
// enrolled), no confidential-computing one (amdsev, inteltdx), no
// combined code+variables image - its code alone runs without a store.
// The 4 MiB build first. "": none - the machine gets libvirt's choice,
// with a store.
func statelessLoader(domCaps string) string {
	var caps struct {
		OS struct {
			Loader struct {
				Values []string `xml:"value"`
			} `xml:"loader"`
		} `xml:"os"`
	}
	if xml.Unmarshal([]byte(domCaps), &caps) != nil {
		return ""
	}
	best := ""
	for _, v := range caps.OS.Loader.Values {
		name := strings.ToLower(v[strings.LastIndex(v, "/")+1:])
		if !strings.Contains(name, "code") || !strings.HasSuffix(name, ".fd") {
			continue
		}
		skip := false
		for _, bad := range []string{"secboot", ".ms.", "_ms.", "snakeoil", "enrolled", "secure", "amdsev", "inteltdx", "csm"} {
			if strings.Contains(name, bad) {
				skip = true
			}
		}
		if skip {
			continue
		}
		if best == "" || (strings.Contains(name, "4m") && !strings.Contains(strings.ToLower(best), "4m")) {
			best = v
		}
	}
	return best
}
