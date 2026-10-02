package libvirt

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

func testSpec() hypervisor.MachineSpec {
	return hypervisor.MachineSpec{
		MachineID: "0123456789abcdef",
		Name:      "janus-lb<1>",
		VCPUs:     2,
		MemoryMiB: 1024,
		Image:     hypervisor.Image{Name: "janus-base-a055fbb6-v2026.10.02-4.qcow2", VirtualSize: 330 << 20},
		NICs: []hypervisor.NIC{
			{Network: "lab-mgmt", MAC: "52:54:00:aa:bb:01"},
			{Network: "lab-front", MAC: "52:54:00:aa:bb:02"},
		},
	}
}

func TestDomainXML(t *testing.T) {
	x := domainXML(testSpec(), "/var/lib/libvirt/janus/disk.qcow2", "/var/lib/libvirt/janus/ci.iso", "ctl-1")

	// Well-formed, with the name escaped.
	var d struct {
		Name     string `xml:"name"`
		Metadata struct {
			Inner string `xml:",innerxml"`
		} `xml:"metadata"`
		Memory     int `xml:"memory"`
		VCPU       int `xml:"vcpu"`
		Interfaces []struct {
			Source struct {
				Network string `xml:"network,attr"`
			} `xml:"source"`
			MAC struct {
				Address string `xml:"address,attr"`
			} `xml:"mac"`
		} `xml:"devices>interface"`
		Serial struct {
			Type string    `xml:"type,attr"`
			Log  *struct{} `xml:"log"`
		} `xml:"devices>serial"`
	}
	if err := xml.Unmarshal([]byte(x), &d); err != nil {
		t.Fatalf("domain XML doesn't parse: %v\n%s", err, x)
	}
	if d.Name != "janus-lb<1>" || d.Memory != 1024 || d.VCPU != 2 {
		t.Errorf("name/memory/vcpu = %q/%d/%d", d.Name, d.Memory, d.VCPU)
	}
	if len(d.Interfaces) != 2 || d.Interfaces[1].Source.Network != "lab-front" || d.Interfaces[1].MAC.Address != "52:54:00:aa:bb:02" {
		t.Errorf("interfaces = %+v", d.Interfaces)
	}
	// The first boot prints the root admin credential on the console:
	// never a log file on the host.
	if d.Serial.Type != "pty" || d.Serial.Log != nil || strings.Contains(x, "<log ") {
		t.Errorf("serial console must be a pty without a log file:\n%s", x)
	}
	if !strings.Contains(x, "name='secure-boot'") || !strings.Contains(x, "machine='q35'") {
		t.Error("expected q35 with UEFI firmware features")
	}

	// The ownership tag, as DomainGetMetadata would return the element.
	if !tagMatches(d.Metadata.Inner, "ctl-1", "0123456789abcdef") {
		t.Errorf("ownership tag doesn't match: %s", d.Metadata.Inner)
	}

	files, err := domainDiskFiles(x)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0] != "/var/lib/libvirt/janus/disk.qcow2" || files[1] != "/var/lib/libvirt/janus/ci.iso" {
		t.Errorf("domainDiskFiles = %v", files)
	}
}

func TestTagMatches(t *testing.T) {
	tag := `<janus:machine xmlns:janus="` + metadataNS + `" controller="ctl-1" id="m-1"/>`
	for _, tc := range []struct {
		meta, ctl, id string
		want          bool
	}{
		{tag, "ctl-1", "m-1", true},
		{tag, "ctl-2", "m-1", false}, // another Controller's machine
		{tag, "ctl-1", "m-2", false}, // another machine of ours
		{`<machine controller="" id=""/>`, "", "", false},
		{`not xml`, "ctl-1", "m-1", false},
	} {
		if got := tagMatches(tc.meta, tc.ctl, tc.id); got != tc.want {
			t.Errorf("tagMatches(%q, %q, %q) = %v, want %v", tc.meta, tc.ctl, tc.id, got, tc.want)
		}
	}
}

func TestOwnsVolumeName(t *testing.T) {
	spec := testSpec()
	if !ownsVolumeName(diskVolumeName(spec), spec.MachineID) || !ownsVolumeName(ciDataVolumeName(spec), spec.MachineID) {
		t.Error("the machine's own volumes not recognized")
	}
	for _, v := range []string{"janus-lb.qcow2", "debian-13.qcow2", spec.Image.Name, "janus-lb-fedcba98.qcow2"} {
		if ownsVolumeName(v, spec.MachineID) {
			t.Errorf("%s recognized as the machine's", v)
		}
	}
}

func TestUUIDRoundTrip(t *testing.T) {
	const s = "6f1c7d2e-1b8a-4c3d-9e0f-a1b2c3d4e5f6"
	u, err := parseUUID(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := formatUUID(u); got != s {
		t.Errorf("formatUUID(parseUUID(%s)) = %s", s, got)
	}
	if _, err := parseUUID("nope"); err == nil {
		t.Error("parseUUID accepted garbage")
	}
}

// What libvirt's dumpxml gives back for a defined domain.
const dumpedDomain = `<domain type='kvm'>
  <name>janus-lb1</name>
  <uuid>6f1c7d2e-1b8a-4c3d-9e0f-a1b2c3d4e5f6</uuid>
  <metadata>
    <janus:machine xmlns:janus="https://janus.sw-servers.net/xmlns/libvirt/machine/1" controller="ctl-1" id="0123456789abcdef"/>
  </metadata>
  <memory unit='KiB'>1048576</memory>
  <currentMemory unit='KiB'>1048576</currentMemory>
  <vcpu placement='static'>2</vcpu>
  <os firmware='efi'>
    <type arch='x86_64' machine='pc-q35-10.0'>hvm</type>
  </os>
</domain>`

func TestResizeXML(t *testing.T) {
	out, err := resizeXML(dumpedDomain, 4, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Memory        int `xml:"memory"`
		CurrentMemory int `xml:"currentMemory"`
		VCPU          struct {
			N         int    `xml:",chardata"`
			Placement string `xml:"placement,attr"`
		} `xml:"vcpu"`
		Metadata struct {
			Inner string `xml:",innerxml"`
		} `xml:"metadata"`
	}
	if err := xml.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if d.Memory != 2048*1024 || d.CurrentMemory != 2048*1024 || d.VCPU.N != 4 || d.VCPU.Placement != "static" {
		t.Errorf("resized to %+v", d)
	}
	if !tagMatches(d.Metadata.Inner, "ctl-1", "0123456789abcdef") {
		t.Error("the ownership tag was lost")
	}
	if _, err := resizeXML("<domain/>", 1, 512); err == nil {
		t.Error("a definition without memory/vcpu accepted")
	}
}
