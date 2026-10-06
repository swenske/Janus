# Certificates and the fleet

The certificates a Janus node accepts, as a specification: the fleet's
root and the bundle it signs, issuing CAs, client certificates and their
roles - made here with openssl, or by any PKI that meets them.

Every call to a node is a TLS 1.3 connection both sides authenticate.
A node accepts a client certificate from two places:

- **its own CA**, made on its first boot - its first-boot admin
  certificate comes from it. Each node has its own: an orchestrator
  doesn't use it;
- **its fleet**: a root the node pins once, and the issuing CAs a
  bundle signed by that root lists. One fleet for all your nodes: your
  orchestrator's certificates open every node that trusts it.

How the two fit together, and what the Controller does with them:
[trust and certificates](../internals/trust.md).

## The fleet

| Piece | Requirements |
|---|---|
| **Root** | A self-signed CA certificate (`basicConstraints` CA, `keyCertSign`) with an **ECDSA** key - the bundle's signature is ECDSA with SHA-256; P-256 is what Janus uses. Long-lived: a node pins it once and never takes another. Its key only signs issuing CAs and bundles - keep it offline, or in an HSM. |
| **Issuing CAs** | CA certificates signed **directly by the root** - no CA in between. They sign the client certificates. The bundle lists them: a CA the root signed but no bundle lists opens nothing. |
| **Bundle** | The list of issuing CAs nodes accept, signed by the root, with a version that only grows. |

`fleet.sh` makes all three with `openssl` and `jq`:

```sh title="examples/orchestrator/fleet.sh"
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
```

### The bundle's format

A bundle is a JSON object of two base64 strings (standard alphabet, with
padding):

```json
{
  "payload": "<base64 of the payload's bytes>",
  "signature": "<base64 of the root's signature over those bytes>"
}
```

- **The signature** is ECDSA over the SHA-256 of the payload's exact
  bytes, encoded in ASN.1 DER (`SEQUENCE { r INTEGER, s INTEGER }`) -
  what `openssl dgst -sha256 -sign` writes. A signer that returns raw
  `r || s` (some HSM and KMS APIs) must be converted to DER first.
- **The payload** is a JSON object - checked as the bytes it is, never
  re-serialized:

| Field | Type | |
|---|---|---|
| `version` | integer, more than 0 | A node takes a bundle only if its version is higher than the one it has - the same version is accepted again only byte for byte. Use a counter or a timestamp (Janus uses the Unix time in milliseconds). |
| `issued` | string, RFC 3339 | When it was signed - shown, not checked. |
| `issuing_cas` | array of strings | The issuing CAs' certificates, PEM, one per string. Each must be a CA the root signed. |
| `limits` | object, optional | By issuing CA - the lowercase hex SHA-256 of its certificate's DER - the roles its certificates may carry, e.g. `{"3f2a...": ["os:operator", "os:reader"]}`. A CA without a limit may sign any role. |

### Giving nodes the fleet

A node learns its fleet's root and its first bundle once, at its first
boot or when it's adopted, and newer bundles after:

| When | How |
|---|---|
| A node your orchestrator creates | its NoCloud volume's `fleet_root_cert` and `fleet_bundle` ([first contact](first-contact.md#a-node-you-create)) |
| A node that announces itself | your registration endpoint's answer ([first contact](first-contact.md#the-node-announces-itself)) |
| A running node | `AccessService/TrustSet` - the root (the first time) and the bundle, with an `os:admin` certificate the node already accepts |
| A new bundle - an issuing CA added, removed or limited | `TrustSet` on every node, with the bundle only: nodes that miss it keep the one they have |

There's no revocation list: a compromised issuing CA is removed by a new
bundle without it, given to every node; the certificates it signed are
short-lived.

## Client certificates

| Field | Value |
|---|---|
| Subject Organization (`O`) | The role: `os:reader`, `os:operator`, `os:admin` - or `janus:controller` ([acting for a user](#acting-for-a-user)) |
| Subject Common Name (`CN`) | Who it is: the node logs it with every change it makes (`api: HAProxyService/ApplyConfig: orchestrator (os:admin, fleet)`) |
| Issuer | An issuing CA the node's bundle lists - within its limits - or the root itself |
| Extended key usage | `clientAuth`, or none; an issuing CA with an extended key usage must include `clientAuth` too |
| Key | Any key Go's TLS accepts - ECDSA, RSA, Ed25519; Janus's own certificates and tests use ECDSA P-256 |
| Validity | Short - hours. The node checks it with its own clock: start it a few minutes in the past |
| Sent with | Its issuing CA: the node knows only the root, so the client presents the certificate and its issuing CA together |

`client-cert.sh` issues one:

```sh title="examples/orchestrator/client-cert.sh"
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
```

It signs with `sign.sh`, which starts the certificate five minutes in
the past - `openssl ca`, since `openssl x509` only takes a start date
from OpenSSL 3.4 on:

```sh title="examples/orchestrator/sign.sh"
#!/usr/bin/env bash
# Signs a certificate request with a CA, valid from five minutes ago - a
# node whose clock is a little behind still takes it. openssl ca rather
# than openssl x509 -req: it sets the start date since OpenSSL 1.1.1.
#
#   sign.sh REQUEST.csr CA.crt CA.key EXTENSIONS-FILE OUT.crt HOURS
#
# EXTENSIONS-FILE: the certificate's extensions, one per line, in
# openssl's configuration syntax.
set -euo pipefail
csr=$1 ca=$2 cakey=$3 ext=$4 out=$5 hours=$6
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
: > "$work/index"
cat > "$work/ca.cnf" <<CNF
[ca]
default_ca = sign
[sign]
database = $work/index
new_certs_dir = $work
rand_serial = yes
default_md = sha256
unique_subject = no
policy = subject
x509_extensions = extensions
[subject]
commonName = supplied
organizationName = optional
[extensions]
$(cat "$ext")
CNF
if ! openssl ca -batch -notext -preserveDN -config "$work/ca.cnf" -cert "$ca" -keyfile "$cakey" \
  -startdate "$(date -u -d '-5 minutes' +%Y%m%d%H%M%SZ)" \
  -enddate "$(date -u -d "+$hours hours" +%Y%m%d%H%M%SZ)" \
  -in "$csr" -out "$out" 2>"$work/log"; then
  cat "$work/log" >&2
  exit 1
fi
```

### Roles

| Role | May |
|---|---|
| `os:reader` | read the node's state and its configurations (but Consul's, which may hold secrets) - not the node's own files, its logs or its traffic |
| `os:operator` | run what's set up: HAProxy's configuration and runtime state (servers, maps, ACLs, certificates, files, ACME), services and their logs, reboots |
| `os:admin` | everything: the network, the firewall, VRRP, BGP, Consul, updates, the fleet's trust, packet captures, files, a reset |

Each call's role is in the [API reference](api-reference.md).

### Acting for a user

An orchestrator with users of its own - tenants, a portal, operators -
can have each call checked against the user's role, by the node: its
certificate carries `O=janus:controller`, which opens nothing by itself,
and each call names the user in its gRPC metadata.

| Metadata | Value |
|---|---|
| `janus-as-user` | The user's name - 1 to 64 characters: letters, digits, space, `. _ @ : + -`, starting with a letter or a digit. The node logs it. |
| `janus-as-roles` | The user's roles, comma-separated: `os:reader`, `os:operator`, `os:admin` |
| `janus-as-domains` | Optional, once: what the user may touch, comma-separated - `haproxy`, `services`, `network`, `system` ([domains](api-reference.md)) |

A call without them is refused; a call beyond the user's role is
refused with the user's name in the message (`... requires role
[os:admin os:operator], alice has [os:reader]`). An issuing CA limited
to some roles can't sign a `janus:controller` certificate unless its
limits name `janus:controller`.

The Controller also issues **scoped** certificates (`O=janus:scoped`,
with an extension listing permissions per node); their extension's OID
is provisional and its format may change - not for other issuers yet.

## The node's certificate

A node serves a certificate its own CA signs, for its hostname,
`localhost`, `127.0.0.1` and its addresses - reissued when they change,
and renewed 30 days before it expires. A client verifies it against the
node's CA, which it pins at [first contact](first-contact.md); through a
NAT or a port forward, verify it for one of the node's own names (a
server-name override - `grpcurl -servername`, `ServerName` in most TLS
libraries).

When an admin replaces a node's CA (`AccessService/LocalCARotate`), the
node serves its new CA cross-signed by the old one after its
certificate: a client pinning the old CA still verifies it. Read the new
CA from `AccessService/TrustGet` (`local_ca_cert`) and pin it - the
Controller does it on its next connection.

## With your own PKI

Whatever issues the certificates - an internal CA, Vault, a cloud's CA
service - nodes need:

- **a dedicated root**, self-signed, ECDSA: the node accepts any client
  certificate it signs directly, and trusts nothing above it - a
  company's root that signs other things isn't one;
- **issuing CAs one level below it**, listed in a bundle - a deeper
  hierarchy (root, intermediate, issuing CA) isn't accepted;
- **the bundle signed by the root's key**, ECDSA with SHA-256 over the
  payload's bytes, DER-encoded - an HSM or a KMS holding the root's key
  can sign it;
- **client certificates** with the role in `O`, sent with their issuing
  CA.
