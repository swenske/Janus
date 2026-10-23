#!/usr/bin/env bash
# A Controller and three nodes to look at in a browser - and with
# janusctl tui -, live: dashboardd and janusd built from this tree (its
# committed frontend build: rebuild it first, make
# dashboard-frontend-build), three janus-local-dev containers (a real
# janusd and HAProxy each) running hack/ui-preview/node.cfg - HTTP and
# TCP frontends, HTTPS with HTTP/2 - under traffic that comes and goes
# (hack/ui-preview/load.mjs), sending their logs to rsyslog and
# Elasticsearch (hack/rsyslog, hack/elasticsearch); edge-1 has a second
# interface, the one its rsyslog target goes by; edge-3 runs many.cfg
# instead, two dozen backends. Set up through the
# Controller's API (hack/ui-preview/setup.mjs), with a second account,
# lea's, on edge-1's page now and then, and a janusctl of its own
# following edge-1's events: the Traffic flow's Controller and direct
# clients.
#
#   hack/ui-preview.sh [up|reload [ui]|check|down|info]
#
# up (the default): the environment from scratch - the Controller at
# https://127.0.0.1:18443 (UI_PREVIEW_PORT), its address, account and
# janusctl commands printed (info prints them again). From another
# machine, through ssh: ssh -N -L 18443:127.0.0.1:18443 HOST - or
# published on one of this machine's addresses: UI_PREVIEW_BIND=ADDRESS
# (0.0.0.0: all of them), UI_PREVIEW_HOST=NAME the name it's opened by.
# Both are read by up only: the port's mapping and the names in the
# Controller's certificate are fixed then (reload and info reuse them).
# reload: the binaries built again and the Controller and nodes
# restarted - their state kept (the Controller's data, the nodes' PKI and
# configuration): to see a change of the tree. reload ui: dashboardd
# only, the Controller alone restarted - a frontend change, rebuilt
# first (cd dashboard/frontend && npm run build).
# check: its pages walked in the browser image (hack/ui-preview/
# check.mjs) - every palette in both modes, axe, no error - and their
# screenshots in build/ui-preview/check.
# down: everything removed.
#
# Nothing reaches the internet: the networks are internal; only the
# Controller's port is published, on the loopback unless UI_PREVIEW_BIND
# says otherwise - its passwords are fixed: anyone on that network signs
# in.
set -euo pipefail

CMD="${1:-up}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="$ROOT/build/ui-preview"
P=janus-preview
NET=10.0.30    # the nodes, application servers, Controller, Elasticsearch
LOGNET=10.0.31 # edge-1's second interface, rsyslog's
PORT="${UI_PREVIEW_PORT:-18443}"
BIND="${UI_PREVIEW_BIND:-127.0.0.1}"
HOST="${UI_PREVIEW_HOST:-}"
# What up chose, for reload and info.
if [ "$CMD" != up ] && [ -s "$ROOT/build/ui-preview/env" ]; then
  # shellcheck source=/dev/null
  . "$ROOT/build/ui-preview/env"
fi
CONNECT="$BIND"                            # where this machine reaches the Controller
[ "$CONNECT" != 0.0.0.0 ] || CONNECT=127.0.0.1
PUBLIC="${HOST:-$CONNECT}"                 # what a browser opens
LOCAL_DEV_IMAGE="${LOCAL_DEV_IMAGE:-janus-local-dev}"
BROWSER_IMAGE="${BROWSER_IMAGE:-janus-browser}"
RSYSLOG_IMAGE=janus-rsyslog-test
ES_IMAGE=janus-es-test
PASSWORD=ui-preview-admin-password
VISITOR_PASSWORD=ui-preview-lea-password
NODES="edge-1=$NET.21,edge-2=$NET.22,edge-3=$NET.23"
CONTAINERS=(controller app1 app2 edge-1 edge-2 edge-3 rsyslog elastic load cli)

down() {
  for c in "${CONTAINERS[@]}"; do docker rm -f "$P-$c" >/dev/null 2>&1 || true; done
  for n in "$P" "$P-logs" "$P-pub"; do docker network rm "$n" >/dev/null 2>&1 || true; done
}

build() {
  mkdir -p "$DIR/bin"
  local version
  version="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
  # janus-acme next to dashboardd, as in the Controller's image: what it
  # orders its nodes' certificates with.
  for bin in ${1:-dashboardd:./dashboard/backend janus-acme:./cmd/janus-acme janusd:./cmd/janusd janusctl:./cmd/janusctl}; do
    # Built aside, then moved: the containers see the directory, a new
    # file in it at their next start.
    (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$DIR/bin/.${bin%%:*}" "${bin#*:}")
    mv "$DIR/bin/.${bin%%:*}" "$DIR/bin/${bin%%:*}"
  done
}

info() {
  [ -s "$DIR/token" ] || {
    echo "ui-preview: not set up - hack/ui-preview.sh up" >&2
    exit 1
  }
  local fp
  fp="$(fingerprint)"
  cat <<EOF

The Controller: https://$PUBLIC:$PORT  (a self-signed certificate: SHA-256 $fp)
  sign in as admin, password $PASSWORD (or lea, an operator: $VISITOR_PASSWORD)
  the Traffic flow: a node's page, Monitoring › Traffic flow (edge-1: two interfaces, two log targets)
  many backends: edge-3's page, Apps › HAProxy › Backends

janusctl tui, from this machine:
  export JANUSCONFIG=$DIR/janusctl.json
  JANUS_TOKEN=$(cat "$DIR/token") $DIR/bin/janusctl login -controller $CONNECT:$PORT -controller-fingerprint $fp
  $DIR/bin/janusctl -n edge-1 tui     # f: the traffic flow

hack/ui-preview.sh reload - after a change (reload ui: a frontend change); down - to remove it all.
EOF
  if [ "$CONNECT" != 127.0.0.1 ]; then
    echo "Published on $BIND: these passwords are fixed - anyone on that network signs in."
  fi
}

fingerprint() {
  openssl s_client -connect "$CONNECT:$PORT" </dev/null 2>/dev/null | openssl x509 -outform der | sha256sum | cut -d' ' -f1
}

# Each node prints its CA and admin credential once, on its first start.
creds() {
  mkdir -p "$DIR/creds"
  for i in 1 2 3; do
    local n="$P-edge-$i"
    for _ in $(seq 1 60); do
      docker logs "$n" 2>&1 | grep -q 'listening on' && break
      sleep 1
    done
    docker logs "$n" 2>&1 | awk -v dir="$DIR/creds" -v n="edge-$i" '
      /pki: CA CERTIFICATE/ {f = dir "/" n ".ca"; next}
      /pki: ADMIN CERTIFICATE/ {f = dir "/" n ".admin"; next}
      f && /^-----BEGIN/ {w = 1}
      w {print > f}
      /^-----END/ {w = 0; if (f ~ /\.ca$/) f = ""}'
    [ -s "$DIR/creds/edge-$i.ca" ] && [ -s "$DIR/creds/edge-$i.admin" ] || {
      echo "ui-preview: edge-$i printed no credential:" >&2
      docker logs "$n" 2>&1 | tail -20 >&2
      exit 1
    }
  done
}

up() {
  for image in "$LOCAL_DEV_IMAGE" "$BROWSER_IMAGE"; do
    docker image inspect "$image" >/dev/null 2>&1 || {
      echo "ui-preview: no $image image - make local-dev-image browser-image" >&2
      exit 1
    }
  done
  down
  rm -rf "$DIR/controller" "$DIR/creds" "$DIR/token" "$DIR/janusctl.json" "$DIR"/janusctl
  mkdir -p "$DIR/preview" "$DIR/controller" "$DIR/rsyslog/out"
  printf 'BIND=%q\nHOST=%q\n' "$BIND" "$HOST" >"$DIR/env"
  echo "ui-preview: building dashboardd, janus-acme, janusd, janusctl"
  build
  docker build -q -t "$RSYSLOG_IMAGE" "$ROOT/hack/rsyslog" >/dev/null
  docker build -q -t "$ES_IMAGE" "$ROOT/hack/elasticsearch" >/dev/null

  docker network create --internal --subnet "$NET.0/24" --gateway "$NET.1" "$P" >/dev/null
  docker network create --internal --subnet "$LOGNET.0/24" --gateway "$LOGNET.1" "$P-logs" >/dev/null
  docker network create "$P-pub" >/dev/null

  # Elasticsearch first: it takes the longest to start.
  docker run -d --name "$P-elastic" --hostname elastic --network "$P" --ip "$NET.30" \
    -e discovery.type=single-node -e ELASTIC_PASSWORD="$PASSWORD" -e ES_JAVA_OPTS="-Xms1g -Xmx1g" -e xpack.ml.enabled=false \
    "$ES_IMAGE" >/dev/null

  # rsyslog on both networks: UDP 514, TCP 601, TLS 6514.
  local conf="$DIR/rsyslog"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj "/CN=ui-preview logs CA" \
    -keyout "$conf/ca.key" -out "$conf/ca.pem" >/dev/null 2>&1
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj "/CN=rsyslog" -CA "$conf/ca.pem" -CAkey "$conf/ca.key" \
    -keyout "$conf/server.key" -out "$conf/server.pem" >/dev/null 2>&1
  sed -e "s/@UDP@/514/; s/@TCP@/601/; s/@TLS@/6514/" "$ROOT/hack/rsyslog/rsyslog.conf.in" >"$conf/rsyslog.conf"
  chmod 644 "$conf"/*.pem "$conf"/*.key "$conf/rsyslog.conf"
  chmod 777 "$conf/out"
  docker create --name "$P-rsyslog" --hostname rsyslog --network "$P-logs" --ip "$LOGNET.20" \
    -v "$conf:/etc/rsyslog-test:ro" -v "$conf/out:/out" "$RSYSLOG_IMAGE" >/dev/null
  docker network connect --ip "$NET.20" "$P" "$P-rsyslog"
  docker start "$P-rsyslog" >/dev/null

  # The application servers, and the Controller's releases: one, the
  # version the nodes run.
  local version
  version="$(git -C "$ROOT" describe --tags --abbrev=0 2>/dev/null || echo v2026.10.06)"
  cp "$ROOT/hack/ui-preview/app.cfg" "$ROOT/hack/ui-preview/node.cfg" "$DIR/preview/"
  head -c 9000 /dev/urandom | base64 -w 76 >"$DIR/preview/payload"
  cat >"$DIR/preview/releases.json" <<EOF
[{"tag_name": "$version", "html_url": "http://$NET.11:8081/releases/tag/$version",
  "published_at": "2026-10-06T08:00:00Z", "prerelease": false, "draft": false, "assets": []}]
EOF
  chmod -R a+rX "$DIR/preview"
  for i in 1 2; do
    docker run -d --name "$P-app$i" --hostname "app$i" --network "$P" --ip "$NET.1$i" \
      -v "$DIR/preview:/preview:ro" --entrypoint /usr/local/sbin/haproxy "$LOCAL_DEV_IMAGE" -f /preview/app.cfg >/dev/null
  done

  # The nodes: edge-1 on both networks (eth0, eth1).
  for i in 1 2 3; do
    docker create --name "$P-edge-$i" --hostname "edge-$i" --network "$P" --ip "$NET.2$i" \
      -v "$DIR/bin:/opt/janus:ro" --entrypoint /opt/janus/janusd "$LOCAL_DEV_IMAGE" >/dev/null
  done
  docker network connect --ip "$LOGNET.21" "$P-logs" "$P-edge-1"
  for i in 1 2 3; do docker start "$P-edge-$i" >/dev/null; done

  # The Controller: its port on this machine's loopback only, unless
  # UI_PREVIEW_BIND. Its certificate names the addresses and name it's
  # opened by: from inside its container it sees only the container's.
  local names=127.0.0.1
  [ "$CONNECT" = 127.0.0.1 ] || names="$names,$CONNECT"
  [ -z "$HOST" ] || names="$names,$HOST"
  docker create --name "$P-controller" --hostname janus-controller --network "$P-pub" -p "$BIND:$PORT:8080" \
    --user "$(id -u):$(id -g)" -v "$DIR/bin:/opt/janus:ro" -v "$DIR/controller:/data" \
    --entrypoint /opt/janus/dashboardd "$LOCAL_DEV_IMAGE" \
    -data-dir /data -master-key-file /data/master.key -image-factory "" -updater-socket "" \
    -advertise-address "$names" -releases-url "http://$NET.11:8081/releases" >/dev/null
  docker network connect --ip "$NET.5" "$P" "$P-controller"
  docker start "$P-controller" >/dev/null

  creds
  # The nodes' HTTPS certificate (node.cfg's web-tls), key included.
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj "/CN=edge" \
    -keyout "$DIR/creds/tls.key" -out "$DIR/creds/tls.crt" >/dev/null 2>&1
  cat "$DIR/creds/tls.crt" "$DIR/creds/tls.key" >"$DIR/creds/tls.pem"

  echo "ui-preview: waiting for Elasticsearch"
  local deadline=$((SECONDS + 240)) es_ca="$DIR/es-ca.crt"
  until docker exec "$P-elastic" cat /usr/share/elasticsearch/config/certs/http_ca.crt >"$es_ca" 2>/dev/null && [ -s "$es_ca" ]; do
    [ "$SECONDS" -lt "$deadline" ] || { echo "ui-preview: Elasticsearch made no HTTP CA" >&2; exit 1; }
    sleep 2
  done
  # Its certificate names localhost: asked as that, at its address.
  until curl -sf -o /dev/null --cacert "$es_ca" -u "elastic:$PASSWORD" --resolve "localhost:9200:$NET.30" "https://localhost:9200/_security/_authenticate"; do
    [ "$SECONDS" -lt "$deadline" ] || { echo "ui-preview: Elasticsearch didn't start" >&2; exit 1; }
    sleep 2
  done
  local key
  key="$(curl -sf --cacert "$es_ca" -u "elastic:$PASSWORD" --resolve "localhost:9200:$NET.30" -H 'Content-Type: application/json' \
    -X POST "https://localhost:9200/_security/api_key" \
    -d '{"name":"janus-ui-preview","role_descriptors":{"janus":{"index":[{"names":["logs-janus*"],"privileges":["create_doc","auto_configure"]}]}}}' |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["encoded"])')"
  local es_fp
  es_fp="$(openssl x509 -in "$es_ca" -noout -fingerprint -sha256 | cut -d= -f2)"

  echo "ui-preview: setting the Controller up"
  docker run --rm --network "$P" --ip "$NET.100" --user "$(id -u):$(id -g)" -e HOME=/tmp -e NODE_TLS_REJECT_UNAUTHORIZED=0 \
    -v "$ROOT/hack/ui-preview:/preview:ro" -v "$DIR/creds:/creds:ro" \
    -e CONTROLLER="https://$NET.5:8080" -e NODES="$NODES" -e NET="$NET" -e LOGNET="$LOGNET" -e PASSWORD="$PASSWORD" -e VISITOR_PASSWORD="$VISITOR_PASSWORD" \
    -e ES_URL="https://$NET.30:9200" -e ES_KEY="$key" -e ES_FP="$es_fp" \
    "$BROWSER_IMAGE" node /preview/setup.mjs >"$DIR/setup.json"
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["token"])' "$DIR/setup.json" >"$DIR/token"

  local edge1
  edge1="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["nodes"]["edge-1"])' "$DIR/setup.json")"
  docker run -d --name "$P-load" --network "$P" --ip "$NET.101" --user "$(id -u):$(id -g)" -e HOME=/tmp -e NET="$NET" \
    -e CONTROLLER="https://$NET.5:8080" -e VISIT_NODE="$edge1" -e VISITOR_PASSWORD="$VISITOR_PASSWORD" -e NODE_TLS_REJECT_UNAUTHORIZED=0 \
    -v "$ROOT/hack/ui-preview:/preview:ro" "$BROWSER_IMAGE" node --no-warnings /preview/load.mjs >/dev/null

  # A janusctl of its own on edge-1: its events followed, a call now and
  # then - signed in with the API token, renewed with it.
  local fp
  fp="$(fingerprint)"
  docker run -d --name "$P-cli" --network "$P" --ip "$NET.102" --user "$(id -u):$(id -g)" -e HOME=/tmp -e JANUSCONFIG=/tmp/janusctl.json \
    -e JANUS_TOKEN="$(cat "$DIR/token")" -v "$DIR/bin:/opt/janus:ro" --entrypoint sh "$LOCAL_DEV_IMAGE" -c "
      until /opt/janus/janusctl login -controller $NET.5:8080 -controller-fingerprint $fp; do sleep 5; done
      while :; do /opt/janus/janusctl -n edge-1 system events >/dev/null 2>&1; sleep 5; done &
      while :; do /opt/janus/janusctl -n edge-1 haproxy show-info >/dev/null 2>&1; sleep 20; done" >/dev/null
  info
}

reload() {
  docker inspect "$P-controller" >/dev/null 2>&1 || {
    echo "ui-preview: not running - hack/ui-preview.sh up" >&2
    exit 1
  }
  if [ "${1:-}" = ui ]; then
    echo "ui-preview: building dashboardd"
    build dashboardd:./dashboard/backend
    docker restart "$P-controller" >/dev/null
    echo "ui-preview: the Controller restarted"
    return
  fi
  echo "ui-preview: building dashboardd, janus-acme, janusd, janusctl"
  build
  docker restart "$P-controller" "$P-edge-1" "$P-edge-2" "$P-edge-3" "$P-load" "$P-cli" >/dev/null
  echo "ui-preview: the Controller, the nodes and their traffic restarted"
}

check() {
  docker inspect "$P-controller" >/dev/null 2>&1 || {
    echo "ui-preview: not running - hack/ui-preview.sh up" >&2
    exit 1
  }
  rm -rf "$DIR/check"
  mkdir -p "$DIR/check"
  docker run --rm --network "$P" --ip "$NET.103" --user "$(id -u):$(id -g)" -e HOME=/tmp -e NODE_TLS_REJECT_UNAUTHORIZED=0 \
    -v "$ROOT/hack/ui-preview/check.mjs:/browser/check.mjs:ro" -v "$DIR/check:/out" \
    -e CONTROLLER="https://$NET.5:8080" -e PASSWORD="$PASSWORD" -e OUT=/out ${PALETTES:+-e PALETTES="$PALETTES"} \
    "$BROWSER_IMAGE" node --no-warnings check.mjs
}

case "$CMD" in
up) up ;;
reload) reload "${2:-}" ;;
check) check ;;
down) down ;;
info) info ;;
*)
  echo "usage: $0 [up|reload [ui]|check|down|info]" >&2
  exit 2
  ;;
esac
