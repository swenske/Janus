#!/bin/sh
# Sets up (or updates) the image factory's mirror on a host, run as root
# beside the janus-site binary (built from this tree: go build -trimpath
# -o janus-site ./site/backend) and this directory's janus-mirror.service.
# Idempotent. Before the first start:
#
#   /etc/janus-mirror/token  the site's -mirror-token-file content
#   /etc/janus-mirror/flags  JANUS_MIRROR_FLAGS="-addr <listen> -mirror-of <the site's own address>"
#
# The site's index and files are read straight from janus-site, never
# through its public front end (which refuses /api/v1/mirror/).
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
id janus-mirror >/dev/null 2>&1 || useradd --system --home-dir /srv/janus-mirror --shell /usr/sbin/nologin janus-mirror
install -d -o janus-mirror -g janus-mirror -m 0755 /srv/janus-mirror
install -d -o root -g janus-mirror -m 0750 /etc/janus-mirror
install -m 0755 "$HERE/../janus-site" /usr/local/bin/janus-mirror.new
mv /usr/local/bin/janus-mirror.new /usr/local/bin/janus-mirror
install -m 0644 "$HERE/janus-mirror.service" /etc/systemd/system/janus-mirror.service
systemctl daemon-reload
systemctl enable janus-mirror >/dev/null
if [ -s /etc/janus-mirror/token ] && [ -s /etc/janus-mirror/flags ]; then
  systemctl restart janus-mirror
  echo "janus-mirror restarted"
else
  echo "set /etc/janus-mirror/token and /etc/janus-mirror/flags, then: systemctl start janus-mirror"
fi
