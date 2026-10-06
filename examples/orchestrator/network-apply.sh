#!/usr/bin/env bash
# A node's network configuration changed safely (docs/private-cloud/
# driving-nodes.md): applied on trial, then confirmed over a new
# connection - proof the node is still reachable. Unconfirmed, the node
# goes back to its previous configuration by itself.
#
#   network-apply.sh ADDRESS NETWORK-CONFIG-JSON [CONFIRM-ADDRESS]
#
# CONFIRM-ADDRESS: where the node answers under the new configuration,
# when it changes the address the orchestrator reaches it at. Same
# environment as janus.sh; os:admin.
set -euo pipefail
address=$1 config=$2 confirm=${3:-$1}
here=$(dirname "$0")
request=$(jq -n --slurpfile config "$config" '{config: $config[0], confirm_timeout_seconds: 60}')
"$here/janus.sh" "$address" NetworkService/NetworkConfigApply "$request" | jq -c .
# grpcurl opens a new connection for each call - the confirmation must
# not come over the one the trial was applied through.
"$here/janus.sh" "$confirm" NetworkService/NetworkConfigConfirm | jq -c .
