# image/kvm-proxmox

`assemble.sh` converts `image/disk/assemble.sh`'s real, single-disk GPT
image (ESP + both A/B slots + STATE) to qcow2 via `qemu-img convert -c`
- Proxmox's own preferred import/storage format. `make proxmox-image`
builds one end to end; `image-build.yml`'s "Build an alpha Proxmox VM
image (qcow2)" step does the same on `janus-runner01` and uploads
it as a workflow artifact.

## Importing into Proxmox

How to run it - from the Controller, with Terraform, or by hand with
`qm create`/`qm importdisk` - is the [Proxmox VE
guide](../../docs/private-cloud/platforms/proxmox.md): OVMF with
`pre-enrolled-keys=0` (Secure Boot off - Proxmox's default vars enroll
Microsoft's keys), a virtio disk, a serial console to read the
first-boot credentials from, a NoCloud volume for the node's network
and Controller.
