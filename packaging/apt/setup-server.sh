#!/bin/sh
# One-time setup of janusctl's publishing on the aptly server
# (apt.int.sw-servers.net, Debian 13, aptly 1.6), run as root. Idempotent.
# Same design as swenske/gotochanger's: after it,
#
#   - the "janus" aptly repo exists (suite "stable", component "main"),
#     published at https://apt.sw-servers.net/janus by the first release;
#   - janus-publish can only, over SSH and with one of the given keys,
#     upload janusctl_<version>_<arch>.deb into its incoming/ directory
#     and run janus-aptly-publish.sh as root (janus-publish-dispatch.sh, a
#     forced command) - the release step of image-build.yml.
#
# Usage: setup-server.sh <runner-public-key>...
#   (~actions-runner/.ssh/id_janus_aptly.pub on each runner labeled
#   janus-publish - one key per machine, the private half never leaves
#   it). The keys given replace every key accepted before: pass them all.
set -eu
[ "$#" -gt 0 ] || { echo "usage: $0 <runner-public-key>..." >&2; exit 1; }
HERE="$(cd "$(dirname "$0")" && pwd)"

for key in "$@"; do
  case "$key" in
    ssh-ed25519\ *) ;;
    *) echo "$0: expected ssh-ed25519 public keys" >&2; exit 1 ;;
  esac
done

id janus-publish >/dev/null 2>&1 \
  || useradd --system --create-home --home-dir /home/janus-publish --shell /bin/sh janus-publish
chmod 700 /home/janus-publish
install -d -o janus-publish -g janus-publish -m 700 \
  /home/janus-publish/incoming /home/janus-publish/.ssh

install -o root -g root -m 0755 "$HERE/janus-publish-dispatch.sh" /usr/local/sbin/janus-publish-dispatch.sh
install -o root -g root -m 0700 "$HERE/janus-aptly-publish.sh" /usr/local/sbin/janus-aptly-publish.sh

sudoers="$(mktemp)"
echo 'janus-publish ALL=(root) NOPASSWD: /usr/local/sbin/janus-aptly-publish.sh' > "$sudoers"
visudo -cf "$sudoers" >/dev/null
install -o root -g root -m 0440 "$sudoers" /etc/sudoers.d/janus-publish
rm -f "$sudoers"

for key in "$@"; do
  echo "command=\"/usr/local/sbin/janus-publish-dispatch.sh\",no-pty,no-agent-forwarding,no-X11-forwarding,no-port-forwarding $key"
done > /home/janus-publish/.ssh/authorized_keys
chown janus-publish:janus-publish /home/janus-publish/.ssh/authorized_keys
chmod 600 /home/janus-publish/.ssh/authorized_keys

aptly repo show janus >/dev/null 2>&1 \
  || aptly repo create -comment="Janus CLI (janusctl) - https://github.com/swenske/Janus" \
       -distribution=stable -component=main janus

echo "janus-publish ready; the first release publishes https://apt.sw-servers.net/janus"
