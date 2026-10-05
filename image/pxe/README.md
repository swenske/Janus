# image/pxe

No new build artifact here - PXE/HTTP Boot just delivers a UKI
`image/uki/assemble.sh` already produces over the network instead of
from an ESP/USB/ISO. A release's own `uki-a.efi` names its root
partitions by GPT label (`PARTLABEL=BOOT-A-DATA`/`BOOT-A-HASH`), so served
as is it boots slot A of whatever Janus disk the machine has - NVMe,
SATA, virtio - with no rebuild, provided slot A holds that release's
rootfs (the UKI carries its root hash). (A node booted this way still updates
its local ESP on Upgrade/Rollback; the PXE server decides what boots.)

## Serving it - native PXE/HTTP Boot (recommended)

Almost all real bare-metal UEFI firmware supports one of these
natively - no extra software needed on the node at all, and the
firmware loads the UKI *directly* (its own boot manager calling
`LoadImage`/`StartImage` once, same as loading it from an ESP) - the
exact same well-tested code path every other boot test in this
project already relies on.

**Legacy PXE (TFTP)** - a `dnsmasq` example:

```ini
dhcp-range=192.0.2.10,192.0.2.100,12h
dhcp-boot=janus.efi
enable-tftp
tftp-root=/srv/tftp
```

Copy the UKI to `/srv/tftp/janus.efi`.

**UEFI HTTP Boot** - simpler, no TFTP at all, and what most current
firmware prefers:

```ini
dhcp-range=192.0.2.10,192.0.2.100,12h
dhcp-option=tag:efi-http,60,HTTPClient
dhcp-boot=tag:efi-http,"http://192.0.2.1:8080/janus.efi"
```

Serve `janus.efi` from any plain HTTP server at that path.

## Serving it - iPXE fallback (firmware with no native PXE/HTTP Boot)

Some firmware - including, as it turns out, this project's own dev/CI
OVMF build - has no PXE/HTTP Boot driver stack at all (confirmed via
its own UEFI Shell's `drivers` command: the NIC driver is present, but
none of `Dhcp4Dxe`/`Ip4Dxe`/`UefiPxeBcDxe`/`HttpBootDxe` are). The
standard real-world workaround is iPXE (`ipxe.efi`, e.g. Debian's
`ipxe` package): chainloaded from a small local ESP (or a real PXE ROM
that already knows how to load it), it implements its own DHCP/TFTP/
HTTP stack entirely in userspace against the platform's NIC driver, so
it works even where the firmware's own network-boot stack doesn't.

```sh
mkfs.vfat -F 32 ipxe-esp.img            # then \EFI\BOOT\BOOTX64.EFI = ipxe.efi
```

Once running, iPXE's own default behavior (no custom script needed) is
to DHCP and fetch+boot whatever `bootfile`/`next-server` the DHCP
response carries - the exact same `dnsmasq` config above works
unchanged.

### Known limitation (this project's own OVMF, not a Janus bug)

Verified with `hack/qemu-pxe-fetch-test.sh`: iPXE DHCPs and fetches the
real Janus UKI via TFTP correctly (exact byte-for-byte match). What
isn't verified in this environment: booting past that point. Executing
a Linux kernel EFI stub via a *nested* `LoadImage`/`StartImage` call -
i.e. loaded by another already-running EFI application, whether iPXE
**or the plain UEFI Shell** - hangs in this project's own OVMF build,
at or immediately after `ExitBootServices`. This was isolated with a
clean set of differential tests (same UKI boots perfectly when loaded
directly by firmware; a bare `bzImage` with no `systemd-stub` wrapper
hangs identically; chaining iPXE to *itself* via the identical
mechanism works fine; the plain UEFI Shell manually executing the same
kernel - zero iPXE, zero network - hangs identically) - ruling out
iPXE, `systemd-stub`, and cmdline complexity as the cause. A patch to
iPXE's own EFI image loader (calling `shutdown_boot()` before
`StartImage`, mirroring its legacy BIOS kernel-boot path) was tried and
did not change the outcome, confirming the fault isn't in iPXE at all.

This only affects firmware relying on the iPXE fallback. Real hardware
with native PXE/HTTP Boot loads the UKI directly (never nested) and
isn't exposed to this at all.
