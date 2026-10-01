# image/kvm-proxmox

`assemble.sh` converts `image/disk/assemble.sh`'s real, single-disk GPT
image (ESP + both A/B slots + STATE) to qcow2 via `qemu-img convert -c`
- Proxmox's own preferred import/storage format. `make proxmox-image`
builds one end to end; `image-build.yml`'s "Build an alpha Proxmox VM
image (qcow2)" step does the same on `janus-runner01` and uploads
it as a workflow artifact.

## Importing into Proxmox

The image is unsigned (no Secure Boot cert of this project's is
enrolled in Proxmox's own OVMF by default), which matters for how the VM
is created:

```sh
qm create <vmid> --name janus-alpha --memory 512 --cores 1 \
  --machine q35 --bios ovmf --efidisk0 <storage>:0,pre-enrolled-keys=0 \
  --net0 virtio,bridge=<bridge> \
  --serial0 socket

qm importdisk <vmid> janus.qcow2 <storage>
qm set <vmid> --scsihw virtio-scsi-pci --virtio0 <storage>:vm-<vmid>-disk-1
qm set <vmid> --boot order=virtio0

qm start <vmid>
qm terminal <vmid>   # the serial console
```

`pre-enrolled-keys=0` on the EFI disk is what leaves Secure Boot off
(Proxmox's own default OVMF vars otherwise enroll Microsoft's keys,
which don't match this project's own signing key anyway).

The node shows its console in two places: on the screen (the UEFI
framebuffer - Proxmox's noVNC console) and on the serial port
(`console=ttyS0`, which `--serial0 socket` connects to `qm terminal`).
Both show the kernel's messages, the banner and the first-boot
credentials. Keep `--serial0 socket` anyway: those credentials are
easier to copy from a text console than from a screen. Adding `--vga
serial0` makes the GUI's console the serial port instead of the screen.
Images from before v2026.10.01-3 have no screen console at all: they
need `--vga serial0`, or the GUI's console stays blank.

First boot bootstraps a CA and prints the admin gRPC client cert/key to
that console **once** - see `cmd/janusd/main.go` - copy it out
immediately, there's no shell to retrieve it later. From there,
`janusctl pki generate-client-config` (or the printed cert/key
directly) drives everything else - see the root `CLAUDE.md`/`docs/
api-routes.md` for the full gRPC surface.
