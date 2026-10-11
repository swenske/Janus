#!/usr/bin/env bash
# The apt server's publishing (packaging/apt) run for real with aptly, in
# a Debian container: a server published before "testing" existed is set
# up again (setup-server.sh), then a release, a pre-release and its
# promotion go through the forced command (janus-publish-dispatch.sh) as
# image-build.yml and release-promote.yml send them:
#
#   - setup-server.sh adds "janus-testing" with every package "janus"
#     has, published as suite "testing";
#   - "publish testing" (a pre-release) reaches testing only;
#   - "publish" (a release, or a promoted pre-release: the same bytes
#     again) reaches stable and testing;
#   - the dispatcher refuses anything else, and sudo runs the publishing
#     script with "stable" or "testing" only.
#
# Usage: hack/apt-publish-test.sh (Docker; no root on the host)
set -euo pipefail
cd "$(dirname "$0")/.."

docker run --rm -i -v "$PWD/packaging/apt:/apt:ro" debian:trixie bash -euo pipefail <<'EOF'
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq aptly gnupg sudo dpkg-dev >/dev/null 2>&1

fail() { echo "FAIL: $*" >&2; exit 1; }
gpg --batch --quiet --passphrase '' --quick-gen-key "Janus apt test" ed25519 sign never 2>/dev/null
fpr=$(gpg --list-keys --with-colons | awk -F: '/^fpr:/ { print $10; exit }')
mkdir -p /srv/apt
for f in janus-aptly-publish.sh janus-publish-dispatch.sh setup-server.sh; do
  sed "s/0731333D9DDFFF9408CD6AECA3339293BD0CBBDC/$fpr/" "/apt/$f" > "/srv/apt/$f"
  chmod 755 "/srv/apt/$f"
done

# deb VERSION [MARK]: a janusctl package (MARK changes its bytes).
deb() {
  d=$(mktemp -d)
  mkdir -p "$d/DEBIAN" "$d/usr/bin"
  printf 'Package: janusctl\nVersion: %s\nArchitecture: amd64\nMaintainer: test <t@example.org>\nDescription: test\n' "$1" > "$d/DEBIAN/control"
  echo "${2:-janusctl $1}" > "$d/usr/bin/janusctl"
  dpkg-deb --root-owner-group -Zgzip --build "$d" "/tmp/janusctl_$1_amd64.deb" >/dev/null
}
has() { # has REPO VERSION
  aptly repo show -with-packages "$1" | grep -q "janusctl_$2_amd64"
}
published() { aptly publish list -raw | grep -qx "janus $1"; }
# ssh COMMAND [FILE]: the forced command, as janus-publish, stdin FILE.
ssh_as_publisher() {
  su janus-publish -s /bin/sh -c "SSH_ORIGINAL_COMMAND='$1' /usr/local/sbin/janus-publish-dispatch.sh" < "${2:-/dev/null}"
}

echo "== a server from before testing: janus published as stable"
deb 2026.10.11
aptly repo create -distribution=stable -component=main janus >/dev/null
aptly repo add janus /tmp/janusctl_2026.10.11_amd64.deb >/dev/null
aptly publish repo -gpg-key="$fpr" -architectures=amd64,arm64 -distribution=stable -component=main janus janus >/dev/null

echo "== setup-server.sh again: testing appears, with stable's packages"
/srv/apt/setup-server.sh "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest test" >/dev/null
has janus-testing 2026.10.11 || fail "testing lacks the release stable has"
published testing || fail "testing isn't published"
/srv/apt/setup-server.sh "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest test" >/dev/null || fail "setup-server.sh isn't idempotent"

echo "== a pre-release: testing only"
deb 2026.10.12
ssh_as_publisher "upload janusctl_2026.10.12_amd64.deb" /tmp/janusctl_2026.10.12_amd64.deb
ssh_as_publisher "publish testing" >/dev/null
has janus-testing 2026.10.12 || fail "testing lacks the pre-release"
if has janus 2026.10.12; then fail "stable has the pre-release"; fi
grep -q '2026.10.12' /root/.aptly/public/janus/dists/testing/main/binary-amd64/Packages || fail "testing's index lacks the pre-release"

echo "== its promotion: the same bytes, now stable"
ssh_as_publisher "upload janusctl_2026.10.12_amd64.deb" /tmp/janusctl_2026.10.12_amd64.deb
ssh_as_publisher "publish" >/dev/null
has janus 2026.10.12 || fail "stable lacks the promoted release"
grep -q '2026.10.12' /root/.aptly/public/janus/dists/stable/main/binary-amd64/Packages || fail "stable's index lacks the promoted release"

echo "== a release cut directly: stable and testing"
deb 2026.10.13
ssh_as_publisher "upload janusctl_2026.10.13_amd64.deb" /tmp/janusctl_2026.10.13_amd64.deb
ssh_as_publisher "publish" >/dev/null
if ! has janus 2026.10.13 || ! has janus-testing 2026.10.13; then fail "a release must reach stable and testing"; fi

echo "== other bytes under a published version: refused"
deb 2026.10.13 tampered
ssh_as_publisher "upload janusctl_2026.10.13_amd64.deb" /tmp/janusctl_2026.10.13_amd64.deb
if ssh_as_publisher "publish" >/dev/null 2>&1; then fail "other bytes for 2026.10.13 were published"; fi
rm -f /home/janus-publish/incoming/*.deb

echo "== nothing else"
for cmd in "publish stable now" "publish unstable" "rm -rf /" "upload ../x.deb"; do
  if ssh_as_publisher "$cmd" >/dev/null 2>&1; then fail "the dispatcher accepted: $cmd"; fi
done
if su janus-publish -s /bin/sh -c 'sudo -n /usr/local/sbin/janus-aptly-publish.sh unstable' >/dev/null 2>&1; then
  fail "sudo ran the publishing script with another argument"
fi
echo "ok: apt publishing (stable, testing, promotion)"
EOF
