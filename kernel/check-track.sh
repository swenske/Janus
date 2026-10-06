#!/usr/bin/env bash
# Checks what a kernel track's build (make kernel-build-<track>) left in
# build/kernel-<track>/ against what's committed for it: the configuration
# kbuild resolved must be kernel/configs/janus_<track>_defconfig itself
# (else a symbol was added, dropped or defaulted behind the file's back -
# make kernel-config-refresh), and the files it read
# kernel/built-files-<track>-amd64.txt (hack/upstream filters the
# kernel's CVEs with it - make kernel-built-files-<track>).
#
# Usage: kernel/check-track.sh <track> [build-dir]
set -euo pipefail

TRACK="${1:?usage: $0 <track> [build-dir]}"
BUILD="${2:-build}/kernel-$TRACK"
CONFIG="kernel/configs/janus_${TRACK}_defconfig"

symbols() { grep -E '^(CONFIG_[A-Za-z0-9_]+=|# CONFIG_[A-Za-z0-9_]+ is not set$)' "$1"; }
if ! diff <(symbols "$CONFIG") <(symbols "$BUILD/config") > "$BUILD/config.diff"; then
  echo "$CONFIG isn't what kbuild resolves it to (< committed, > resolved) - run make kernel-config-refresh KERNEL_TRACK=$TRACK, review and commit:" >&2
  cat "$BUILD/config.diff" >&2
  exit 1
fi
if ! cmp -s "$BUILD/built-files.txt" "kernel/built-files-$TRACK-amd64.txt"; then
  echo "kernel/built-files-$TRACK-amd64.txt doesn't match this build - run make kernel-built-files-$TRACK and commit" >&2
  exit 1
fi
echo "kernel $TRACK: its config and list of built files are current"
