# Images and formats

Every release publishes the same Janus disk in the formats private
clouds import, an installer for machines without a hypervisor, and the
update bundle nodes upgrade from. An image is generic: nothing in it is
specific to one node - its identity is made on its first boot, its
configuration given at boot or through its API afterwards
([first boot](first-boot.md)).

## What a release publishes

| File | For | Notes |
|---|---|---|
| `janus.qcow2` | Proxmox VE | Imported with `qm importdisk`, or by the Controller |
| `janus-kvm.qcow2` | libvirt/KVM, any QEMU host | What the Controller uses on a libvirt host; `virt-install --import` |
| `janus.vmdk` | VMware vSphere/ESXi, Workstation, Fusion | `streamOptimized`; no OVA yet ([VMware](platforms/vmware.md)) |
| `janus.iso` | Bare metal; a VM installed from a medium | An installer, with the release's signed bundle inside: written to a USB stick, presented as a disk by a BMC, or attached to a VM as a disk - never as a CD/DVD ([bare metal](platforms/bare-metal.md)) |
| `pi4-disk.img`, `pi5-disk.img` | Raspberry Pi 4 and 5 | For testing only ([hardware tests](../raspberry-pi-testing.md)) |
| `rootfs.squashfs`, `rootfs.squashfs.sha256`, `rootfs.verity`, `uki-a.efi`, `uki-b.efi` | Updates and installs | The update bundle: its UKIs are signed with the release key, which pins the root filesystem's dm-verity hash |

Releases are on [GitHub](https://github.com/swenske/Janus/releases);
`latest` is the newest:

```sh
v=$(curl -fsSL https://api.github.com/repos/swenske/Janus/releases/latest | python3 -c 'import json, sys; print(json.load(sys.stdin)["tag_name"])')
curl -fsSLO "https://github.com/swenske/Janus/releases/download/$v/janus-kvm.qcow2"
```

**A raw disk**: the qcow2 converted is the disk image the boot tests
boot - `qemu-img convert -O raw janus-kvm.qcow2 janus.raw` - for a
platform that only takes raw disks.

## Checking what you downloaded

- **The images**: GitHub computes each release file's SHA-256 and shows
  it with the file; the API gives it too, and the Controller checks the
  images it uses against it:

  ```sh
  curl -fsSL "https://api.github.com/repos/swenske/Janus/releases/tags/$v" \
    | python3 -c 'import json, sys; [print(a["digest"], a["name"]) for a in json.load(sys.stdin)["assets"]]'
  sha256sum janus-kvm.qcow2
  ```

- **The update bundle** is checked by the node itself before it's
  installed: its UKIs must be signed with the Janus release key
  ([`image/secureboot/production-cert.pem`](../../image/secureboot/production-cert.pem)),
  and the root filesystem must match the dm-verity hash their signed
  command line carries - one changed byte and the node refuses it.
- **An image from the image factory** comes with a manifest listing
  each file's SHA-256.

## Extensions: one image per schematic

A node's optional features - VRRP, BGP, the firewall, Consul, Let's
Encrypt, node_exporter, the QEMU guest agent - are **extensions**, built
into its read-only image: an image's set of extensions is its
**schematic**, and a node keeps its schematic through its updates. The
release's images have none; the image factory builds the others, on
demand:

- the [image builder](https://janus.sw-servers.net/builder) gives a
  schematic's images for each platform, and its ID;
- the Controller and Terraform ask the factory themselves: a node
  created with `extensions = ["keepalived"]` boots that image, and its
  updates are built from the same schematic.

[Images and extensions](../image-factory.md) lists the extensions and
how the factory builds an image.

## What a node needs

- **UEFI firmware** - OVMF on QEMU, Proxmox's `ovmf` BIOS, VMware's EFI.
  No legacy BIOS boot. Secure Boot off for the VM images (their boot
  entries aren't signed); every update a node installs is signed and
  checked anyway.
- **One disk** (virtio, SCSI, SATA, NVMe): the image's own layout - an
  EFI partition, two system slots for A/B updates, and STATE, a small
  partition keeping the node's identity and configuration. The images
  are a few hundred megabytes and need no more: there's nothing to
  install on a node. The installer gives STATE the rest of the disk it
  installs.
- **Memory and CPU**: 512 MiB and one vCPU boot a test node; size a
  production one by its HAProxy load - two vCPUs and 2 GiB serve a lot
  of traffic. Without `maxconn` in its configuration, HAProxy sizes its
  tables for about 262000 connections (around 50 MB of memory): see
  [connections and memory](../haproxy-config.md#connections-and-memory).
- **A serial port** is worth adding: the node's console - its first
  boot's credentials, its registration's errors - is on both the screen
  and the serial port, and the serial one is easier to read and copy
  from.
- **Network interfaces**: virtio, VMware's vmxnet3, Intel's and
  Broadcom's common ones, Mellanox ConnectX-4 and later - none needing
  a firmware file ([physical hardware](../provisioning-a-node.md#physical-hardware)).
