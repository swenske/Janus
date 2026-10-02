#!/usr/bin/env bash
# Builds janusctl's Debian package for one architecture: a static
# (CGO_ENABLED=0) binary at /usr/bin/janusctl and nothing it depends on -
# the same .deb installs on any Debian or Ubuntu release. A release run of
# image-build.yml attaches both architectures to the GitHub Release and
# publishes them on https://apt.sw-servers.net/janus (README.md).
#
# Reproducible: -trimpath, no Go build ID, every timestamp clamped to the
# commit's date (SOURCE_DATE_EPOCH) - rebuilding a release gives the same
# bytes, which aptly requires to accept the same version twice.
#
# Usage: hack/janusctl-deb.sh VERSION ARCH OUTDIR
#   VERSION  janusctl's version, as the Makefile's VERSION (v2026.10.02-3);
#            the package's is the same without the leading "v"
#   ARCH     amd64 or arm64 (Go and Debian name them the same)
set -euo pipefail

if [ $# -ne 3 ]; then
  echo "usage: $0 VERSION ARCH OUTDIR" >&2
  exit 2
fi
version="$1"
arch="$2"
outdir="$3"

case "$arch" in
  amd64|arm64) ;;
  *) echo "$0: unsupported architecture $arch (amd64, arm64)" >&2; exit 2 ;;
esac

# A Debian version starts with a digit: a release tag gives 2026.10.02-3;
# anything else (a bare commit, "dev") sorts below every release.
debversion="${version#v}"
if ! [[ "$debversion" =~ ^[0-9][0-9A-Za-z.+~-]*$ ]]; then
  debversion="0~${debversion//[^0-9A-Za-z.+~]/.}"
fi

repo="$(cd "$(dirname "$0")/.." && pwd)"
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$repo" log -1 --format=%ct)}"

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT

mkdir -p "$root/DEBIAN" "$root/usr/bin" "$root/usr/share/doc/janusctl" \
  "$root/usr/share/lintian/overrides"
(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
  -ldflags "-s -w -buildid= -X main.version=$version" \
  -o "$root/usr/bin/janusctl" ./cmd/janusctl)
chmod 0755 "$root/usr/bin/janusctl"

{
  echo "Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/"
  echo "Upstream-Name: Janus"
  echo "Source: https://github.com/swenske/Janus"
  echo
  echo "Files: *"
  echo "Copyright: 2026 Sebastien WENSKE"
  echo "License: MIT"
  sed -e 's/^$/./' -e 's/^/ /' "$repo/LICENSE"
} > "$root/usr/share/doc/janusctl/copyright"
chmod 0644 "$root/usr/share/doc/janusctl/copyright"

# Debian policy's changelog: one entry, pointing at the release notes.
if [[ "$version" =~ ^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}(-[0-9]+)?$ ]]; then
  entry="Janus $version: https://github.com/swenske/Janus/releases/tag/$version"
else
  entry="Built from Janus $version, not a release."
fi
cat <<EOF | gzip -9n > "$root/usr/share/doc/janusctl/changelog.gz"
janusctl ($debversion) stable; urgency=medium

  * $entry

 -- Sebastien WENSKE <sebastien@wenske.fr>  $(date -u -R -d "@$SOURCE_DATE_EPOCH")
EOF
chmod 0644 "$root/usr/share/doc/janusctl/changelog.gz"

# Static on purpose: one package for every Debian/Ubuntu release.
echo "janusctl: statically-linked-binary [usr/bin/janusctl]" \
  > "$root/usr/share/lintian/overrides/janusctl"
chmod 0644 "$root/usr/share/lintian/overrides/janusctl"

cat > "$root/DEBIAN/control" <<EOF
Package: janusctl
Version: $debversion
Architecture: $arch
Maintainer: Sebastien WENSKE <sebastien@wenske.fr>
Installed-Size: $(du -sk --exclude=DEBIAN "$root" | cut -f1)
Section: admin
Priority: optional
Homepage: https://github.com/swenske/Janus
Description: command-line client for Janus nodes
 Janus is an immutable, API-only Linux distribution built around HAProxy:
 no shell, no SSH, everything goes through a mutual-TLS gRPC API.
 janusctl drives that API - system information, logs, packet captures,
 HAProxy configuration, maps, ACLs and certificates, network, firewall,
 VRRP and BGP, upgrades and rollbacks - and writes a Controller or
 network configuration onto a Janus disk image before its first boot.
EOF

# Directories 0755 whatever the umask; every mtime at SOURCE_DATE_EPOCH.
find "$root" -type d -exec chmod 0755 {} +
find "$root" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +

mkdir -p "$outdir"
deb="$outdir/janusctl_${debversion}_${arch}.deb"
dpkg-deb --root-owner-group -Zxz --build "$root" "$deb" >/dev/null
echo "$deb"
