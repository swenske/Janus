#!/bin/sh
# One-time setup of the companion site's container (CT 222 on lgspve02,
# Debian 13), run as root. Idempotent. After it:
#
#   - janus-site (service user) runs /srv/janus-site/bin/janus-site under
#     systemd, data in /srv/janus-site/data;
#   - janus-upload can only write into /srv/janus-site/data/images, over
#     SSH with rrsync - the schematic build workflow's uploads;
#   - janus-deploy can only upload bin/incoming/janus-site and run
#     janus-site-activate as root - the site deploy workflow.
#
# Usage: setup-ct.sh <upload-public-key> <deploy-public-key>
#   (the public halves of the SITE_UPLOAD_KEY / SITE_DEPLOY_KEY secrets)
# Put the GitHub token (Actions: read and write on swenske/Janus) in
# /etc/janus-site/github-token afterwards; the service reads it at start.
set -eu
UPLOAD_KEY="${1:?usage: $0 <upload-public-key> <deploy-public-key>}"
DEPLOY_KEY="${2:?usage: $0 <upload-public-key> <deploy-public-key>}"
HERE="$(cd "$(dirname "$0")" && pwd)"

apt-get update -qq
apt-get install -y -qq rsync curl sudo

for u in janus-site janus-upload janus-deploy; do
  id "$u" >/dev/null 2>&1 || useradd --system --create-home --home-dir "/var/lib/$u" --shell /bin/sh "$u"
done
# Uploads land group-writable in the janus-site group, so the site can
# prune old builds.
usermod -a -G janus-site janus-upload

install -d -o root -g root -m 0755 /srv/janus-site /srv/janus-site/bin
install -d -o janus-deploy -g janus-deploy -m 0755 /srv/janus-site/bin/incoming
install -d -o janus-site -g janus-site -m 2775 /srv/janus-site/data /srv/janus-site/data/schematics
install -d -o janus-upload -g janus-site -m 2775 /srv/janus-site/data/images
install -d -o root -g janus-site -m 0750 /etc/janus-site
[ -f /etc/janus-site/github-token ] || install -o root -g janus-site -m 0640 /dev/null /etc/janus-site/github-token

authorize() { # authorize USER "OPTIONS" KEY
  install -d -o "$1" -g "$1" -m 0700 "/var/lib/$1/.ssh"
  printf '%s %s\n' "$2" "$3" > "/var/lib/$1/.ssh/authorized_keys"
  chown "$1:$1" "/var/lib/$1/.ssh/authorized_keys"
  chmod 0600 "/var/lib/$1/.ssh/authorized_keys"
}
# umask 002: directories rsync creates (--mkpath) stay group-writable, so
# the site can prune them.
authorize janus-upload 'command="umask 002; exec /usr/bin/rrsync -wo /srv/janus-site/data/images",restrict' "$UPLOAD_KEY"
# janus-deploy: rsync into bin/incoming only, then the activation script.
cat > /usr/local/sbin/janus-deploy-shell <<'EOF'
#!/bin/sh
# The only commands janus-deploy may run over SSH.
case "$SSH_ORIGINAL_COMMAND" in
  "rsync --server"*) exec /usr/bin/rrsync -wo /srv/janus-site/bin/incoming ;;
  activate) exec sudo /usr/local/sbin/janus-site-activate ;;
  *) echo "not allowed: $SSH_ORIGINAL_COMMAND" >&2; exit 1 ;;
esac
EOF
chmod 0755 /usr/local/sbin/janus-deploy-shell
authorize janus-deploy 'command="/usr/local/sbin/janus-deploy-shell",restrict' "$DEPLOY_KEY"

install -o root -g root -m 0755 "$HERE/janus-site-activate" /usr/local/sbin/janus-site-activate
echo 'janus-deploy ALL=(root) NOPASSWD: /usr/local/sbin/janus-site-activate' > /etc/sudoers.d/janus-deploy
chmod 0440 /etc/sudoers.d/janus-deploy
visudo -cf /etc/sudoers.d/janus-deploy

install -m 0644 "$HERE/janus-site.service" /etc/systemd/system/janus-site.service
systemctl daemon-reload
systemctl enable janus-site
echo "Set up. Deploy the binary (site-deploy workflow), then: systemctl status janus-site"
