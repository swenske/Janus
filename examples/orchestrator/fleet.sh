#!/usr/bin/env bash
# A Janus fleet made with openssl and jq alone
# (docs/private-cloud/trust.md): the root CA - its key goes offline
# once this ran -, an issuing CA for the orchestrator, and the bundle the
# root signs to tell nodes which issuing CAs they accept.
#
#   fleet.sh DIR [VERSION]
#
# Writes root.crt, root.key, issuing.crt, issuing.key and bundle.json in
# DIR; keeps a root or an issuing CA already there. Run it again with a
# higher VERSION after changing the issuing CAs: a node only takes a
# bundle newer than its own.
set -euo pipefail
dir=$1 version=${2:-1}
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$dir"
cd "$dir"

# The root: ECDSA P-256 - the bundle's signature is ECDSA with SHA-256 -,
# self-signed, a CA.
if [ ! -f root.key ]; then
  cat > root.cnf <<'CNF'
[req]
prompt = no
distinguished_name = dn
x509_extensions = root
[dn]
CN = Example fleet root
[root]
basicConstraints = critical,CA:TRUE
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash
CNF
  openssl req -x509 -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -config root.cnf -keyout root.key -out root.crt -days 7300
  rm root.cnf
fi

# The issuing CA the orchestrator signs its clients' certificates with,
# signed by the root.
if [ ! -f issuing.key ]; then
  openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -subj "/CN=Example orchestrator" -keyout issuing.key -out issuing.csr
  printf '%s\n' 'basicConstraints = critical,CA:TRUE,pathlen:0' \
    'keyUsage = critical,keyCertSign,cRLSign' 'subjectKeyIdentifier = hash' > issuing.ext
  "$here/sign.sh" issuing.csr root.crt root.key issuing.ext issuing.crt 17520
  rm -f issuing.csr issuing.ext
fi

# The bundle: the issuing CAs nodes accept, signed by the root over these
# exact bytes.
jq -n --argjson version "$version" --arg issued "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --rawfile issuing issuing.crt \
  '{version: $version, issued: $issued, issuing_cas: [$issuing]}' > payload.json
openssl dgst -sha256 -sign root.key -out payload.sig payload.json
jq -n --arg payload "$(base64 < payload.json | tr -d '\n')" \
  --arg signature "$(base64 < payload.sig | tr -d '\n')" \
  '{payload: $payload, signature: $signature}' > bundle.json
rm payload.json payload.sig
