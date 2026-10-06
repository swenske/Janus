# First contact

How a custom orchestrator learns a new Janus node and comes to trust it:
the node's CA read from its serial console, or the node announcing
itself to a registration endpoint - both specified here.

A node makes its own certificate authority on its first boot, and serves
its API with a certificate that CA signs. Before anything else, your
orchestrator needs that CA - to know it's talking to the node, and not
to whatever answers at its address - and the node needs to trust your
fleet. Two ways, each tested on real nodes by `make
qemu-orchestrator-test`:

| | A node you create | The node announces itself |
|---|---|---|
| For | virtual machines your orchestrator creates | machines that show up - bare metal, a VM made elsewhere |
| The node learns the fleet | from its NoCloud volume | from your registration endpoint's answer |
| You learn the node's CA | from its serial console, then the node itself | from its announcement - checked against its console, or vouched for by a token you gave it |
| You serve | nothing | an HTTPS endpoint |

## A node you create

### Its NoCloud volume

The node's first boot reads a `cidata` volume ([first
boot](first-boot.md#the-nocloud-volume)): with `fleet_root_cert` and
`fleet_bundle`, the node trusts your fleet from that boot on, and your
orchestrator's certificates open it at once.

```sh title="examples/orchestrator/user-data.sh"
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
```

Attach it as a CD-ROM - libvirt's `<disk device='cdrom'>`, Proxmox's
cloud-init drive slot, any hypervisor's ISO drive:

```sh title="examples/orchestrator/cidata.sh"
#!/usr/bin/env bash
# The NoCloud volume - an ISO 9660 image labelled cidata, attached to the
# node's virtual machine as a CD-ROM (an ISO image read as a disk isn't
# found: its sectors aren't a CD's).
#
#   user-data.sh ... | cidata.sh OUT.iso
set -euo pipefail
out=$1
seed=$(mktemp -d)
trap 'rm -rf "$seed" "$seed.log"' EXIT
cat > "$seed/user-data"
: > "$seed/meta-data"
if ! xorriso -as mkisofs -V cidata -J -r -o "$out" "$seed" 2>"$seed.log"; then
  cat "$seed.log" >&2
  exit 1
fi
```

### Its CA, from its console

On every boot, janusd prints one line on the serial console with the
SHA-256 of its CA's certificate (DER), in lowercase hex:

```text
pki: this node's CA: SHA-256 2bcdf13249a8cbdedcead005d4b309cb34229a73e4c355b6a5028d69d9111ae5
```

The line matches `pki: this node's CA: SHA-256 ([0-9a-f]{64})` and
doesn't change format - a test holds it. It comes with a timestamp in
front and among other lines; when there are several, the last one is
the current CA. Your orchestrator reads the console the way its
hypervisor offers - a serial port logged to a file (libvirt `<serial
type='file'>`, VMware's serial port to a file), libvirt's or Proxmox's
console API.

Then it asks the node for its CA - `AccessService/TrustGet`'s
`local_ca_cert` - and keeps it only if its SHA-256 is the console's:

```sh title="examples/orchestrator/first-contact.sh"
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
```

That first call can't verify the node: nothing it answers is believed
until its hash matches what the console - which only the node writes -
shows. It gives nothing away either: TLS client authentication proves
the orchestrator holds its key without sending it. The second call,
verified with the CA, proves the node serves a certificate that CA
signed.

From then on, every call verifies the node with that CA
([driving nodes](driving-nodes.md)).

## The node announces itself

A node provisioned with a registration endpoint - in its NoCloud
volume, or by the installer - announces itself there on its first boot:
its name, its address, its CA, never a key. Your endpoint admits it at
once for a token you gave it, or holds it until someone approves it;
an admitted node gets your fleet's trust in the answer, and your
orchestrator reaches it with its fleet certificates.

The NoCloud `user-data` for it (`user-data.sh announce` above):

| Key | |
|---|---|
| `controller_address` | Your endpoint, `host:port` |
| `controller_ca_cert` | A CA certificate (PEM) the endpoint's certificate chains to, for the `host` of `controller_address` - required with `controller_address` |
| `controller_fleet_root_cert` | Optional: your fleet's root. The node then checks the endpoint against it first, asking for the name `controller.fleet.janus` - one certificate for that name, from your fleet, works at any address - and refuses an answer that hands it another fleet |
| `registration_token` | Optional: any string your endpoint recognizes - an orchestrator puts one in each VM's volume to admit its node at once |

### The protocol

HTTPS (TLS 1.2 or later), JSON bodies.

**`POST /register`** - the announcement:

```json
{
  "protocol": 2,
  "name": "edge-2",
  "address": "10.0.2.15:9505",
  "ca_cert_pem": "-----BEGIN CERTIFICATE-----\n...",
  "registration_token": "the token, when the node was given one",
  "poll_secret": "64 hex characters the node made"
}
```

| Field | |
|---|---|
| `protocol` | 2 |
| `name` | The node's hostname |
| `address` | Where its API listens: the source address it reaches your endpoint from, with port 9505. Behind a NAT, your orchestrator knows better |
| `ca_cert_pem` | The node's CA certificate. Before approving a node by hand, check its SHA-256 against the node's console - the line [above](#its-ca-from-its-console), and the one the node prints when it announces itself: `check it shows this node's CA as SHA-256 ...` |
| `registration_token` | Absent without one |
| `poll_secret` | What the node proves itself with when it asks again. Keep only a hash of it |

Answers:

| Status | Body | The node |
|---|---|---|
| `201` | `{"id": "...", "admitted": true, "trust": {...}}` | applies the trust, and is done |
| `201` | `{"id": "...", "admitted": false}` | waits, asking every 15 seconds |
| `409`, or a `400` whose message names `service_cert_pem` | - | falls back to protocol 1: it mints an `os:admin` credential and posts it, **private key included**. Never answer either |
| any other | an error message | logs it, and announces again 5 seconds later, then 10, 20... every 2 minutes at most |

**`GET /register/{id}`**, with `Authorization: Bearer <poll_secret>` -
the node asking whether it's admitted:

| Status | Body | The node |
|---|---|---|
| `200` | `{"root_cert": "...", "bundle": "..."}` | applies the trust, and is done |
| `202` | - | asks again 15 seconds later |
| `404` | - | its announcement is unknown - refused, or forgotten: it announces again an hour later. Also the answer to a wrong secret |

**The trust**: `root_cert`, your fleet's root (PEM); `bundle`, the
signed bundle file's bytes in base64 ([the bundle's
format](orchestrator-certificates.md#the-bundles-format)). The node
checks the bundle against the root, and refuses a root other than its
`controller_fleet_root_cert`.

A node that applied the trust never announces itself again - not after
a reboot, not after an update: it keeps a mark on its own storage, and
drops its token. A reset (`SystemService/Reset` wiping its state) makes
it a new node, with a new CA.

### An endpoint

Any HTTPS server that keeps the state does: an endpoint in your
orchestrator, a function, this example in Python's standard library.
Its certificate, for the fleet-root check:

```sh title="examples/orchestrator/registration-cert.sh"
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
```

The endpoint: tokens in a file, nodes as JSON files, an approval as a
file:

```python title="examples/orchestrator/registration-server.py"
#!/usr/bin/env python3
"""The registration endpoint nodes announce themselves to
(docs/private-cloud/first-contact.md), protocol 2: a node sends its CA,
name and address - never a key -, and gets the fleet's trust once it's
admitted. Python's standard library only: any language that serves
HTTPS does the same.

    registration-server.py --listen 0.0.0.0:8443 --fleet FLEET-DIR \
        --cert registration.crt --key registration.key --state DIR

- A token listed in DIR/tokens (one per line), given to a machine the
  orchestrator created, admits its node at once - once.
- Any other announcement waits: DIR/nodes/<id>.json says what it is;
  creating DIR/approved/<id> admits it, and the node gets the trust at
  its next poll (every 15 seconds).
- DIR/nodes/<id>.json is the inventory: the node's name, its address,
  its CA - what the orchestrator checks it with.

A node falls back to protocol 1 - announcing with a key - only when told
409: this server never asks for one.
"""
import argparse
import base64
import hashlib
import hmac
import json
import os
import re
import secrets
import ssl
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def write_json(path, value):
    """Replaces path with value as JSON, whole or not at all."""
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path))
    with os.fdopen(fd, "w") as f:
        json.dump(value, f, indent=2)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def is_ca(pem):
    """The certificate is a CA's: openssl prints its basic constraints."""
    try:
        out = subprocess.run(["openssl", "x509", "-noout", "-ext", "basicConstraints"],
                             input=pem.encode(), capture_output=True, check=True).stdout
    except subprocess.CalledProcessError:
        return False
    return b"CA:TRUE" in out


class Registration(BaseHTTPRequestHandler):
    def answer(self, status, body=None):
        data = json.dumps(body).encode() if body is not None else b""
        self.send_response(status)
        if data:
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def trust(self):
        """The fleet's trust: its root, and the bundle - the signed file's
        bytes, base64."""
        fleet = self.server.fleet
        with open(os.path.join(fleet, "root.crt")) as f:
            root = f.read()
        with open(os.path.join(fleet, "bundle.json"), "rb") as f:
            bundle = f.read()
        return {"root_cert": root, "bundle": base64.b64encode(bundle).decode()}

    def take_token(self, token):
        """Consumes token from DIR/tokens: true when it was there."""
        path = os.path.join(self.server.state, "tokens")
        with self.server.lock:
            try:
                with open(path) as f:
                    tokens = f.read().split()
            except FileNotFoundError:
                return False
            if not token or token not in tokens:
                return False
            tokens.remove(token)
            with open(path + ".tmp", "w") as f:
                f.write("".join(t + "\n" for t in tokens))
            os.replace(path + ".tmp", path)
            return True

    def do_POST(self):
        if self.path != "/register":
            return self.answer(404)
        try:
            length = int(self.headers.get("Content-Length", "0"))
            req = json.loads(self.rfile.read(min(length, 1 << 20)))
        except (ValueError, json.JSONDecodeError):
            return self.answer(400, {"error": "not JSON"})
        if req.get("protocol", 1) < 2:
            # A node sends protocol 2 first; protocol 1 carries a key.
            return self.answer(400, {"error": "protocol 2 only"})
        name, address, ca = req.get("name", ""), req.get("address", ""), req.get("ca_cert_pem", "")
        secret = req.get("poll_secret", "")
        if not name or not address or len(secret) < 32 or not is_ca(ca):
            return self.answer(400, {"error": "name, address, a CA certificate and a poll secret are required"})
        node_id = secrets.token_hex(16)
        admitted = self.take_token(req.get("registration_token", ""))
        write_json(os.path.join(self.server.state, "nodes", node_id + ".json"), {
            "id": node_id, "name": name, "address": address, "ca_cert_pem": ca,
            "admitted": admitted,
            # Only the hash: whoever reads the state can't poll as the node.
            "poll_secret_sha256": hashlib.sha256(secret.encode()).hexdigest(),
        })
        if admitted:
            open(os.path.join(self.server.state, "approved", node_id), "w").close()
            return self.answer(201, {"id": node_id, "admitted": True, "trust": self.trust()})
        return self.answer(201, {"id": node_id, "admitted": False})

    def do_GET(self):
        m = re.fullmatch(r"/register/([0-9a-f]{32})", self.path)
        if not m:
            return self.answer(404)
        node_id = m.group(1)
        try:
            with open(os.path.join(self.server.state, "nodes", node_id + ".json")) as f:
                node = json.load(f)
        except FileNotFoundError:
            return self.answer(404)  # unknown, or forgotten: the node announces again
        auth = self.headers.get("Authorization", "")
        given = hashlib.sha256(auth.removeprefix("Bearer ").encode()).hexdigest()
        if not auth.startswith("Bearer ") or not hmac.compare_digest(given, node["poll_secret_sha256"]):
            return self.answer(404)
        if not os.path.exists(os.path.join(self.server.state, "approved", node_id)):
            return self.answer(202)
        return self.answer(200, self.trust())

    def log_message(self, fmt, *args):
        print("registration: " + fmt % args, flush=True)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--listen", default="0.0.0.0:8443")
    p.add_argument("--fleet", required=True, help="the fleet's root.crt and bundle.json")
    p.add_argument("--cert", required=True, help="this endpoint's certificate and its issuing CA")
    p.add_argument("--key", required=True)
    p.add_argument("--state", required=True)
    a = p.parse_args()
    for d in ("nodes", "approved"):
        os.makedirs(os.path.join(a.state, d), exist_ok=True)

    host, port = a.listen.rsplit(":", 1)
    server = ThreadingHTTPServer((host, int(port)), Registration)
    server.fleet, server.state = a.fleet, a.state
    server.lock = threading.Lock()
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.load_cert_chain(a.cert, a.key)
    server.socket = ctx.wrap_socket(server.socket, server_side=True)
    print(f"registration: listening on {a.listen}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
```

## Other ways in

- **The installer**: `LifecycleService/Install` writes a blank disk
  with the same registration settings or the fleet
  ([provisioning a node](../provisioning-a-node.md)).
- **A seeded image**: the registration settings or the fleet written
  onto an image's state partition before its first boot (`janusctl image
  seed-controller`, `seed-fleet`).
- **A node already running**, provisioned with neither: its first-boot
  admin credential, printed once on its console, calls `TrustSet` to
  give it your fleet ([a fleet without a Controller](../fleet-without-controller.md)).
