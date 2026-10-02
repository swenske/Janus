#!/bin/sh
# Root-owned, mode 0700: the ONLY command /etc/sudoers.d/janus-publish lets
# the janus-publish account run as root. No arguments - everything comes
# from the fixed incoming directory (same design as
# gotochanger-aptly-publish.sh). Publishes the "janus" local repo at
# https://apt.sw-servers.net/janus, suite "stable", component "main":
# janusctl is a static binary, the same package for every Debian/Ubuntu
# release.
set -eu

INCOMING="/home/janus-publish/incoming"
REPO="janus"
PREFIX="janus"
DIST="stable"
ARCHS="amd64,arm64"
GPGKEY="0731333D9DDFFF9408CD6AECA3339293BD0CBBDC"

added=0
for f in "$INCOMING"/*.deb; do
  [ -e "$f" ] || continue
  aptly repo add "$REPO" "$f"
  added=1
done

if [ "$added" = "0" ]; then
  echo "janus-aptly-publish: nothing to add in $INCOMING" >&2
  exit 1
fi

# The prefix must be given explicitly (without it aptly publishes at the
# root of /opt/aptly/public - see gotochanger-aptly-publish.sh).
if aptly publish list -raw | grep -qx "$PREFIX $DIST"; then
  aptly publish update -gpg-key="$GPGKEY" "$DIST" "$PREFIX"
else
  aptly publish repo -gpg-key="$GPGKEY" -architectures="$ARCHS" \
    -distribution="$DIST" -component=main "$REPO" "$PREFIX"
fi

rm -f "$INCOMING"/*.deb
