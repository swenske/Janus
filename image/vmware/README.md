# image/vmware

`assemble.sh` converts `image/disk/assemble.sh`'s real, single-disk GPT
image (ESP + both A/B slots + STATE) to a `streamOptimized` VMDK via
`qemu-img convert` - VMware/ESXi's own native disk format. `make
vmware-image` builds one end to end.

Bare VMDK only for now, not a full OVA - an OVA additionally needs an
OVF XML descriptor (CPU/memory/network hardware, disk controller type)
plus a tar wrapper around it and the VMDK, which this project doesn't
generate yet. TODO(follow-up): build that descriptor (`ovftool` can
package a raw VMDK + a hand-written `.ovf` into a real `.ova`).

## Importing into ESXi / Workstation / Fusion

Unsigned (no Secure Boot cert of this project's is enrolled in a
generic ESXi host's firmware by default), which matters for how the VM
is created - e.g. via `govc`:

```sh
govc import.vmdk janus.vmdk <datastore>
govc vm.create -g otherLinux64Guest -firmware=efi \
  -disk.controller=pvscsi -disk="<datastore>/janus.vmdk" \
  -net.adapter=vmxnet3 -on=false janus-alpha
govc vm.change -vm janus-alpha -e efi-secure-boot-enabled=FALSE
govc vm.power -on janus-alpha
govc vm.console janus-alpha -serial file:///tmp/janus-alpha.log
```

The disk controller must be **VMware Paravirtual** (PVSCSI), SATA or
NVMe: the kernel has no driver for VMware's LSI Logic controllers (the
parallel and SAS ones), and govc picks LSI Logic unless told otherwise.
The network adapter is VMXNET 3 (or E1000/E1000e). The guest OS type
must be a 64-bit one.

(`vSphere Web Client` equivalent: import the VMDK as a disk, add it to
a new VM - guest OS "Other Linux (64-bit)", EFI firmware with Secure
Boot unchecked, a VMware Paravirtual SCSI controller, a VMXNET 3
adapter - and attach a serial port redirected to a file or named pipe.)

The node shows its console on the screen (the UEFI framebuffer, so the
VM's console in the vSphere client) and on the serial port
(`console=ttyS0`) - the kernel's messages, the banner and the first-boot
credentials on both; the serial port is the easier one to copy them
from. VMware's devices (PVSCSI, VMXNET 3) and the screen console are
tested under QEMU, not yet on a real ESXi host. Images from before
v2026.10.01-3 can't boot on VMware at all: their kernel had none of
these drivers.

First boot bootstraps a CA and prints the admin gRPC client cert/key to
that console **once** - see `cmd/janusd/main.go` - copy it out
immediately, there's no shell to retrieve it later. From there,
`janusctl pki generate-client-config` (or the printed cert/key
directly) drives everything else - see the root `CLAUDE.md`/`docs/
api-routes.md` for the full gRPC surface.
