#!/usr/bin/env bash
# Checks the Terraform provider's release archives
# (hack/terraform-provider-dist.sh): the checksums, each archive's
# contents and platform, that the linux/amd64 binary is the provider
# (and says its version), and a byte-identical rebuild.
#
# Usage: hack/terraform-provider-dist-test.sh VERSION DISTDIR
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: $0 VERSION DISTDIR" >&2
  exit 2
fi
version="$1"
dist="$(cd "$2" && pwd)"
repo="$(cd "$(dirname "$0")/.." && pwd)"
name="${version#v}"
name="${name//[^0-9A-Za-z.+-]/.}"

fail() { echo "terraform-provider-dist-test FAILED: $*" >&2; exit 1; }

sums="terraform-provider-janus_${name}_SHA256SUMS"
[ -f "$dist/$sums" ] || fail "no $sums in $dist"
(cd "$dist" && sha256sum --quiet -c "$sums") || fail "checksums"
[ "$(wc -l < "$dist/$sums")" = 4 ] || fail "$sums lists $(wc -l < "$dist/$sums") archives, want 4"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
for want in "linux amd64 ELF 64-bit LSB executable, x86-64" "linux arm64 ELF 64-bit LSB executable, ARM aarch64" \
  "darwin amd64 Mach-O 64-bit x86_64 executable" "darwin arm64 Mach-O 64-bit arm64 executable"; do
  read -r os arch kind <<<"$want"
  archive="$dist/terraform-provider-janus_${name}_${os}_${arch}.tar.gz"
  [ -f "$archive" ] || fail "no $(basename "$archive")"
  [ "$(tar -tzf "$archive" | sort | xargs)" = "LICENSE terraform-provider-janus" ] || fail "$(basename "$archive") holds $(tar -tzf "$archive" | xargs)"
  mkdir -p "$work/$os-$arch"
  tar -xzf "$archive" -C "$work/$os-$arch"
  file -b "$work/$os-$arch/terraform-provider-janus" | grep -q "^$kind" || fail "$os/$arch: $(file -b "$work/$os-$arch/terraform-provider-janus")"
  [ -x "$work/$os-$arch/terraform-provider-janus" ] || fail "$os/$arch: not executable"
done

# Run on its own, a provider says it's one - and which version.
out="$("$work/linux-amd64/terraform-provider-janus" 2>&1 || true)"
grep -qi "plugin" <<<"$out" || fail "the linux/amd64 binary didn't say it's a provider: $out"
grep -qaF "$version" "$work/linux-amd64/terraform-provider-janus" || fail "the binary doesn't carry the version $version"

# The same bytes again.
"$repo/hack/terraform-provider-dist.sh" "$version" "$work/again" >/dev/null
cmp -s "$dist/$sums" "$work/again/$sums" || fail "a rebuild gave other archives: $(diff "$dist/$sums" "$work/again/$sums")"

echo "terraform-provider-dist test OK: 4 archives, their checksums, contents and platforms, a byte-identical rebuild"
