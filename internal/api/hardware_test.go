package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/schematic"
)

// fakeMachine lays out a sysfs tree of a machine with an NVMe boot disk
// and a virtio network interface, and points checkHardware at it.
func fakeMachine(t *testing.T) {
	root := t.TempDir()
	mk := func(dir string) {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, at string) {
		mk(filepath.Dir(at))
		if err := os.Symlink(target, filepath.Join(root, at)); err != nil {
			t.Fatal(err)
		}
	}
	mk("bus/pci/drivers/nvme")
	mk("bus/pci/drivers/virtio-pci")
	mk("bus/virtio/drivers/virtio_net")
	mk("devices/pci0000:00/0000:00:04.0/nvme/nvme0/nvme0n1")
	link("../../../bus/pci/drivers/nvme", "devices/pci0000:00/0000:00:04.0/driver")
	link("../devices/pci0000:00/0000:00:04.0/nvme/nvme0/nvme0n1", "block/nvme0n1")
	mk("devices/pci0000:00/0000:00:03.0/virtio0/net/eth0")
	link("../../../bus/pci/drivers/virtio-pci", "devices/pci0000:00/0000:00:03.0/driver")
	link("../../../../bus/virtio/drivers/virtio_net", "devices/pci0000:00/0000:00:03.0/virtio0/driver")
	link("../../devices/pci0000:00/0000:00:03.0/virtio0/net/eth0", "class/net/eth0")
	link("../../../virtio0", "devices/pci0000:00/0000:00:03.0/virtio0/net/eth0/device")
	old := sysfsRoot
	sysfsRoot = root
	t.Cleanup(func() { sysfsRoot = old })
}

func TestCheckHardware(t *testing.T) {
	fakeMachine(t)
	l := &Lifecycle{CheckHardware: true}
	virt := &schematic.ImageInfo{Kernel: schematic.ImageComponent{Flavor: "virt", Drivers: []string{"virtio-pci", "virtio_blk", "virtio_net"}}}
	metal := &schematic.ImageInfo{Kernel: schematic.ImageComponent{Flavor: "metal", Drivers: []string{"virtio-pci", "virtio_net", "nvme"}}}

	// No drivers named (an older image, a kernel for every machine): not checked.
	for _, img := range []*schematic.ImageInfo{nil, {}} {
		if msg, err := l.checkHardware(&janusv1alpha1.ImageSource{}, img, "/dev/nvme0n1"); msg != "" || err != nil {
			t.Errorf("%v: %q, %v", img, msg, err)
		}
	}
	// A kernel that drives everything.
	if msg, err := l.checkHardware(&janusv1alpha1.ImageSource{}, metal, "/dev/nvme0n1"); err != nil || !strings.Contains(msg, "drives this machine") {
		t.Errorf("metal: %q, %v", msg, err)
	}
	// A kernel without the boot disk's driver: refused, saying why.
	_, err := l.checkHardware(&janusv1alpha1.ImageSource{}, virt, "/dev/nvme0n1")
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "nvme (boot disk nvme0n1)") || !strings.Contains(err.Error(), "never find its disk") {
		t.Errorf("virt on NVMe: %v", err)
	}
	// ... unless allowed.
	msg, err := l.checkHardware(&janusv1alpha1.ImageSource{AllowUnsupportedHardware: true}, virt, "/dev/nvme0n1")
	if err != nil || !strings.Contains(msg, "installed all the same") {
		t.Errorf("virt on NVMe, allowed: %q, %v", msg, err)
	}
	// Only a network interface's driver missing.
	noNet := &schematic.ImageInfo{Kernel: schematic.ImageComponent{Flavor: "x", Drivers: []string{"virtio-pci", "nvme"}}}
	if _, err := l.checkHardware(&janusv1alpha1.ImageSource{}, noNet, "/dev/nvme0n1"); err == nil || !strings.Contains(err.Error(), "lose those network interfaces") {
		t.Errorf("no virtio_net: %v", err)
	}
	// A janusd that doesn't run a node checks nothing.
	if msg, err := (&Lifecycle{}).checkHardware(&janusv1alpha1.ImageSource{}, virt, "/dev/nvme0n1"); msg != "" || err != nil {
		t.Errorf("not a node: %q, %v", msg, err)
	}
	// A disk sysfs doesn't know: can't tell, refused.
	if _, err := l.checkHardware(&janusv1alpha1.ImageSource{}, virt, "/dev/vdz"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "can't tell") {
		t.Errorf("unknown disk: %v", err)
	}
}
