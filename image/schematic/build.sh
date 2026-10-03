#!/usr/bin/env bash
# Builds every image of one architecture for an image schematic, from a
# release's build inputs - the base tree, kernel and extension packs the
# release published - rebuilding nothing (see docs/image-factory.md). Run
# by .github/workflows/schematic-build.yml; works locally too.
#
# Usage: image/schematic/build.sh <inputs-dir> <arch> <schematic.json> <out-dir> [signing-key signing-cert]
#
# <inputs-dir> holds, as a release names them: schematic-catalog.json,
# kernel-<arch>, rootfs-base-<arch>.tar, extension-<name>-<arch>.tar.
# <out-dir> gets, with the release's own file names:
#   amd64: the update bundle (rootfs.squashfs, .sha256, rootfs.verity,
#          uki-a.efi, uki-b.efi - signed when a key is given), janus.qcow2,
#          janus-kvm.qcow2, janus.vmdk, janus.iso; and disk.img, for a
#          boot test, which isn't published
#   arm64: pi4-disk.img, pi5-disk.img
# and manifest.json (schematic, version, files with size and sha256).
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

IN="${1:?usage: $0 <inputs-dir> <arch> <schematic.json> <out-dir> [signing-key signing-cert]}"
ARCH="${2:?}"
SCHEMATIC_FILE="${3:?}"
OUT="${4:?}"
SIGNING_KEY="${5:-}"
SIGNING_CERT="${6:-}"
REPO="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO"

extpack() { go run ./hack/extpack "$@"; }

# JANUS_VERSION: the release the inputs come from (default: the catalog's).
VERSION="${JANUS_VERSION:-$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$IN/schematic-catalog.json")}"
extpack check -schematic "$SCHEMATIC_FILE" -catalog "$IN/schematic-catalog.json" -arch "$ARCH"
JANUS_SCHEMATIC="$(extpack id -schematic "$SCHEMATIC_FILE")"
export JANUS_SCHEMATIC
LAYERS="$(extpack layers -schematic "$SCHEMATIC_FILE" -arch "$ARCH" -dir "$IN")"
KERNEL="$IN/kernel-$ARCH"
[ -s "$KERNEL" ] || { echo "no $KERNEL" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT"

echo "== rootfs: $VERSION base + extensions [${LAYERS:-none}], schematic $JANUS_SCHEMATIC"
JANUS_EXTENSIONS="$LAYERS" ./rootfs/assemble-from-base.sh "$WORK/rootfs" "$IN/rootfs-base-$ARCH.tar"
./rootfs/state-image.sh "$WORK/state.img" 128

case "$ARCH" in
amd64)
  echo "== update bundle"
  ./image/release/assemble.sh "$WORK/bundle" "$KERNEL" "$WORK/rootfs" "$SIGNING_KEY" "$SIGNING_CERT"
  if [ -n "$SIGNING_CERT" ]; then
    for uki in "$WORK/bundle"/uki-*.efi; do sbverify --cert "$SIGNING_CERT" "$uki"; done
  fi
  cp "$WORK/bundle"/{rootfs.squashfs,rootfs.squashfs.sha256,rootfs.verity,uki-a.efi,uki-b.efi} "$OUT/"
  echo "== disk images"
  ./image/disk/assemble.sh "$OUT/disk.img" "$KERNEL" "$WORK/rootfs" "$WORK/state.img" A
  ./image/kvm-proxmox/assemble.sh "$OUT/janus.qcow2" "$KERNEL" "$WORK/rootfs" "$WORK/state.img" A
  ./image/kvm/assemble.sh "$OUT/janus-kvm.qcow2" "$KERNEL" "$WORK/rootfs" "$WORK/state.img" A
  ./image/vmware/assemble.sh "$OUT/janus.vmdk" "$KERNEL" "$WORK/rootfs" "$WORK/state.img" A
  echo "== installer ISO"
  ./image/iso/assemble.sh "$OUT/janus.iso" "$KERNEL" "$WORK/rootfs" "$WORK/bundle"
  ;;
arm64)
  echo "== Raspberry Pi images"
  make -s systemd-stub pi4-firmware pi5-firmware
  for board in pi4 pi5; do
    UKIFY_STUB=build/systemd-stub/linuxaa64.efi.stub \
      ./image/rpi-uefi/assemble.sh "$OUT/$board-disk.img" "build/rpi-uefi/$board-firmware" \
      "$KERNEL" "$WORK/rootfs" "$WORK/state.img"
  done
  ;;
*)
  echo "unknown architecture $ARCH" >&2
  exit 1
  ;;
esac

python3 - "$OUT" "$JANUS_SCHEMATIC" "$VERSION" "$ARCH" "${GITHUB_RUN_URL:-}" <<'PY'
import datetime, hashlib, json, os, sys
out, schematic, version, arch, run_url = sys.argv[1:]
files = []
for name in sorted(os.listdir(out)):
    if name in ("manifest.json", "disk.img"):
        continue
    h = hashlib.sha256()
    with open(os.path.join(out, name), "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    files.append({"name": name, "size": os.path.getsize(os.path.join(out, name)), "sha256": h.hexdigest()})
manifest = {"schematic": schematic, "version": version, "arch": arch,
            "built_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), "run_url": run_url, "files": files}
with open(os.path.join(out, "manifest.json"), "w") as f:
    json.dump(manifest, f, indent=2)
print(f"manifest: {len(files)} files")
PY
