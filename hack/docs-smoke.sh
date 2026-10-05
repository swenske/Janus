#!/usr/bin/env bash
# The docs site as readers get it (make docs-smoke): janus-site built with
# whatever make docs-build / docs-site left in site/backend/docsdist,
# started on 127.0.0.1, and each built channel walked by
# hack/browser/docs-smoke.mjs in the pinned browser image.
#   hack/docs-smoke.sh <browser image> [<site URL>]
# With a site URL, no local janus-site: that site is checked (site-deploy
# runs it against janus.sw-servers.net once deployed).
set -euo pipefail
cd "$(dirname "$0")/.."
image=$1
url=${2:-}
channels=()

if [ -z "$url" ]; then
  port=$((18600 + ${JANUS_TEST_PORT_OFFSET:-0}))
  if curl -s -o /dev/null "http://127.0.0.1:$port/"; then
    echo "docs-smoke: 127.0.0.1:$port is already taken - set JANUS_TEST_PORT_OFFSET" >&2
    exit 1
  fi
  work=$(mktemp -d)
  trap '[ -f "$work/pid" ] && kill "$(cat "$work/pid")" 2>/dev/null; rm -rf "$work"' EXIT
  go build -o "$work/janus-site" ./site/backend
  mkdir -p "$work/data"
  # No GitHub: the docs need none.
  "$work/janus-site" -addr "127.0.0.1:$port" -data-dir "$work/data" -github-api http://127.0.0.1:9 >"$work/log" 2>&1 &
  echo $! >"$work/pid"
  for _ in $(seq 1 50); do
    curl -sf "http://127.0.0.1:$port/healthz" >/dev/null && break
    sleep 0.2
  done
  if ! kill -0 "$(cat "$work/pid")" 2>/dev/null; then
    cat "$work/log" >&2
    exit 1
  fi
  url=http://127.0.0.1:$port
  [ -d site/backend/docsdist/latest ] && channels+=(/docs/)
  [ -d site/backend/docsdist/next ] && channels+=(/docs/next/)
else
  channels=(/docs/ /docs/next/)
fi
if [ ${#channels[@]} -eq 0 ]; then
  echo "docs-smoke: nothing built in site/backend/docsdist - make docs-build first" >&2
  exit 1
fi
for ch in "${channels[@]}"; do
  docker run --rm --network host --ipc host "$image" node docs-smoke.mjs "$url" "$ch"
done
