#!/usr/bin/env bash
# The Controller's screenshots in the docs and on the landing page, taken
# by Playwright (hack/browser/shots.mjs, what to shoot in
# hack/browser/shots.yaml) against a real Controller and real nodes:
# three janus-local-dev containers - a real janusd and HAProxy each - on a
# private Docker network, added to the Controller, trusting its fleet,
# running examples/haproxy/web.cfg with its two application servers
# answering. Nothing is published on the host and nothing reaches the
# internet: the network is internal, and the Controller's releases come
# from a local file.
#
#   hack/docs-screenshots.sh [write|check] [browser image]
#
# write (the default): docs/assets/screenshots/<id>-{light,dark}.webp,
# rewritten only when more than SHOTS_THRESHOLD of their pixels changed -
# a new run doesn't churn the repository with identical-looking images.
# A colour changed a little - a shade - counts as no change:
# SHOTS_THRESHOLD=-1 rewrites them all.
# check: the same comparison, nothing written in the repository; the
# shots that drifted go to build/screenshots-drift/, and the script
# fails when there's one.
#
# KEEP=1 leaves the environment running afterwards, to look at it.
set -euo pipefail

MODE="${1:-write}"
BROWSER_IMAGE="${2:-janus-browser}"
LOCAL_DEV_IMAGE="${LOCAL_DEV_IMAGE:-janus-local-dev}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/docs/assets/screenshots"
DRIFT="$ROOT/build/screenshots-drift"
case "$MODE" in write | check) ;; *)
  echo "usage: $0 [write|check] [browser image]" >&2
  exit 2
  ;;
esac

# The versions the pages show: fixed, so a new commit doesn't change
# every screenshot.
VERSION=v2026.10.06
P=janus-shots
# examples/haproxy/web.cfg's application servers are 10.0.10.11 and .12:
# the example runs as written.
NET=10.0.10

# The subnet is fixed: one environment per host at a time.
exec 9>/tmp/janus-docs-screenshots.lock
flock -w 3600 9 || {
  echo "docs-screenshots: another run kept the lock for an hour" >&2
  exit 1
}

WORK="$(mktemp -d)"
stop_env() {
  docker rm -f "$P-controller" "$P-app1" "$P-app2" "$P-edge-par-1" "$P-edge-par-2" "$P-edge-par-3" >/dev/null 2>&1 || true
  docker network rm "$P" >/dev/null 2>&1 || true
}
cleanup() {
  if [ -n "${KEEP:-}" ]; then
    echo "docs-screenshots: the environment is still up (docker ps --filter name=$P); remove it with: docker rm -f \$(docker ps -aq --filter name=$P) && docker network rm $P"
  else
    stop_env
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT
stop_env

echo "docs-screenshots: the Controller and janusd, $VERSION"
for bin in dashboardd:./dashboard/backend janusd:./cmd/janusd; do
  (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$WORK/${bin%%:*}" "${bin#*:}")
done

docker network create --internal --subnet "$NET.0/24" --gateway "$NET.1" "$P" >/dev/null

# The application servers web.cfg balances - HAProxy answering "ok" - and
# the Controller's releases: one, the version the nodes run.
mkdir -p "$WORK/releases"
cat >"$WORK/app.cfg" <<'EOF'
global
    log stdout format raw local0
    chroot /
    uid 65534
    gid 65534
defaults
    mode http
    timeout connect 5s
    timeout client 30s
    timeout server 30s
frontend app
    bind :8080
    http-request return status 200 content-type text/plain string "ok\n"
frontend releases
    bind :8081
    http-request return status 200 content-type application/json file /releases/releases.json if { path /releases }
    http-request return status 200 content-type text/plain file /releases/rootfs.squashfs.sha256 if { path_end /rootfs.squashfs.sha256 }
    http-request return status 404
EOF
cat >"$WORK/releases/releases.json" <<EOF
[{"tag_name": "$VERSION", "html_url": "http://$NET.11:8081/releases/tag/$VERSION",
  "published_at": "2026-10-06T08:00:00Z", "prerelease": false, "draft": false,
  "assets": [{"name": "rootfs.squashfs.sha256", "browser_download_url": "http://$NET.11:8081/download/$VERSION/rootfs.squashfs.sha256"}]}]
EOF
printf '%064d\n' 0 >"$WORK/releases/rootfs.squashfs.sha256"
chmod -R a+rX "$WORK"
for i in 1 2; do
  docker run -d --name "$P-app$i" --hostname "app$i" --network "$P" --ip "$NET.1$i" \
    -v "$WORK/app.cfg:/app.cfg:ro" -v "$WORK/releases:/releases:ro" \
    --entrypoint /usr/local/sbin/haproxy "$LOCAL_DEV_IMAGE" -f /app.cfg >/dev/null
done

# The nodes: fixed names, addresses and MACs, so the pages showing them
# don't change from one run to the next.
for i in 1 2 3; do
  docker run -d --name "$P-edge-par-$i" --hostname "edge-par-$i" --network "$P" --ip "$NET.2$i" \
    --mac-address "52:54:00:4a:10:2$i" -v "$WORK/janusd:/usr/local/bin/janusd:ro" "$LOCAL_DEV_IMAGE" >/dev/null
done

docker run -d --name "$P-controller" --hostname janus-controller --network "$P" --ip "$NET.5" \
  -v "$WORK/dashboardd:/dashboardd:ro" --tmpfs /data --tmpfs /secrets \
  --entrypoint /dashboardd "$LOCAL_DEV_IMAGE" \
  -data-dir /data -master-key-file /secrets/master.key -image-factory "" -updater-socket "" \
  -releases-url "http://$NET.11:8081/releases" >/dev/null

# Each node prints its CA and admin credential once, on its first start.
mkdir -p "$WORK/creds"
for i in 1 2 3; do
  n="$P-edge-par-$i"
  for _ in $(seq 1 60); do
    docker logs "$n" 2>&1 | grep -q 'listening on' && break
    sleep 1
  done
  docker logs "$n" 2>&1 | awk -v dir="$WORK/creds" -v n="edge-par-$i" '
    /pki: CA CERTIFICATE/ {f = dir "/" n ".ca"; next}
    /pki: ADMIN CERTIFICATE/ {f = dir "/" n ".admin"; next}
    f && /^-----BEGIN/ {w = 1}
    w {print > f}
    /^-----END/ {w = 0; if (f ~ /\.ca$/) f = ""}'
  [ -s "$WORK/creds/edge-par-$i.ca" ] && [ -s "$WORK/creds/edge-par-$i.admin" ] || {
    echo "docs-screenshots: edge-par-$i printed no credential:" >&2
    docker logs "$n" 2>&1 | tail -20 >&2
    exit 1
  }
done
chmod -R a+rX "$WORK"

mkdir -p "$OUT"
rm -rf "$DRIFT"
mkdir -p "$DRIFT"
ro=""
[ "$MODE" = check ] && ro=":ro"
# Shots are written as this user, not root, so the repository's files
# stay the user's.
docker run --rm --network "$P" --ip "$NET.100" --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$ROOT/hack/browser/shots.mjs:/browser/shots.mjs:ro" -v "$ROOT/hack/browser/images.mjs:/browser/images.mjs:ro" \
  -v "$ROOT/hack/browser/shots.yaml:/browser/shots.yaml:ro" \
  -v "$ROOT/examples:/examples:ro" -v "$ROOT/versions.mk:/versions.mk:ro" -v "$WORK/creds:/creds:ro" \
  -v "$OUT:/screenshots$ro" -v "$DRIFT:/drift" \
  -e SHOTS_MODE="$MODE" -e SHOTS_THRESHOLD="${SHOTS_THRESHOLD:-0.00015}" -e SHOTS_ONLY="${SHOTS_ONLY:-}" \
  -e CONTROLLER="https://$NET.5:8080" -e NODES="edge-par-1=$NET.21,edge-par-2=$NET.22,edge-par-3=$NET.23" \
  "$BROWSER_IMAGE" node shots.mjs
