#!/usr/bin/env bash
# Packs a HAProxy binary into the layer rootfs/layer-and-squash.sh lays
# onto a base tree: an image carries exactly one, the branch its schematic
# picks (variants.mk) - which is why HAProxy isn't in the base tree a
# release publishes, and each branch's layer is published next to it.
# Deterministic: the same binary always gives the same tar.
#
# Usage: rootfs/haproxy-layer.sh <haproxy-bin> <out.tar>
set -euo pipefail

BIN="${1:?usage: $0 <haproxy-bin> <out.tar>}"
OUT="${2:?usage: $0 <haproxy-bin> <out.tar>}"

TREE="$(mktemp -d)"
trap 'rm -rf "$TREE"' EXIT
mkdir -p "$TREE/usr/local/sbin"
install -m 0755 "$BIN" "$TREE/usr/local/sbin/haproxy"
# Its SELinux type, set when the image is written (layer-and-squash.sh).
echo "usr/local/sbin/haproxy haproxy_exec_t" > "$TREE/.janus-labels"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -cf "$OUT" -C "$TREE" .janus-labels usr
