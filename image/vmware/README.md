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
generic ESXi host's firmware by default) and has no VGA/framebuffer
console - only a serial one (`console=ttyS0` baked into the UKI
cmdline). Both matter for how the VM is created - e.g. via `govc`:

```sh
govc import.vmdk janus.vmdk <datastore>
govc vm.create -net.adapter=vmxnet3 -disk="<datastore>/janus.vmdk" \
  -firmware=efi -on=false janus-alpha
govc vm.change -vm janus-alpha -e efi-secure-boot-enabled=FALSE
govc vm.power -on janus-alpha
govc vm.console janus-alpha -serial file:///tmp/janus-alpha.log
```

(`vSphere Web Client` equivalent: import the VMDK as a disk, add it to
a new EFI VM with Secure Boot unchecked, and attach a serial port
redirected to a file or named pipe to get the boot console - this
rootfs has no VGA console driver compiled in at all, by design.)

First boot bootstraps a CA and prints the admin gRPC client cert/key to
that console **once** - see `cmd/janusd/main.go` - copy it out
immediately, there's no shell to retrieve it later. From there,
`janusctl pki generate-client-config` (or the printed cert/key
directly) drives everything else - see the root `CLAUDE.md`/`docs/
api-routes.md` for the full gRPC surface.
