#!/usr/bin/env bash
# One call to a node's gRPC API with grpcurl (docs/private-cloud/
# driving-nodes.md): the node's certificate checked against its CA, the
# client's presented, the request in JSON - none for an empty one, - to
# read it from stdin.
#
#   janus.sh ADDRESS SERVICE/METHOD [REQUEST-JSON | -]
#   janus.sh 192.0.2.21:9505 SystemService/Version
#
# Environment:
#   JANUS_PROTO       the directory holding janus/v1alpha1/*.proto (the
#                     repository's api/proto, at the nodes' release)
#   JANUS_NODE_CA     the node's CA (PEM) - first-contact.sh or the node's
#                     registration gives it
#   JANUS_CERT        the client certificate and its issuing CA (PEM)
#   JANUS_KEY         its key
#   JANUS_AS_USER, JANUS_AS_ROLES
#                     with a janus:controller certificate: the user the
#                     call is made for, and that user's roles
#   JANUS_SERVERNAME  the name to check the node's certificate for, when
#                     ADDRESS isn't one of the node's own (a NAT)
#   GRPCURL           the grpcurl command (grpcurl)
#
# Streams print one JSON object per message. A refused call exits
# non-zero with the gRPC status on stderr.
set -euo pipefail
address=$1 method=$2
args=(-import-path "$JANUS_PROTO")
for p in "$JANUS_PROTO"/janus/v1alpha1/*.proto; do
  args+=(-proto "janus/v1alpha1/${p##*/}")
done
args+=(-cacert "$JANUS_NODE_CA" -cert "$JANUS_CERT" -key "$JANUS_KEY" -format-error)
[ -n "${JANUS_SERVERNAME:-}" ] && args+=(-servername "$JANUS_SERVERNAME")
if [ -n "${JANUS_AS_USER:-}" ]; then
  args+=(-H "janus-as-user: $JANUS_AS_USER" -H "janus-as-roles: $JANUS_AS_ROLES")
fi
if [ "${3:-}" = - ]; then
  args+=(-d @)
elif [ $# -ge 3 ]; then
  args+=(-d "$3")
fi
exec ${GRPCURL:-grpcurl} "${args[@]}" "$address" "janus.v1alpha1.$method"
