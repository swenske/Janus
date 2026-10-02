#!/usr/bin/env bash
# Checks janusctl's Debian packages (hack/janusctl-deb.sh): metadata and
# contents of both architectures, a byte-identical rebuild, then installs
# the amd64 one with dpkg in Debian and Ubuntu containers, runs it and
# removes it.
#
# Usage: hack/janusctl-deb-test.sh VERSION DEBDIR
#   VERSION  the one the packages were built with (the Makefile's VERSION)
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: $0 VERSION DEBDIR" >&2
  exit 2
fi
tag="$1"
debdir="$(cd "$2" && pwd)"
repo="$(cd "$(dirname "$0")/.." && pwd)"

fail() { echo "janusctl-deb-test FAILED: $*" >&2; exit 1; }

shopt -s nullglob
debs=("$debdir"/janusctl_*_amd64.deb)
[ ${#debs[@]} -eq 1 ] || fail "expected one janusctl_*_amd64.deb in $debdir, found ${#debs[@]}"
amd64="${debs[0]}"
arm64="${amd64%_amd64.deb}_arm64.deb"
[ -f "$arm64" ] || fail "no $arm64"
version="$(dpkg-deb -f "$amd64" Version)"

for deb in "$amd64" "$arm64"; do
  arch="$(dpkg-deb -f "$deb" Architecture)"
  [ "$(dpkg-deb -f "$deb" Package)" = janusctl ] || fail "$deb: wrong Package"
  [ "$(dpkg-deb -f "$deb" Version)" = "$version" ] || fail "$deb: version differs from the amd64 one"
  [ -z "$(dpkg-deb -f "$deb" Depends)" ] || fail "$deb: has dependencies"
  files="$(dpkg-deb -c "$deb" | awk '{print $1, $2, $6}')"
  grep -qx -- '-rwxr-xr-x root/root ./usr/bin/janusctl' <<<"$files" || fail "$deb: no root-owned 0755 /usr/bin/janusctl"
  grep -qx -- '-rw-r--r-- root/root ./usr/share/doc/janusctl/copyright' <<<"$files" || fail "$deb: no copyright file"
  tmp="$(mktemp -d)"
  dpkg-deb -x "$deb" "$tmp"
  case "$arch" in
    amd64) want="x86-64" ;;
    arm64) want="aarch64" ;;
  esac
  desc="$(file -b "$tmp/usr/bin/janusctl")"
  rm -rf "$tmp"
  [[ "$desc" == *"$want"* && "$desc" == *"statically linked"* ]] || fail "$deb: binary is \"$desc\""
  echo "OK $(basename "$deb"): $arch, static $want binary, no dependency"
done

# A rerun release must give the same bytes - aptly refuses a different
# package under a version it already has.
rebuild="$(mktemp -d)"
trap 'rm -rf "$rebuild"' EXIT
for deb in "$amd64" "$arm64"; do
  arch="$(dpkg-deb -f "$deb" Architecture)"
  again="$("$repo/hack/janusctl-deb.sh" "$tag" "$arch" "$rebuild")"
  cmp -s "$deb" "$again" || fail "rebuilding $(basename "$deb") gives different bytes"
done
echo "OK rebuild: both packages byte-identical"

for image in debian:trixie debian:bookworm ubuntu:24.04; do
  out="$(docker run --rm -v "$amd64:/tmp/janusctl.deb:ro" "$image" sh -ec '
    dpkg -i /tmp/janusctl.deb >/dev/null
    test "$(command -v janusctl)" = /usr/bin/janusctl
    janusctl version 2>&1 || true
    janusctl image seed-controller 2>&1 || true
    dpkg -r janusctl >/dev/null
    test ! -e /usr/bin/janusctl
  ')" || fail "$image: install/run/remove failed: $out"
  grep -qx "Client: $tag" <<<"$out" || fail "$image: janusctl version didn't print $tag: $out"
  echo "OK $image: dpkg -i, janusctl version (Client: $tag), dpkg -r"
done

echo "janusctl-deb-test OK: $version"
