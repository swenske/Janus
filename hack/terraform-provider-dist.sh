#!/usr/bin/env bash
# Builds the Janus Terraform provider's release archives: one per
# platform (linux and darwin, amd64 and arm64), each holding the static
# binary terraform-provider-janus - the name a development override
# looks for (docs/terraform.md) - with the LICENSE, and their SHA-256 in
# terraform-provider-janus_<version>_SHA256SUMS. A release run of
# image-build.yml attaches them to the GitHub Release.
#
# Reproducible like janusctl's packages: -trimpath, no Go build ID, the
# archive's entries sorted, owned by root and dated the commit's date
# (SOURCE_DATE_EPOCH), gzip without a timestamp.
#
# Usage: hack/terraform-provider-dist.sh VERSION OUTDIR
#   VERSION  the Makefile's VERSION (v2026.10.04); the archives' names
#            carry it without the leading "v"
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: $0 VERSION OUTDIR" >&2
  exit 2
fi
version="$1"
outdir="$2"
name="${version#v}"
name="${name//[^0-9A-Za-z.+-]/.}"

repo="$(cd "$(dirname "$0")/.." && pwd)"
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$repo" log -1 --format=%ct)}"

mkdir -p "$outdir"
outdir="$(cd "$outdir" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

archives=()
for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${platform%/*}"
  arch="${platform#*/}"
  dir="$work/$os-$arch"
  mkdir -p "$dir"
  (cd "$repo/terraform-provider-janus" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -buildid= -X main.version=$version" -o "$dir/terraform-provider-janus" .)
  cp "$repo/LICENSE" "$dir/LICENSE"
  chmod 0755 "$dir/terraform-provider-janus"
  chmod 0644 "$dir/LICENSE"
  archive="terraform-provider-janus_${name}_${os}_${arch}.tar.gz"
  tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner --format=gnu \
    -C "$dir" -cf - terraform-provider-janus LICENSE | gzip -9n > "$outdir/$archive"
  archives+=("$archive")
done
(cd "$outdir" && sha256sum "${archives[@]}" > "terraform-provider-janus_${name}_SHA256SUMS")
echo "terraform-provider-janus $version: ${archives[*]} terraform-provider-janus_${name}_SHA256SUMS in $outdir"
