#!/usr/bin/env bash
# Writes a configuration kbuild resolved (make menuconfig, make
# olddefconfig) back to kernel/configs/, keeping that file's hand-written
# header - everything above kbuild's own "Automatically generated file"
# line. A new file gets HEADER_FROM's header (another track's).
#
# Usage: kernel/save-config.sh <resolved .config> <kernel/configs/file> [HEADER_FROM]
set -euo pipefail

NEW="${1:?usage: $0 <resolved .config> <kernel/configs/file> [header-from]}"
OUT="${2:?usage: $0 <resolved .config> <kernel/configs/file> [header-from]}"
FROM="${3:-$OUT}"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
[ -f "$FROM" ] && sed '/^# Automatically generated file/,$d' "$FROM" > "$tmp"
# kbuild's own header ("#", "Automatically generated file; DO NOT EDIT.",
# the version, "#"): its first "#" is already the hand-written header's
# last line.
sed -e '1{/^#$/d}' \
  -e "s/^# Automatically generated file; DO NOT EDIT\.\$/# Automatically generated file; DO NOT EDIT (by hand - regenerate it with\n# make kernel-menuconfig or make kernel-config-refresh, KERNEL_TRACK=<track>)./" \
  "$NEW" >> "$tmp"
cp "$tmp" "$OUT"
