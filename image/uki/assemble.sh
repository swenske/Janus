#!/usr/bin/env bash
# Assembles a real Unified Kernel Image (UKI) - the kernel plus its
# exact boot command line, bundled into one PE/COFF executable UEFI
# firmware can load and run directly, no separate bootloader, no
# initramfs, no interactive EFI shell needed to pass arguments. Uses
# `ukify` (systemd-ukify) rather than hand-assembling one with objcopy:
# the vanilla kernel's own EFI stub only ever reads its command line
# from the EFI LoadOptions the firmware passes when launching an image
# interactively (see Documentation/admin-guide/efi-stub.rst in the
# kernel source and drivers/firmware/efi/libstub/efi-stub-helper.c's
# efi_convert_cmdline) - it does NOT look for a `.cmdline` PE section on
# its own. `ukify`'s stub (systemd-stub, linuxx64.efi.stub) does, and
# then chain-loads into the kernel's own embedded EFI stub via the EFI
# handover protocol (CONFIG_EFI_HANDOVER_PROTOCOL) - which is exactly
# why the kernel needs CONFIG_EFI_STUB itself too, not just systemd's
# stub: the code receiving that handover call lives in the kernel.
#
# Booting this with no drive attached at the STATE partition device
# (see rootfs/init/main.go's mountState) is fine - it falls back to the
# ephemeral tmpfs for PKI/config the same way it always has; this
# script only cares about root=, not STATE.
#
# Usage: image/uki/assemble.sh <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]
# <rootfs-dir> must contain rootfs.verity.info/rootfs.roothash (see
# rootfs/assemble.sh and hack/dm-verity-cmdline.sh). <data-device>/
# <hash-device> are the two virtio-blk devices root will actually be
# attached as at boot (e.g. /dev/vdb /dev/vdc, if an ESP is vda) -
# baked into the UKI's cmdline permanently, since there's no way to
# override it after the fact without reassembling.
#
# [signing-key]/[signing-cert] are optional - given both, the UKI is
# Secure Boot-signed (`ukify` shells out to `sbsign`) with them; given
# neither (the default, used by every caller except
# hack/qemu-secureboot-test.sh), the UKI is unsigned, exactly as
# before - Secure Boot enforcement is opt-in at the firmware/vars level
# (see image/secureboot/), not something every other boot test in this
# project needs to care about.
#
# UKIFY_STUB (env var, optional): explicit path to the sd-stub PE
# binary `ukify` should embed. Unset, the stub for the kernel's
# architecture (an arm64 Image carries "ARMd" at offset 56, anything
# else is taken as x86) comes from systemd-stub/Dockerfile's pinned
# Debian image, built into build/systemd-stub/ the first time - never
# the build host's own systemd-boot-efi, whose version nobody tracks.
#
# UKI_CONSOLE (env var, optional, default ttyS0): the serial console
# name baked into the cmdline - x86 targets (QEMU's isa-serial/OVMF)
# use ttyS0, but the Single Board Computer tranche's aarch64 targets
# (QEMU's raspi4b/virt machines, PL011 UART) need ttyAMA0 instead - a
# genuinely different device name, not a default every caller can share.
#
# UKI_SELINUX_ENFORCING (env var, optional, default 1): whether
# `enforcing=1` is appended - left off for the Single Board Computer
# tranche's aarch64 targets, whose kernel config has no
# CONFIG_SECURITY_SELINUX at all yet (Phase 4 cont'd's SELinux work is
# x86-only so far) - baking in a claim of enforcement the kernel can't
# even act on would be misleading, not just harmless.
set -euo pipefail

OUT="${1:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]}"
KERNEL="${2:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]}"
ROOTFS_DIR="${3:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]}"
DATA_DEV="${4:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]}"
HASH_DEV="${5:?usage: $0 <out.efi> <bzImage> <rootfs-dir> <data-device> <hash-device> [signing-key] [signing-cert]}"
SIGNING_KEY="${6:-}"
SIGNING_CERT="${7:-}"

DM_TABLE="$(dirname "$0")/../../hack/dm-verity-cmdline.sh"
CMDLINE_FILE="$(mktemp)"
trap 'rm -f "$CMDLINE_FILE"' EXIT
{
  # enforcing=1: Phase 4 cont'd's real SELinux policy (selinux/) already
  # proved a clean, zero-denial boot under enforcing mode (see
  # hack/qemu-selinux-test.sh) - this is what actually makes that the
  # shipped default rather than just a fact proven about a test boot.
  # kernel/configs/janus_<track>_defconfig's own SECURITY_SELINUX_DEVELOP=y
  # deliberately stays on regardless (so /sys/fs/selinux/enforce can
  # still be toggled interactively for debugging, and the kernel's own
  # default without this cmdline override would still be the safer
  # permissive one) - this UKI-baked cmdline is what makes enforcing the
  # real, permanent default in practice, since there's no boot menu to
  # add it from later.
  enforcing_arg=""
  [ "${UKI_SELINUX_ENFORCING:-1}" = "1" ] && enforcing_arg=" enforcing=1"
  # JANUS_SCHEMATIC: the image schematic's ID (internal/schematic), for an
  # image built with extensions. Signed with the rest of the cmdline, it's
  # how the node knows its schematic and how Upgrade keeps it. Unset: the
  # default schematic (no extension), as in every image built before.
  schematic_arg=""
  if [ -n "${JANUS_SCHEMATIC:-}" ]; then
    if ! [[ "$JANUS_SCHEMATIC" =~ ^[0-9a-f]{64}$ ]]; then
      echo "JANUS_SCHEMATIC must be a 64-character hex schematic ID, got: $JANUS_SCHEMATIC" >&2
      exit 1
    fi
    schematic_arg=" janus.schematic=$JANUS_SCHEMATIC"
  fi
  # UKI_EXTRA_CMDLINE: development builds only, e.g.
  # sysctl.kernel.printk_ratelimit=0 to see every SELinux denial while
  # writing a new domain's rules. Never set for a release.
  #
  # The data and hash devices are usually partition labels
  # (PARTLABEL=BOOT-A-DATA): the kernel resolves them itself, whatever
  # the disk's name. dm-mod.waitfor makes it wait for them - USB and
  # NVMe disks appear after the device-mapper would otherwise look.
  #
  # Two consoles: the screen (tty0) and the serial port, which comes
  # last so it's /dev/console - rootfs/init copies its output to the
  # screen as well.
  printf 'console=tty0 console=%s panic=-1 dm-mod.create="%s" dm-mod.waitfor=%s,%s root=/dev/dm-0 rootfstype=squashfs ro ip=dhcp%s%s%s' \
    "${UKI_CONSOLE:-ttyS0}" "$("$DM_TABLE" "$ROOTFS_DIR" "$DATA_DEV" "$HASH_DEV")" "$DATA_DEV" "$HASH_DEV" "$enforcing_arg" "$schematic_arg" \
    "${UKI_EXTRA_CMDLINE:+ $UKI_EXTRA_CMDLINE}"
} > "$CMDLINE_FILE"

mkdir -p "$(dirname "$OUT")"
UKIFY_ARGS=(
  build
  --linux="$KERNEL"
  --cmdline="@$CMDLINE_FILE"
  --os-release="$(printf 'NAME=Janus\nPRETTY_NAME=Janus\nID=janus\n')"
  -o "$OUT"
)
if [ -n "$SIGNING_KEY" ] && [ -n "$SIGNING_CERT" ]; then
  UKIFY_ARGS+=(--secureboot-private-key="$SIGNING_KEY" --secureboot-certificate="$SIGNING_CERT")
fi
if [ -z "${UKIFY_STUB:-}" ]; then
  stub_dir="$(dirname "$0")/../../build/systemd-stub"
  stub=linuxx64.efi.stub
  [ "$(dd if="$KERNEL" bs=1 skip=56 count=4 2>/dev/null)" = ARMd ] && stub=linuxaa64.efi.stub
  if [ ! -f "$stub_dir/$stub" ]; then
    mkdir -p "$stub_dir"
    docker build -q --target export -o "$stub_dir" "$(dirname "$0")/../../systemd-stub" >/dev/null
  fi
  UKIFY_STUB="$stub_dir/$stub"
fi
UKIFY_ARGS+=(--stub="$UKIFY_STUB")
ukify "${UKIFY_ARGS[@]}"

echo "Wrote $OUT"
