#!/usr/bin/env bash
# The NoCloud user-data of a node the orchestrator creates
# (docs/private-cloud/first-contact.md) - JSON, not cloud-init's.
#
#   user-data.sh fleet FLEET-DIR HOSTNAME
#     The node trusts the fleet from its first boot: the orchestrator
#     reaches it at once, and pins its CA from the console
#     (first-contact.sh).
#   user-data.sh announce FLEET-DIR HOSTNAME REGISTRATION-ADDRESS TOKEN
#     The node announces itself to the orchestrator's registration
#     endpoint (registration-server.py), checked through the fleet's
#     root; the token admits it at once - an unknown one waits for an
#     approval.
set -euo pipefail
mode=$1 fleet=$2 hostname=$3
case $mode in
  fleet)
    jq -n --arg hostname "$hostname" --rawfile root "$fleet/root.crt" --rawfile bundle "$fleet/bundle.json" \
      '{fleet_root_cert: $root, fleet_bundle: ($bundle | fromjson), network: {hostname: $hostname}}'
    ;;
  announce)
    jq -n --arg hostname "$hostname" --arg address "$4" --arg token "$5" --rawfile root "$fleet/root.crt" \
      '{controller_address: $address, controller_ca_cert: $root, controller_fleet_root_cert: $root,
        registration_token: $token, network: {hostname: $hostname}}'
    ;;
  *) echo "usage: user-data.sh fleet|announce FLEET-DIR HOSTNAME [ADDRESS TOKEN]" >&2; exit 2 ;;
esac
