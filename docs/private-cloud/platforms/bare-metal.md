# Bare metal, end to end

Janus on physical machines: booted once from the installer - a USB
stick, or a BMC's virtual disk - installed onto the machine's disk with
its network and its Controller, then admitted on an enrollment token
with no approval to click through. The same steps install a virtual
machine from the ISO.

> [!NOTE]
> Proven, under QEMU with UEFI and SELinux enforcing:
> `make qemu-iso-install-test` installs a disk from the ISO's own bundle,
> and `make qemu-baremetal-test` boots NVMe, SATA and USB disks with
> Intel network cards, and the installer from a USB stick installing a
> disk that then registers itself.

## What you need

- **The machines**: UEFI firmware, a disk (SATA, SAS, NVMe, USB), a
  supported network card - Intel e1000/e1000e/igb/igc/ixgbe/i40e,
  Realtek r8169, Broadcom tg3/bnxt, Mellanox ConnectX-4 and later; none
  that needs a firmware file ([physical
  hardware](../../provisioning-a-node.md#physical-hardware)). Secure
  Boot off, or the Janus release certificate enrolled.
- **The Controller** (the [libvirt guide](kvm-libvirt.md#1-the-controller)'s
  step 1), and `janusctl` on your workstation.
- **The release's ISO**, `janus.iso` - or one with extensions from the
  [image builder](https://janus.sw-servers.net/builder).
- **Per machine**: its address on the network, and the target disk's
  name.

## 1. An enrollment token

On the Controller's **Nodes** page, an admin makes an **enrollment
token** under **Provision new nodes**: a name, how many machines (the
rack's), how long it's valid, and labels (`rack=r12`) the nodes get. It's
shown once. A machine installed with it is admitted on its first boot,
without approval; past its uses or its date, a machine waits for
approval like any other.

From the same panel: the Controller's address and certificate
(`controller-ca.crt`).

## 2. The machine's network

What the installed node configures from its first boot - a static
address, since the kernel's DHCP lease is never renewed
([network configuration](../../network-configuration.md)):

```json title="examples/network/two-interfaces.json"
{
  "hostname": "lb1",
  "interfaces": [
    {"name": "mgmt", "mac": "52:54:00:00:10:21", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["10.0.0.21/24"]},
    {"name": "front", "mac": "52:54:00:00:20:21", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.0.2.21/24"], "gateway": "192.0.2.1"}
  ],
  "dns": {"servers": ["10.0.0.53"], "search": ["example.net"]},
  "ntp": {"servers": ["10.0.0.1"]}
}
```

The MAC addresses are the machine's own: they name the interfaces
whatever order the kernel finds them in.

## 3. Boot the installer

- **A USB stick**: `dd if=janus.iso of=/dev/sdX bs=4M conv=fsync`, then
  boot the machine from it, in UEFI mode.
- **A BMC** (iDRAC, iLO...): attach the ISO as a **removable disk**
  ("USB key"), not as a CD/DVD - an optical drive doesn't expose the
  partition the installer runs from.

The installer is a temporary Janus with the release's signed bundle on
the stick. It gets an address by DHCP and prints, on its console - the
screen and the serial port - its address and a temporary admin
certificate. Copy them (`ca.crt`, `admin.crt`, `admin.key`): they drive
this installation only.

## 4. Install

```sh
CTL="janusctl -endpoint <installer-address>:9505 -ca ca.crt -cert admin.crt -key admin.key"
$CTL system info                       # the disks the machine has
$CTL lifecycle install \
  -controller-address controller.example.net:8443 -controller-ca controller-ca.crt \
  -registration-token janus-enroll_... \
  -network-config lb1.json \
  -sha256 "$(curl -fsSL https://github.com/swenske/Janus/releases/download/<version>/rootfs.squashfs.sha256)" \
  /dev/nvme0n1 /etc/janus/release
```

- The disk and the bundle are the machine's paths: `/etc/janus/release`
  is the bundle on the stick. The installer refuses the disk it booted
  from, and one that already holds a Janus.
- The bundle's signature is checked whatever `-sha256` says; `-sha256`
  checks it's the release you meant.
- It ends with `[done 100%]`: the disk has both system slots, its STATE
  with the Controller, the token and the network.

## 5. First boot

Power off, remove the stick, boot from the disk. On its first boot the
node makes its own certificates - its admin credential printed once on
the console: keep it, or let it go -, applies its network, and registers
with the Controller on the token: it's admitted at once, labelled
`rack=r12`.

Then its HAProxy configuration - with `janusctl`, from your workstation
signed in to the Controller:

```sh
janusctl login -controller controller.example.net:8080 -controller-ca controller-ca.crt
janusctl -n lb1 haproxy apply-config examples/haproxy/web.cfg
```

## At scale

- **A whole rack**: the same token for every machine, each with its own
  network file; the installer can run from one stick after another, or
  from the BMCs' virtual media in parallel.
- **Network boot**: the installer boots over native PXE or UEFI HTTP
  Boot too ([network boot](../../../image/pxe/README.md)) - then the
  same `lifecycle install`.
- **Updates** reach each machine like any node: A/B, signed, with an
  automatic revert ([lifecycle](../lifecycle.md)). The installer isn't
  needed again.
