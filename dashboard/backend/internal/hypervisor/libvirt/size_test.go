package libvirt

import (
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

var scalable = hypervisor.Hardware{VCPUs: 2, MemoryMiB: 1024, MaxVCPUs: 4, MaxMemoryMiB: 4096}

func TestScalableDomainXML(t *testing.T) {
	spec := testSpec()
	spec.Hardware = scalable
	x := domainXML(spec, "/disk.qcow2", "/ci.iso", "ctl-1", "")
	sz, err := parseSizes(x)
	if err != nil {
		t.Fatalf("%v\n%s", err, x)
	}
	want := sizes{vcpus: 2, maxVCPUs: 4, memMiB: 1024, maxMemMiB: 4096, baseMiB: 1024, devSizeMiB: 3072, hasVirtioMem: true}
	if sz != want {
		t.Errorf("sizes = %+v, want %+v", sz, want)
	}
	for _, s := range []string{"<maxMemory slots='4' unit='MiB'>4096</maxMemory>", "<vcpu placement='static' current='2'>4</vcpu>", "<cell id='0' cpus='0-3' memory='1024' unit='MiB'/>", "<memory model='virtio-mem'>"} {
		if !strings.Contains(x, s) {
			t.Errorf("no %s in:\n%s", s, x)
		}
	}

	// Only the vCPUs scalable: no NUMA cell, no virtio-mem.
	spec.Hardware = hypervisor.Hardware{VCPUs: 1, MemoryMiB: 1024, MaxVCPUs: 8}
	x = domainXML(spec, "/disk.qcow2", "/ci.iso", "ctl-1", "")
	if strings.Contains(x, "numa") || strings.Contains(x, "virtio-mem") || strings.Contains(x, "maxMemory") || !strings.Contains(x, "current='1'>8</vcpu>") {
		t.Errorf("vCPUs only:\n%s", x)
	}
}

// What libvirt 11.3's dumpxml --inactive gives back for a scalable domain
// whose vCPUs and memory were resized (kvm01, 2026-10-14).
const dumpedScalable = `<domain type='kvm'>
  <name>janus-lb1</name>
  <uuid>6f1c7d2e-1b8a-4c3d-9e0f-a1b2c3d4e5f6</uuid>
  <metadata>
    <janus:machine xmlns:janus="https://janus.sw-servers.net/xmlns/libvirt/machine/1" controller="ctl-1" id="0123456789abcdef"/>
  </metadata>
  <maxMemory slots='4' unit='KiB'>5242880</maxMemory>
  <memory unit='KiB'>5242880</memory>
  <currentMemory unit='KiB'>5242880</currentMemory>
  <vcpu placement='static' current='3'>4</vcpu>
  <vcpus>
    <vcpu id='0' enabled='yes' hotpluggable='no' order='1'/>
    <vcpu id='1' enabled='yes' hotpluggable='yes' order='2'/>
    <vcpu id='2' enabled='yes' hotpluggable='yes' order='3'/>
    <vcpu id='3' enabled='no' hotpluggable='yes'/>
  </vcpus>
  <os firmware='efi'>
    <type arch='x86_64' machine='pc-q35-10.0'>hvm</type>
  </os>
  <cpu mode='host-passthrough' check='none' migratable='on'>
    <numa>
      <cell id='0' cpus='0-3' memory='1048576' unit='KiB'/>
    </numa>
  </cpu>
  <devices>
    <interface type='network'>
      <mac address='52:54:00:aa:bb:01'/>
      <source network='lab-mgmt'/>
      <model type='virtio'/>
      <address type='pci' domain='0x0000' bus='0x01' slot='0x00' function='0x0'/>
    </interface>
    <memballoon model='none'/>
    <memory model='virtio-mem'>
      <target>
        <size unit='KiB'>4194304</size>
        <node>0</node>
        <block unit='KiB'>2048</block>
        <requested unit='KiB'>2097152</requested>
      </target>
      <address type='pci' domain='0x0000' bus='0x07' slot='0x00' function='0x0'/>
    </memory>
  </devices>
</domain>`

// The same domain's live definition: what's plugged is <current>.
var liveScalable = strings.Replace(dumpedScalable, "<requested unit='KiB'>2097152</requested>",
	"<requested unit='KiB'>2097152</requested>\n        <current unit='KiB'>1048576</current>\n        <address base='0x100000000'/>", 1)

func TestParseSizes(t *testing.T) {
	sz, err := parseSizes(dumpedScalable)
	if err != nil {
		t.Fatal(err)
	}
	// Stopped: what's requested is what it will have.
	if sz.vcpus != 3 || sz.maxVCPUs != 4 || sz.memMiB != 3072 || sz.maxMemMiB != 5120 || sz.baseMiB != 1024 {
		t.Errorf("inactive sizes = %+v", sz)
	}
	sz, err = parseSizes(liveScalable)
	if err != nil {
		t.Fatal(err)
	}
	// Running: what's plugged so far.
	if sz.memMiB != 2048 || sz.requestedMiB != 2048 || sz.pluggedMiB != 1024 {
		t.Errorf("live sizes = %+v", sz)
	}
	sz, err = parseSizes(dumpedDomain)
	if err != nil {
		t.Fatal(err)
	}
	if sz.vcpus != 2 || sz.maxVCPUs != 2 || sz.memMiB != 1024 || sz.maxMemMiB != 1024 || sz.hasVirtioMem {
		t.Errorf("fixed sizes = %+v", sz)
	}
	if _, err := toMiB("1", "parsecs"); err == nil {
		t.Error("an unknown unit accepted")
	}
	for _, tc := range []struct {
		v, unit string
		want    int
	}{{"1048576", "KiB", 1024}, {"1048576", "", 1024}, {"2", "GiB", 2048}, {"512", "M", 512}, {"1073741824", "b", 1024}} {
		if got, err := toMiB(tc.v, tc.unit); err != nil || got != tc.want {
			t.Errorf("toMiB(%s %s) = %d %v, want %d", tc.v, tc.unit, got, err, tc.want)
		}
	}
}

// A fixed domain made scalable and back: whatever made it scalable goes,
// everything else - the tag, the interfaces' PCI addresses - stays.
func TestHardwareXMLRoundTrip(t *testing.T) {
	up, err := hardwareXML(dumpedDomain, scalable)
	if err != nil {
		t.Fatal(err)
	}
	sz, err := parseSizes(up)
	if err != nil {
		t.Fatalf("%v\n%s", err, up)
	}
	if sz.vcpus != 2 || sz.maxVCPUs != 4 || sz.memMiB != 1024 || sz.maxMemMiB != 4096 || !sz.hasVirtioMem || sz.requestedMiB != 0 {
		t.Errorf("made scalable: %+v\n%s", sz, up)
	}
	if !strings.Contains(up, "bus='0x02' slot='0x00'") || !strings.Contains(up, `controller="ctl-1"`) {
		t.Errorf("lost the tag or an interface's address:\n%s", up)
	}
	if strings.Count(up, "<memory ") != 2 || strings.Count(up, "<vcpu ") != 1 {
		t.Errorf("a size element twice:\n%s", up)
	}

	// Its maxima changed, resized meanwhile: nominal size again, nothing
	// plugged, libvirt's <vcpus> list gone.
	again, err := hardwareXML(dumpedScalable, hypervisor.Hardware{VCPUs: 2, MemoryMiB: 2048, MaxVCPUs: 8, MaxMemoryMiB: 8192})
	if err != nil {
		t.Fatal(err)
	}
	sz, err = parseSizes(again)
	if err != nil {
		t.Fatalf("%v\n%s", err, again)
	}
	if sz.vcpus != 2 || sz.maxVCPUs != 8 || sz.memMiB != 2048 || sz.maxMemMiB != 8192 || sz.requestedMiB != 0 {
		t.Errorf("new maxima: %+v\n%s", sz, again)
	}
	if strings.Contains(again, "<vcpus>") || strings.Count(again, "<numa>") != 1 || strings.Count(again, "virtio-mem") != 1 || strings.Contains(again, "migratable='on'>\n  </cpu>") {
		t.Errorf("leftovers:\n%s", again)
	}
	if !strings.Contains(again, "<cpu mode='host-passthrough' check='none' migratable='on'>") {
		t.Errorf("the <cpu> element's attributes were lost:\n%s", again)
	}

	down, err := hardwareXML(dumpedScalable, hypervisor.Hardware{VCPUs: 4, MemoryMiB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"maxMemory", "<vcpus>", "<numa>", "virtio-mem", "current="} {
		if strings.Contains(down, gone) {
			t.Errorf("fixed again, still has %s:\n%s", gone, down)
		}
	}
	sz, err = parseSizes(down)
	if err != nil {
		t.Fatalf("%v\n%s", err, down)
	}
	if sz.vcpus != 4 || sz.maxVCPUs != 4 || sz.memMiB != 2048 || sz.maxMemMiB != 2048 {
		t.Errorf("fixed again: %+v", sz)
	}
}

func TestVirtioMemRequestXML(t *testing.T) {
	dev, err := virtioMemRequestXML(liveScalable, 3072)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dev, "<memory model='virtio-mem'>") || !strings.HasSuffix(dev, "</memory>") ||
		!strings.Contains(dev, "<requested unit='KiB'>3145728</requested>") || !strings.Contains(dev, "bus='0x07'") {
		t.Errorf("device:\n%s", dev)
	}
	if _, err := virtioMemRequestXML(dumpedDomain, 1024); err == nil {
		t.Error("a domain without virtio-mem accepted")
	}
}

func TestCheckHardware(t *testing.T) {
	for _, tc := range []struct {
		hw hypervisor.Hardware
		ok bool
	}{
		{hypervisor.Hardware{VCPUs: 2, MemoryMiB: 1024}, true},
		{scalable, true},
		{hypervisor.Hardware{VCPUs: 1, MemoryMiB: 1000, MaxMemoryMiB: 2001}, false},
		{hypervisor.Hardware{VCPUs: 1, MemoryMiB: 1024, MaxVCPUs: 65}, false},
		{hypervisor.Hardware{MemoryMiB: 1024}, false},
	} {
		if err := checkHardware(tc.hw); (err == nil) != tc.ok {
			t.Errorf("checkHardware(%+v) = %v", tc.hw, err)
		}
	}
}
