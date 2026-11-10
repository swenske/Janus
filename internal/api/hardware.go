package api

import (
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/hwsupport"
	"github.com/swenske/Janus/internal/schematic"
)

// sysfsRoot is where checkHardware reads which drivers the node's devices
// are bound to (tests lay out a tree of their own).
var sysfsRoot = "/sys"

// checkHardware refuses an image whose kernel has no driver for disk - the
// boot disk for Upgrade, the target disk for Install - or for one of the
// node's network interfaces (internal/hwsupport), unless src allows it.
// An image whose kernel names no drivers (an older one, or one for every
// machine) isn't checked, nor anything when janusd doesn't run a node:
// "" and no error. Otherwise the message says what was found.
func (l *Lifecycle) checkHardware(src *janusv1alpha1.ImageSource, image *schematic.ImageInfo, disk string) (string, error) {
	if !l.CheckHardware || image == nil || len(image.Kernel.Drivers) == 0 {
		return "", nil
	}
	kernel := fmt.Sprintf("its kernel (%s)", image.Kernel.Flavor)
	allowed := src.GetAllowUnsupportedHardware()
	uses, err := hwsupport.InUse(sysfsRoot, disk)
	if err != nil {
		if allowed {
			return fmt.Sprintf("can't tell whether %s drives this machine (%v) - installed all the same (allow_unsupported_hardware)", kernel, err), nil
		}
		return "", status.Errorf(codes.FailedPrecondition,
			"can't tell whether this release's kernel (%s) drives this machine: %v - janusctl's -allow-unsupported-hardware installs it all the same (allow_unsupported_hardware; the Controller's Update page doesn't offer it)", image.Kernel.Flavor, err)
	}
	missing := hwsupport.Missing(image.Kernel.Drivers, uses)
	switch {
	case len(missing) == 0:
		return kernel + " drives this machine's disk and network interfaces", nil
	case allowed:
		return fmt.Sprintf("%s has no driver for %s - installed all the same (allow_unsupported_hardware)", kernel, hwsupport.Describe(missing)), nil
	}
	return "", status.Errorf(codes.FailedPrecondition,
		"this release's kernel (%s) has no driver for %s: %s. Only if those devices change before the node boots it, janusctl's -allow-unsupported-hardware installs it all the same (allow_unsupported_hardware; the Controller's Update page doesn't offer it)",
		image.Kernel.Flavor, hwsupport.Describe(missing), consequence(missing))
}

// consequence says what booting a kernel without the missing drivers
// would do.
func consequence(missing []hwsupport.Use) string {
	for _, u := range missing {
		if strings.HasPrefix(u.Device, "boot disk ") {
			return "it would never find its disk and wait for it forever - nothing on the node left to roll it back, only its console"
		}
	}
	return "the node would lose those network interfaces"
}
