#!/usr/bin/env bash
# A node's HAProxy configuration replaced (docs/private-cloud/
# driving-nodes.md): the node has HAProxy check it, then a new HAProxy
# process takes over without dropping a connection. A configuration it
# refuses changes nothing.
#
#   haproxy-apply.sh ADDRESS HAPROXY-CFG
#
# Same environment as janus.sh; a role of os:operator or more.
set -euo pipefail
address=$1 cfg=$2
here=$(dirname "$0")
request=$(jq -n --arg config "$(base64 < "$cfg" | tr -d '\n')" '{config: $config}')
stages=$("$here/janus.sh" "$address" HAProxyService/ApplyConfig "$request" | jq -c .)
echo "$stages"
# Every stage streams; the last says whether it was accepted.
if [ "$(tail -1 <<<"$stages" | jq -r '.accepted // false')" != true ]; then
  echo "haproxy-apply: $address refused the configuration" >&2
  exit 1
fi
