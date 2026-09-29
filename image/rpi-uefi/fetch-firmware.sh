#!/usr/bin/env bash
# Downloads and extracts a real Pi firmware release zip (pftf/RPi4 or
# NumberOneGit/rpi5-uefi - see versions.mk's own comment for why these
# two, specifically, and the real maturity gap between them), verifying
# its sha256 against the pinned value before extracting anything - same
# discipline as every other pinned upstream fetch in this project
# (kernel/Dockerfile's own fetch stage, pkgs/haproxy's, etc), just a
# plain script rather than an isolated Docker stage since this fetch
# needs no foreign-architecture package trickery (unlike ca-certificates/
# or systemd-stub-arm64/) - a plain curl+unzip against a public GitHub
# release asset works directly on the build host or CI runner.
#
# Usage: image/rpi-uefi/fetch-firmware.sh <url> <sha256> <out-dir>
set -euo pipefail

URL="${1:?usage: $0 <url> <sha256> <out-dir>}"
SHA256="${2:?usage: $0 <url> <sha256> <out-dir>}"
OUT_DIR="${3:?usage: $0 <url> <sha256> <out-dir>}"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

ZIP="$WORKDIR/firmware.zip"
curl -fsSL -o "$ZIP" "$URL"

echo "${SHA256}  ${ZIP}" | sha256sum -c - >/dev/null

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"
unzip -q "$ZIP" -d "$OUT_DIR"

echo "Wrote $OUT_DIR (from $URL, sha256 verified)"
