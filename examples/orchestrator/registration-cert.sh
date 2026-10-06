#!/usr/bin/env bash
# The registration endpoint's certificate (docs/private-cloud/
# first-contact.md): a node provisioned with the fleet's root checks the
# endpoint through it, asking for the name controller.fleet.janus - so a
# certificate for that name, signed by the fleet's issuing CA, works at
# any address.
#
#   registration-cert.sh FLEET-DIR OUT
#
# Writes OUT.crt - the certificate and the issuing CA - and OUT.key.
set -euo pipefail
fleet=$1 out=$2
openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -subj "/CN=controller.fleet.janus" -keyout "$out.key" -out "$out.csr"
printf '%s\n' 'basicConstraints = critical,CA:FALSE' 'keyUsage = critical,digitalSignature' \
  'extendedKeyUsage = serverAuth' 'subjectAltName = DNS:controller.fleet.janus' > "$out.ext"
"$(dirname "$0")/sign.sh" "$out.csr" "$fleet/issuing.crt" "$fleet/issuing.key" "$out.ext" "$out.leaf.crt" 8760
cat "$out.leaf.crt" "$fleet/issuing.crt" > "$out.crt"
rm -f "$out.csr" "$out.ext" "$out.leaf.crt"
