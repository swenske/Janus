#!/bin/sh
# Root-owned, mode 0700: the ONLY command /etc/sudoers.d/janus-publish lets
# the janus-publish account run as root, with "stable" or "testing" as its
# one argument. Everything else comes from the fixed incoming directory
# (same design as gotochanger-aptly-publish.sh). Publishes at
# https://apt.sw-servers.net/janus, component "main" - janusctl is a static
# binary, the same package for every Debian/Ubuntu release:
#
#   stable   the "janus" local repo (suite "stable"): releases - added to
#            "janus-testing" too, so testing always has them;
#   testing  the "janus-testing" local repo (suite "testing"): releases
#            and pre-releases (docs/contributing/releasing.md).
#
# A promoted pre-release is published again as stable: the same bytes
# (the packages are reproducible), which aptly accepts in both repos.
set -eu

INCOMING="/home/janus-publish/incoming"
PREFIX="janus"
ARCHS="amd64,arm64"
GPGKEY="0731333D9DDFFF9408CD6AECA3339293BD0CBBDC"

case "${1:-}" in
  stable) REPOS="janus:stable janus-testing:testing" ;;
  testing) REPOS="janus-testing:testing" ;;
  *) echo "usage: $0 stable|testing" >&2; exit 2 ;;
esac

added=0
for f in "$INCOMING"/*.deb; do
  [ -e "$f" ] || continue
  for r in $REPOS; do
    aptly repo add "${r%%:*}" "$f"
  done
  added=1
done

if [ "$added" = "0" ]; then
  echo "janus-aptly-publish: nothing to add in $INCOMING" >&2
  exit 1
fi

# The prefix must be given explicitly (without it aptly publishes at the
# root of /opt/aptly/public - see gotochanger-aptly-publish.sh).
for r in $REPOS; do
  repo="${r%%:*}" dist="${r#*:}"
  if aptly publish list -raw | grep -qx "$PREFIX $dist"; then
    aptly publish update -gpg-key="$GPGKEY" "$dist" "$PREFIX"
  else
    aptly publish repo -gpg-key="$GPGKEY" -architectures="$ARCHS" \
      -distribution="$dist" -component=main "$repo" "$PREFIX"
  fi
done

rm -f "$INCOMING"/*.deb
