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

How to run it with `govc` or the vSphere client is the [VMware
vSphere guide](../../docs/private-cloud/platforms/vmware.md): EFI with
Secure Boot off, a VMware Paravirtual (PVSCSI), SATA or NVMe disk
controller - the kernel has no driver for the LSI Logic ones - and a
VMXNET 3 adapter. VMware's devices are tested under QEMU, not yet on a
real ESXi host.
