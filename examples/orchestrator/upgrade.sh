#!/usr/bin/env bash
# A node updated to a release (docs/private-cloud/driving-nodes.md):
# the node fetches the bundle itself, checks its signature, writes its
# other slot, and reboots into it - and goes back to the slot it left by
# itself unless HAProxy is healthy on the new one within the timeout.
#
#   upgrade.sh ADDRESS BUNDLE-URL [SHA256]
#
# BUNDLE-URL: a directory holding the release's rootfs.squashfs,
# rootfs.verity, uki-a.efi and uki-b.efi, like a GitHub release's
# download URL. Same environment as janus.sh; os:admin.
# JANUS_INSECURE_SKIP_SIGNATURE_CHECK=1 takes an unsigned bundle - a
# development build, never a release.
set -euo pipefail
address=$1 url=$2 sha256=${3:-}
here=$(dirname "$0")
request=$(jq -n --arg url "$url" --arg sha256 "$sha256" \
  --argjson insecure "$([ "${JANUS_INSECURE_SKIP_SIGNATURE_CHECK:-}" = 1 ] && echo true || echo false)" \
  '{source: {reference: $url, sha256: $sha256, insecure_skip_signature_check: $insecure},
    wait_for_health: true, health_timeout_seconds: 120}')
# The stream's last stage is "rebooting": the node is going down.
"$here/janus.sh" "$address" LifecycleService/Upgrade "$request" | jq -c '{stage, message}'
