# Secure Boot

UEFI Secure Boot makes a node's firmware refuse anything not signed by a
certificate it trusts: with the Janus release certificate in its `db`,
a node boots only what a Janus release signed - its two slots' kernels,
command lines and, through the dm-verity root hash those command lines
pin, their whole root filesystems. It's off unless you turn it on, node
by node, in the virtual machine's or the server's firmware.

> [!NOTE]
> Every update a node installs is checked against the same certificate
> whether Secure Boot is on or not
> ([trusted boot](../architecture.md#trusted-boot)). Secure Boot adds the
> firmware's own check at each boot: a disk changed while the node was
> off - its ESP rewritten, an unsigned kernel put in place - doesn't
> boot.

## What Janus signs

- **The update bundles** - `uki-a.efi` and `uki-b.efi` of every release
  and of every image the factory builds: what an update writes to a
  node's ESP.
- **The images** - the Proxmox and KVM qcow2s, the VMDK and the
  installer ISO, from the releases published after v2026.10.30, the
  factory's images of those releases too: the UKIs on their ESP
  (`\EFI\BOOT\BOOTX64.EFI`, `\JANUS\UKI-A.EFI`, `\JANUS\UKI-B.EFI`).

The certificate is
[`image/secureboot/production-cert.pem`](../../image/secureboot/production-cert.pem):
`CN=Janus Secure Boot signing key`, RSA 4096, valid until 2046-09-25,
SHA-256 fingerprint
`D0:D8:91:EF:80:F1:D2:B8:84:F0:C3:2F:EE:30:AB:4D:7F:7D:EA:1F:A9:F9:38:32:C4:4D:63:E7:25:1D:FF:0F`.
Microsoft doesn't sign Janus, so a firmware with only its factory keys
refuses it: the certificate goes into the firmware's `db` - and, on a
machine that boots nothing else, as its `PK` and `KEK` too, so that
nothing else boots.

A signed image boots the same with Secure Boot off: the firmware
doesn't look at the signature then.

## Before turning it on

Both slots must hold signed UKIs: a revert or a rollback boots the other
one, and a firmware that refuses it leaves the node down. That's the
case of a node installed from an image published after v2026.10.30, and
of any node updated twice since it was installed - each update writes a
release's signed UKI into one slot (unless it was installed with
`insecure_skip_signature_check`, for development bundles). `janusctl version` shows what each
slot holds:

```console
$ janusctl -n edge-1 version
...
Active slot: A
Other slot: B - v2026.10.29-6, schematic a51811d8, HAProxy 3.4.7, kernel stable 7.2.13
```

To check the UKIs themselves, read the ESP from the hypervisor - the
disk's first 66 MiB, partition 1 at 1 MiB - and verify each one
(`mtools`, `sbverify` from `sbsigntool`):

```sh
dd if="$(pvesm path local-lvm:vm-101-disk-1)" of=esp.raw bs=1M count=66
for f in /EFI/BOOT/BOOTX64.EFI /JANUS/UKI-A.EFI /JANUS/UKI-B.EFI; do
  mcopy -n -i esp.raw@@1048576 "::$f" uki.efi
  sbverify --cert production-cert.pem uki.efi
done
```

## On Proxmox VE

A Janus virtual machine on Proxmox VE has an EFI disk (`efidisk0`,
`efitype=4m`) created with `pre-enrolled-keys=0`: no key, Secure Boot
off. Proxmox's firmware is Secure Boot-capable; turning it on means
writing the certificate into the virtual machine's own EFI variables,
on the host, while the machine is off. Proven on Proxmox VE 9.2 (edk2
2026.08) on four nodes; `virt-fw-vars` comes with
`python3-virt-firmware`, installed with Proxmox VE 9.

1. Stop the node cleanly - in a cluster, drain it first (Clusters ›
   member › Drain, or `POST /api/clusters/{id}/members/{name}/drain`):

   ```sh
   janusctl -n edge-1 system shutdown
   ```

2. On the host, once `qm status` says `stopped`: the variables are the
   EFI disk's first 540672 bytes. Keep a copy - going back is writing
   it again - and write the new ones over them:

   ```sh
   efi="$(pvesm path "$(qm config 101 | sed -n 's/^efidisk0: \([^,]*\),.*/\1/p')")"
   head -c 540672 "$efi" > vm-101-vars.orig.fd
   guid="$(cat /proc/sys/kernel/random/uuid)"
   virt-fw-vars -i vm-101-vars.orig.fd \
     --set-pk "$guid" production-cert.pem \
     --add-kek "$guid" production-cert.pem \
     --add-db "$guid" production-cert.pem \
     --sb -o vm-101-vars.sb.fd
   dd if=vm-101-vars.sb.fd of="$efi" conv=notrunc,fsync
   ```

3. Start it (`qm start 101`, or the Controller's Start), check that the
   kernel says so, and undrain it:

   ```console
   $ janusctl -n edge-1 system dmesg | grep -i "secure boot"
   [    0.003481] Secure boot enabled
   ```

In a cluster, one member at a time: the drain moves its virtual IPs or
withdraws its routes first and is kept across the reboot. Right after
the boot, the Controller may still hold its connection from before and
fail the undrain once - run it again.

Proxmox VE doesn't lock the firmware's flash to SMM (QEMU's
`cfi.pflash01` `secure` property, which libvirt sets): the variables
are protected from the network and from a disk changed offline, not
from code running in the node's kernel.

## On libvirt

The same certificate in the variable store libvirt keeps for the
machine (`image/secureboot/enroll-vars.sh OUT.fd production-cert.pem
TEMPLATE` writes it from an OVMF variables template), with the Secure
Boot firmware: `<loader readonly='yes' secure='yes' type='pflash'>`
with `OVMF_CODE_4M.secboot.fd`, `<nvram>` pointing at that file, and
`<smm state='on'/>` - the QEMU options `hack/qemu-secureboot-test.sh`
boots a signed image with on every image build. Not for the Controller's own libvirt machines: they
run without a variable store
([hypervisors](../hypervisors.md)). With `secure='yes'` under OVMF
2025.02 (Debian 13's `ovmf` package), a reboot with kexec fails and
the node boots through the firmware instead
([updating nodes](updates.md#with-janusctl)).

## Where it shows

- **`janusctl version`** - `Secure Boot: on`, `off`, or `none - not a
  UEFI boot`, from the node's own UEFI variables (a node of a release
  up to v2026.10.30 doesn't say).
- **The Controller** - Hypervisors › Machines: a **Secure Boot** badge
  with a shield on a machine whose firmware enforces it, a crossed-out
  shield when it's off; `secure_boot` in `GET /api/machines/{id}`
  ([the API](../hypervisors.md#the-api)).
- **The kernel's own line**, on every release:
  `janusctl system dmesg | grep -i "secure boot"`.

## What changes on the node

- **Updates** - nothing: a release is checked the same way before it's
  written. With kexec, only a UKI signed by Janus is jumped into; janusd
  says so (`kexec: ... (Secure Boot enforced, UKI signature verified)`).
- **Reverts and rollbacks** - the other slot boots through the firmware,
  which checks it like the first.
- **A machine made again** - the Controller creates its machines with
  Secure Boot off: a member it recycles or replaces comes back without
  it; turn it on again the same way.
