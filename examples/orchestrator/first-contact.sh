#!/usr/bin/env bash
# A node the orchestrator created, trusted from what its console says
# (docs/private-cloud/first-contact.md): its CA's SHA-256 from the serial
# console, its CA from TrustGet - kept only when the two agree, and when
# the node's certificate then checks against it.
#
#   first-contact.sh ADDRESS CONSOLE-LOG OUT-CA
#
# Same environment as janus.sh, without JANUS_NODE_CA: the node trusts
# the fleet already (user-data.sh fleet), so JANUS_CERT can be any
# client certificate of the fleet. Writes the node's CA to OUT-CA.
set -euo pipefail
address=$1 console=$2 out=$3

# The last line wins: a node prints it at every boot, after a CA
# rotation the new one.
want=$(grep -ao "pki: this node's CA: SHA-256 [0-9a-f]\{64\}" "$console" | tail -1 | awk '{print $NF}')
[ -n "$want" ] || { echo "first-contact: the console shows no CA yet" >&2; exit 1; }

# The first call can't check the node yet: nothing the answer says is
# believed before its hash matches the console's.
${GRPCURL:-grpcurl} -insecure -cert "$JANUS_CERT" -key "$JANUS_KEY" \
  -import-path "$JANUS_PROTO" -proto janus/v1alpha1/access.proto -format-error \
  "$address" janus.v1alpha1.AccessService/TrustGet > "$out.trust.json"
# bytes fields are base64 in JSON.
jq -r '.localCaCert' "$out.trust.json" | base64 -d > "$out.candidate"
got=$(openssl x509 -in "$out.candidate" -outform DER | sha256sum | awk '{print $1}')
rm -f "$out.trust.json"
if [ "$got" != "$want" ]; then
  rm -f "$out.candidate"
  echo "first-contact: $address answers with CA $got, its console shows $want - not the node" >&2
  exit 1
fi

# The same call, checked: the certificate the node serves is its CA's.
JANUS_NODE_CA="$out.candidate" "$(dirname "$0")/janus.sh" "$address" AccessService/TrustGet > /dev/null
mv "$out.candidate" "$out"
echo "first-contact: $address's CA is $want"
