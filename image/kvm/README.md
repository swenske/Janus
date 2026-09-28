# image/kvm

`assemble.sh` converts `image/disk/assemble.sh`'s real, single-disk GPT
image (ESP + both A/B slots + STATE) to qcow2 via `qemu-img convert -c`
- the same conversion `image/kvm-proxmox/assemble.sh` does, kept as a
separate artifact because the target is generic libvirt/KVM
(`virt-install`/`virt-manager`), not Proxmox's own `qm` import flow.
`make kvm-image` builds one end to end.

## Importing via libvirt/virt-install

Unsigned (no Secure Boot cert of this project's is enrolled in a
generic libvirt host's OVMF by default) and has no VGA/framebuffer
console - only a serial one (`console=ttyS0` baked into the UKI
cmdline). Both matter for how the domain is defined:

```sh
virt-install \
  --name janus-alpha --memory 512 --vcpus 1 \
  --import --disk path=janus-kvm.qcow2,bus=virtio \
  --network network=default,model=virtio \
  --boot uefi \
  --graphics none --console pty,target_type=serial \
  --noautoconsole
virsh console janus-alpha
```

`--boot uefi` is what selects OVMF without Secure Boot enrolled by
default (a generic libvirt/OVMF install has no keys enrolled at all,
unlike Proxmox's own default vars, which enroll Microsoft's - so
nothing extra is needed here to leave Secure Boot off). `--graphics
none --console pty,target_type=serial` is what makes `virsh console`
show the actual boot output - without it, the default graphical
console stays blank (this rootfs has no VGA console driver compiled in
at all, by design).

First boot bootstraps a CA and prints the admin gRPC client cert/key to
that console **once** - see `cmd/janusd/main.go` - copy it out
immediately, there's no shell to retrieve it later. From there,
`janusctl pki generate-client-config` (or the printed cert/key
directly) drives everything else - see the root `CLAUDE.md`/`docs/
api-routes.md` for the full gRPC surface.
