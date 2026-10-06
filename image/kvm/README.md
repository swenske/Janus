# image/kvm

`assemble.sh` converts `image/disk/assemble.sh`'s real, single-disk GPT
image (ESP + both A/B slots + STATE) to qcow2 via `qemu-img convert -c`
- the same conversion `image/kvm-proxmox/assemble.sh` does, kept as a
separate artifact because the target is generic libvirt/KVM
(`virt-install`/`virt-manager`), not Proxmox's own `qm` import flow.
`make kvm-image` builds one end to end.

## Importing via libvirt/virt-install

How to run it - from the Controller, with Terraform, or by hand with
`virt-install --import --boot uefi` - is the [libvirt/KVM
guide](../../docs/private-cloud/platforms/kvm-libvirt.md): UEFI without
Secure Boot, a virtio disk, a serial console to read the first-boot
credentials from, a NoCloud volume for the node's network and
Controller.
