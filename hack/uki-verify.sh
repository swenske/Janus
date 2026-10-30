#!/usr/bin/env bash
# uki-verify.sh CERT IMAGE... - every EFI binary on every EFI System
# Partition of each IMAGE (a raw disk, a qcow2, a VMDK or the installer
# ISO) must carry a Secure Boot signature CERT verifies (sbverify): what
# a node installed from the image boots - \EFI\BOOT\BOOTX64.EFI and both
# slots' \JANUS\UKI-<slot>.EFI - starts with Secure Boot on once CERT is
# in its firmware's db (docs/guide/secure-boot.md). An image without an
# ESP, or an ESP without an EFI binary, fails: nothing checked isn't a
# pass.
#
# Run by image-build.yml before a release is published and by
# image/schematic/build.sh for the factory's images; needs qemu-img,
# sgdisk, mtools and sbverify (sbsigntool).
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

CERT="${1:?usage: $0 <cert.pem> <image>...}"
shift
[ "$#" -gt 0 ] || { echo "usage: $0 <cert.pem> <image>..." >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for img in "$@"; do
  raw="$img"
  format="$(qemu-img info --output=json "$img" | python3 -c 'import json, sys; print(json.load(sys.stdin)["format"])')"
  if [ "$format" != raw ]; then
    raw="$WORK/disk.raw"
    qemu-img convert -O raw "$img" "$raw"
  fi
  # sgdisk -p: Number, Start (sector), End, Size (two words), Code, Name.
  mapfile -t starts < <(sgdisk -p "$raw" 2>/dev/null | awk '$6 == "EF00" {print $2}')
  [ "${#starts[@]}" -gt 0 ] || { echo "$img: no EFI System Partition" >&2; exit 1; }
  n=0
  for start in "${starts[@]}"; do
    esp="$raw@@$((start * 512))"
    while IFS= read -r f; do
      case "${f^^}" in
      *.EFI) ;;
      *) continue ;;
      esac
      rm -f "$WORK/uki.efi"
      mcopy -n -i "$esp" "$f" "$WORK/uki.efi"
      if ! sbverify --cert "$CERT" "$WORK/uki.efi" >/dev/null 2>&1; then
        echo "$img: ${f#::} isn't signed by $CERT" >&2
        exit 1
      fi
      echo "$img: ${f#::} signed by $CERT"
      n=$((n + 1))
    done < <(mdir -i "$esp" -/ -b ::/)
  done
  [ "$n" -gt 0 ] || { echo "$img: no EFI binary on its ESP" >&2; exit 1; }
  [ "$raw" = "$img" ] || rm -f "$raw"
done
