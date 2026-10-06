#!/usr/bin/env bash
# A client certificate nodes accept (docs/private-cloud/trust.md): its
# role in the subject's O, its name in CN - what a node logs for each
# change -, for client authentication, signed by the fleet's issuing CA.
# Short-lived: issue one when it's needed rather than renew it.
#
#   client-cert.sh FLEET-DIR NAME ROLE OUT [HOURS]
#
# ROLE: os:admin, os:operator or os:reader - or janus:controller, which
# acts for the user each call names. Writes OUT.crt - the certificate and
# the issuing CA, which the node doesn't know - and OUT.key.
set -euo pipefail
fleet=$1 name=$2 role=$3 out=$4 hours=${5:-12}

openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -subj "/O=$role/CN=$name" -keyout "$out.key" -out "$out.csr"
printf '%s\n' 'basicConstraints = critical,CA:FALSE' 'keyUsage = critical,digitalSignature' \
  'extendedKeyUsage = clientAuth' > "$out.ext"
"$(dirname "$0")/sign.sh" "$out.csr" "$fleet/issuing.crt" "$fleet/issuing.key" "$out.ext" "$out.leaf.crt" "$hours"
cat "$out.leaf.crt" "$fleet/issuing.crt" > "$out.crt"
rm -f "$out.csr" "$out.ext" "$out.leaf.crt"
