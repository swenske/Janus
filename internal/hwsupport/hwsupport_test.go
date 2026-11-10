package hwsupport

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fakeSysfs lays out a sysfs tree the way the kernel links it: devices
// under devices/, drivers under bus/<bus>/drivers/, class entries and
// block/ as symlinks into devices/.
type fakeSysfs struct {
	t    *testing.T
	root string
}

func newFakeSysfs(t *testing.T) *fakeSysfs {
	root := t.TempDir()
	for _, d := range []string{"devices", "block", "class/net"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &fakeSysfs{t: t, root: root}
}

// device makes devices/<path>, bound to driver on bus when driver isn't "".
func (f *fakeSysfs) device(path, bus, driver string) {
	dir := filepath.Join(f.root, "devices", path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if driver == "" {
		return
	}
	drv := filepath.Join(f.root, "bus", bus, "drivers", driver)
	if err := os.MkdirAll(drv, 0o755); err != nil {
		f.t.Fatal(err)
	}
	rel, _ := filepath.Rel(dir, drv)
	if err := os.Symlink(rel, filepath.Join(dir, "driver")); err != nil {
		f.t.Fatal(err)
	}
}

// link makes <from> (block/vda, class/net/eth0/device) point at devices/<path>.
func (f *fakeSysfs) link(from, path string) {
	at := filepath.Join(f.root, from)
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		f.t.Fatal(err)
	}
	// Relative to where the link really lands: class/net/eth0/device is
	// written inside the directory class/net/eth0 points to.
	dir, err := filepath.EvalSymlinks(filepath.Dir(at))
	if err != nil {
		f.t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(filepath.Join(f.root, "devices"))
	if err != nil {
		f.t.Fatal(err)
	}
	rel, _ := filepath.Rel(dir, filepath.Join(target, path))
	if err := os.Symlink(rel, filepath.Join(dir, filepath.Base(at))); err != nil {
		f.t.Fatal(err)
	}
}

// netdev makes a network interface: class/net/<name>, with a device
// behind it when dev isn't "".
func (f *fakeSysfs) netdev(name, dev string) {
	if dev == "" {
		f.device("virtual/net/"+name, "", "")
		f.link("class/net/"+name, "virtual/net/"+name)
		return
	}
	f.device(dev+"/net/"+name, "", "")
	f.link("class/net/"+name, dev+"/net/"+name)
	f.link("class/net/"+name+"/device", dev)
}

func TestInUse(t *testing.T) {
	cases := []struct {
		name  string
		build func(f *fakeSysfs)
		disk  string
		want  []Use
	}{{
		name: "virtio-blk and virtio-net (libvirt, the tests)",
		build: func(f *fakeSysfs) {
			f.device("pci0000:00", "", "")
			f.device("pci0000:00/0000:00:04.0", "pci", "virtio-pci")
			f.device("pci0000:00/0000:00:04.0/virtio1", "virtio", "virtio_blk")
			f.device("pci0000:00/0000:00:04.0/virtio1/block/vda", "", "")
			f.link("block/vda", "pci0000:00/0000:00:04.0/virtio1/block/vda")
			f.device("pci0000:00/0000:00:03.0", "pci", "virtio-pci")
			f.device("pci0000:00/0000:00:03.0/virtio0", "virtio", "virtio_net")
			f.netdev("eth0", "pci0000:00/0000:00:03.0/virtio0")
			f.netdev("lo", "")
			f.netdev("eth0.20", "")
		},
		disk: "/dev/vda",
		want: []Use{
			{"virtio_blk", "boot disk vda"}, {"virtio-pci", "boot disk vda"},
			{"virtio_net", "eth0"}, {"virtio-pci", "eth0"},
		},
	}, {
		name: "virtio-scsi behind a PCIe root port, e1000e (Proxmox q35)",
		build: func(f *fakeSysfs) {
			base := "pci0000:00/0000:00:1c.0"
			f.device("pci0000:00", "", "")
			f.device(base, "pci_express", "pcieport")
			f.device(base+"/0000:01:00.0", "pci", "virtio-pci")
			f.device(base+"/0000:01:00.0/virtio2", "virtio", "virtio_scsi")
			f.device(base+"/0000:01:00.0/virtio2/host0/target0:0:0/0:0:0:0", "scsi", "sd")
			f.device(base+"/0000:01:00.0/virtio2/host0/target0:0:0/0:0:0:0/block/sda", "", "")
			f.link("block/sda", base+"/0000:01:00.0/virtio2/host0/target0:0:0/0:0:0:0/block/sda")
			f.device("pci0000:00/0000:00:03.0", "pci", "e1000e")
			f.netdev("lan0", "pci0000:00/0000:00:03.0")
		},
		disk: "sda",
		want: []Use{
			{"sd", "boot disk sda"}, {"virtio_scsi", "boot disk sda"}, {"virtio-pci", "boot disk sda"}, {"pcieport", "boot disk sda"},
			{"e1000e", "lan0"},
		},
	}, {
		name: "NVMe and a physical NIC (bare metal)",
		build: func(f *fakeSysfs) {
			f.device("pci0000:00/0000:00:01.1", "pci_express", "pcieport")
			f.device("pci0000:00/0000:00:01.1/0000:02:00.0", "pci", "nvme")
			f.device("pci0000:00/0000:00:01.1/0000:02:00.0/nvme/nvme0/nvme0n1", "", "")
			f.link("block/nvme0n1", "pci0000:00/0000:00:01.1/0000:02:00.0/nvme/nvme0/nvme0n1")
			f.device("pci0000:00/0000:00:01.2/0000:03:00.0", "pci", "igb")
			f.netdev("eno1", "pci0000:00/0000:00:01.2/0000:03:00.0")
		},
		disk: "/dev/nvme0n1",
		want: []Use{{"nvme", "boot disk nvme0n1"}, {"pcieport", "boot disk nvme0n1"}, {"igb", "eno1"}},
	}, {
		name: "no disk asked: the network interfaces alone",
		build: func(f *fakeSysfs) {
			f.device("pci0000:00/0000:00:03.0", "pci", "e1000")
			f.netdev("eth0", "pci0000:00/0000:00:03.0")
		},
		want: []Use{{"e1000", "eth0"}},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeSysfs(t)
			c.build(f)
			got, err := InUse(f.root, c.disk)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("InUse:\n got %v\nwant %v", got, c.want)
			}
		})
	}
}

func TestInUseRefusesAnUnknownDisk(t *testing.T) {
	f := newFakeSysfs(t)
	if _, err := InUse(f.root, "/dev/vdz"); err == nil {
		t.Fatal("no error for a disk sysfs doesn't have")
	}
	// A disk with no driver anywhere on its path: nothing to judge with.
	f.device("virtual/block/loop0", "", "")
	f.link("block/loop0", "virtual/block/loop0")
	if _, err := InUse(f.root, "loop0"); err == nil {
		t.Fatal("no error for a disk with no driver")
	}
}

func TestMissing(t *testing.T) {
	uses := []Use{{"nvme", "boot disk nvme0n1"}, {"pcieport", "boot disk nvme0n1"}, {"virtio_net", "eth0"}, {"igb", "eth1"}}
	drivers := []string{"virtio-pci", "virtio_blk", "virtio_net", "pcieport"}
	got := Missing(drivers, uses)
	want := []Use{{"nvme", "boot disk nvme0n1"}, {"igb", "eth1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Missing: got %v, want %v", got, want)
	}
	if got := Describe(got); got != "nvme (boot disk nvme0n1), igb (eth1)" {
		t.Errorf("Describe: %q", got)
	}
	if got := Summary(uses); got != "boot disk nvme0n1: nvme, pcieport; eth0: virtio_net; eth1: igb" {
		t.Errorf("Summary: %q", got)
	}
	if got := Missing(nil, uses); got != nil {
		t.Errorf("an image that names no drivers: got %v, want nothing missing", got)
	}
}
