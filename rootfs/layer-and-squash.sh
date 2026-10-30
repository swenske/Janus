#!/usr/bin/env bash
# Second half of building a rootfs, shared by rootfs/assemble.sh (from
# source) and rootfs/assemble-from-base.sh (from a release's base tree):
# layers the optional extensions onto a prepared tree, then writes the
# squashfs image and its dm-verity hash tree.
#
# Usage: rootfs/layer-and-squash.sh <tree> <out-dir>
#   JANUS_EXTENSIONS  space-separated layer tars: the image's HAProxy
#                     (rootfs/haproxy-layer.sh), then its extensions
#                     (hack/extpack)
#   JANUS_IMAGE_INFO  the image's image.json (hack/extpack image-info):
#                     its schematic and the HAProxy branch and kernel
#                     track it is built with - what the node reports
#
# <tree>/.janus-labels ("path type" lines) gives the SELinux types of the
# executables; each extension tar brings its own. It's consumed here and
# not part of the image.
set -euo pipefail
export PATH="$PATH:/usr/sbin:/sbin"

TREE="${1:?usage: $0 <tree> <out-dir>}"
OUT_DIR="${2:?usage: $0 <tree> <out-dir>}"

LABELS="$(mktemp)"
trap 'rm -f "$LABELS"' EXIT
[ -f "$TREE/.janus-labels" ] && cat "$TREE/.janus-labels" > "$LABELS"
rm -f "$TREE/.janus-labels"

# Extensions: each tar is the extension's file tree, its manifest under
# usr/lib/janus/extensions/, and its own .janus-labels. An extension may
# only add files, never replace one of the base system's or another
# extension's.
for ext in ${JANUS_EXTENSIONS:-}; do
  while IFS= read -r entry; do
    case "$entry" in */) continue ;; esac
    if [ -e "$TREE/$entry" ] && [ "$entry" != ".janus-labels" ]; then
      echo "extension $ext would replace $entry" >&2
      exit 1
    fi
  done < <(tar -tf "$ext")
  tar -xf "$ext" -C "$TREE" --no-same-owner
  if [ -f "$TREE/.janus-labels" ]; then
    cat "$TREE/.janus-labels" >> "$LABELS"
    rm -f "$TREE/.janus-labels"
  fi
  echo "Layered $(basename "$ext")"
done

# Exactly one HAProxy: the base tree has none, two layers can't both
# bring one (nothing replaces a file).
if [ ! -x "$TREE/usr/local/sbin/haproxy" ]; then
  echo "no HAProxy in the image: lay one onto the base (rootfs/haproxy-layer.sh)" >&2
  exit 1
fi

if [ -n "${JANUS_IMAGE_INFO:-}" ]; then
  mkdir -p "$TREE/usr/lib/janus"
  install -m 0644 "$JANUS_IMAGE_INFO" "$TREE/usr/lib/janus/image.json"
fi

# SELinux types via mksquashfs's pseudo-file `x` action, not setfattr on
# the tree before mksquashfs runs, which turned out not to work at all:
# a real setfattr on the tree (tmpfs-backed under WSL2) reports success
# and reads back with getfattr - but mksquashfs, run as the build user or
# as root, never picks the xattr up into the image regardless (traced
# with an isolated single-file reproduction; -xattrs-include didn't help).
# The pseudo-file mechanism sets the attribute while writing the image.
#
# The chroot jail (cmd/janusd's -haproxy-chroot-dir) is a pseudo entry
# too - mode 0000, not even readable by its owner - rather than a real
# chmod 000 directory in the tree: mksquashfs can't read such a directory
# back when it isn't running as root, and it silently vanished from the
# image.
PSEUDO=(-p "var/empty D 0 0000 0 0")
while read -r path type; do
  [ -n "$path" ] || continue
  PSEUDO+=(-p "$path x security.selinux=system_u:object_r:$type")
done < "$LABELS"

mkdir -p "$OUT_DIR"
# -all-root: every file owned by root, whoever runs this - there's no
# /etc/passwd on the target to resolve another owner against.
# -root-mode 0755: mktemp -d's 0700 would otherwise become the image's
# root directory mode.
#
# -b 1M, squashfs's largest block: zstd compresses the big Go binaries
# better over a larger window. The image with every extension was 64.26
# MiB with the default 128 KiB blocks - over the 64 MiB BOOT-*-DATA
# partitions an installed disk has for good (v2026.10.14-6: the factory
# couldn't build it) - and 60.09 MiB with 1 MiB blocks (256 KiB: 62.61,
# 512 KiB: 61.22); the default image went from 14.96 to 14.10 MiB. The
# kernel reads a block whole; janusd still listens 1.2 s after the
# kernel started, as with 128 KiB (OVMF + KVM, six boots each).
#
# Every time in the image is the commit's (hack/reproducible.sh):
# mksquashfs writes the same bytes for the same tree and times, and the
# tree's own times are only when it was copied (#433: two builds of a
# commit gave two rootfs.squashfs, the same files). Given as options:
# from its environment, mksquashfs would only lower the later ones.
. "$(dirname "$0")/../hack/reproducible.sh"
env -u SOURCE_DATE_EPOCH mksquashfs "$TREE" "$OUT_DIR/rootfs.squashfs" -noappend -comp zstd -Xcompression-level 19 -b 1M -all-root -root-mode 0755 \
  -mkfs-time "$SOURCE_DATE_EPOCH" -all-time "$SOURCE_DATE_EPOCH" "${PSEUDO[@]}"

# dm-verity's salt and its superblock's UUID come from the image instead
# of the random ones veritysetup makes: the same image, the same hash
# tree and root hash - the UKIs pin it in their signed command line.
salt="$(sha256sum "$OUT_DIR/rootfs.squashfs" | cut -c1-64)"
u="$(printf 'uuid %s' "$salt" | sha256sum)"
veritysetup format --salt="$salt" --uuid="${u:0:8}-${u:8:4}-${u:12:4}-${u:16:4}-${u:20:12}" \
  "$OUT_DIR/rootfs.squashfs" "$OUT_DIR/rootfs.verity" > "$OUT_DIR/rootfs.verity.info"
grep "^Root hash:" "$OUT_DIR/rootfs.verity.info" | awk '{print $3}' > "$OUT_DIR/rootfs.roothash"

echo "Wrote $OUT_DIR/{rootfs.squashfs,rootfs.verity,rootfs.roothash}"
echo "Root hash: $(cat "$OUT_DIR/rootfs.roothash")"
