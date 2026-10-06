# Driving nodes

Calling a Janus node's gRPC API from any language: the .proto files,
mutual TLS, the errors, and the safe way to change a node's HAProxy
configuration, its network and its release.

## Calling a node

| | |
|---|---|
| Endpoint | the node's address, port **9505**: gRPC over HTTP/2, TLS 1.3 only |
| The node's certificate | verified with the node's CA, pinned at [first contact](first-contact.md) - for one of its own names: its hostname, `localhost`, `127.0.0.1`, its addresses |
| Your certificate | a client certificate of the fleet and its issuing CA, presented together ([certificates](orchestrator-certificates.md#client-certificates)) |
| The contract | the `.proto` files in `api/proto/janus/v1alpha1`, at the release your nodes run; every call and its role in the [API reference](api-reference.md) |

**Your language's code** comes from the `.proto` files: `buf generate`
or `protoc` with your language's gRPC plugin (Java, C#, Python, Rust,
TypeScript, Go... - [gRPC's languages](https://grpc.io/docs/languages/)).
Beyond each other, the files import only `google/protobuf/empty.proto`.

**From a shell**, `grpcurl` - with the `.proto` files, since nodes don't
offer gRPC reflection:

```sh title="examples/orchestrator/janus.sh"
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
```

```sh
JANUS_PROTO=~/janus/api/proto JANUS_NODE_CA=edge-1-ca.pem \
JANUS_CERT=orchestrator.crt JANUS_KEY=orchestrator.key \
  ./janus.sh 192.0.2.21:9505 SystemService/Version
```

In JSON, `bytes` fields are base64 and names are in lowerCamelCase
(`local_ca_cert` is `localCaCert`); 64-bit integers are strings.

### Errors

| gRPC status | Means |
|---|---|
| `PermissionDenied` (7) | the certificate's role - or the user's, through a `janus:controller` certificate - doesn't allow the call; the message says what it needed |
| `InvalidArgument` (3) | the request itself is wrong: a configuration the node refuses, a missing field |
| `FailedPrecondition` (9) | the node can't do it in its state: an extension its image doesn't have, a service its settings disable, nothing on trial to confirm |
| `NotFound` (5) | what the request names doesn't exist: a service, a map, a file |
| `Unimplemented` (12) | `ApplyConfiguration`, `MetaWrite`, `MetaDelete` - never implemented |
| `Unavailable` (14) | the node doesn't answer: down, rebooting - or a TLS handshake it refused (a certificate it doesn't trust, an expired one) |

A configuration the node refuses on a streamed call - HAProxy's,
typically - comes as the stream's last message (`"stage": "rejected"`
with HAProxy's errors), not as a status.

### Connections

- **One connection per node, kept open**: gRPC multiplexes calls over
  it. A node that reboots drops it; reconnect with a backoff - the node
  answers again once it's up, typically within a minute.
- **Short-lived client certificates**: when yours is renewed, the next
  connection takes it - most TLS libraries let a callback give the
  certificate per connection (`GetClientCertificate` in Go,
  `KeyManager` in Java).
- **Many nodes**: calls to different nodes are independent - nothing
  orders them but your orchestrator.

## Changing a node safely

A node checks every change before it makes it, and undoes the ones that
could cut it off unless someone confirms them. What your orchestrator
has to do is follow each call's protocol to its end.

### HAProxy's configuration

`HAProxyService/ApplyConfig` sends the whole `haproxy.cfg`; the node has
HAProxy check it (`haproxy -c`, then a real start), and a new HAProxy
process takes over without dropping a connection. The stream's stages:
`validating`, then `reloading` and `done` with `accepted: true` - or
`rejected` with HAProxy's errors, the running configuration untouched.
`ValidateConfig` checks one without applying it.

```sh title="examples/orchestrator/haproxy-apply.sh"
#!/usr/bin/env bash
# A node's HAProxy configuration replaced (docs/private-cloud/
# driving-nodes.md): the node has HAProxy check it, then a new HAProxy
# process takes over without dropping a connection. A configuration it
# refuses changes nothing.
#
#   haproxy-apply.sh ADDRESS HAPROXY-CFG
#
# Same environment as janus.sh; a role of os:operator or more.
set -euo pipefail
address=$1 cfg=$2
here=$(dirname "$0")
request=$(jq -n --arg config "$(base64 < "$cfg" | tr -d '\n')" '{config: $config}')
stages=$("$here/janus.sh" "$address" HAProxyService/ApplyConfig "$request" | jq -c .)
echo "$stages"
# Every stage streams; the last says whether it was accepted.
if [ "$(tail -1 <<<"$stages" | jq -r '.accepted // false')" != true ]; then
  echo "haproxy-apply: $address refused the configuration" >&2
  exit 1
fi
```

The configuration stays on the node across reboots and updates. What
differs from a distribution's `haproxy.cfg`: [your haproxy.cfg](../haproxy-config.md);
the files it references (certificates, error pages, maps): `FilePut`
([HAProxy's files](../haproxy-files.md)).

### The network

`NetworkService/NetworkConfigApply` switches the node to a new network
configuration **on trial**: unless `NetworkConfigConfirm` arrives within
`confirm_timeout_seconds` (30 by default, at most 300), the node goes
back to the previous one by itself - so a configuration that cuts your
orchestrator off undoes itself.

The confirmation counts only over a **new connection**, opened after the
apply, to an address the new configuration keeps: proof the node is
still reachable. Most gRPC libraries reuse one connection per target -
open a separate channel for the confirmation, or reconnect.

```sh title="examples/orchestrator/network-apply.sh"
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
```

The firewall works the same way (`FirewallApplyRuleset`,
`FirewallConfirm` - [firewall](../firewall.md)). The configuration's
format: [network configuration](../network-configuration.md).

### Updates

`LifecycleService/Upgrade` gives the node a release: the node fetches
its files from a URL, checks the release's signature, writes them to its
other slot, and reboots into it. With `wait_for_health`, the new slot
has `health_timeout_seconds` to show a healthy HAProxy, or the node
reboots back into the slot it left - by itself, whoever's watching.

```sh title="examples/orchestrator/upgrade.sh"
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
```

- **The URL** is a directory holding the release's `rootfs.squashfs`,
  `rootfs.verity`, `uki-a.efi` and `uki-b.efi`: a GitHub release's
  download URL (`https://github.com/swenske/Janus/releases/download/<version>/`),
  a mirror of it, or - for a node with extensions - the URL the image
  factory gives for its schematic
  ([updates](../image-factory.md#updates-keep-the-schematic)). Its
  `rootfs.squashfs.sha256` is the `sha256` to pass.
- **A node that can't reach out**: `UploadReleaseFile` streams each file
  to the node, then `Upgrade` takes the staging directory it returns.
- **The stream ends at `rebooting`**, with the connection. The update is
  done when the node answers again from its new slot (`SystemService/
  Version`'s `active_slot`), and its event stream (`SystemService/Events`)
  has `bootcommit.confirmed`. A node that comes back on its old slot
  reverted: its HAProxy wasn't healthy in time.
- **Which release**: the [GitHub releases](https://github.com/swenske/Janus/releases);
  each has a `security.json` listing the vulnerabilities it fixes.
  `https://janus.sw-servers.net/api/v1/updates/<schematic-id>?arch=amd64`
  answers the newest release built for a schematic.

Update nodes one at a time - or a group at a time, never all the nodes
serving a virtual IP at once ([lifecycle](lifecycle.md)).

## Watching nodes

- **Metrics**: each node serves Prometheus metrics on port 10056 (plain
  HTTP, no credentials - keep it on a management network): its
  certificates' expiry, its boot slot, HAProxy as janusd runs it, time
  synchronization, SELinux ([observability](observability.md)).
- **Events**: `SystemService/Events` streams what happens on the node -
  configurations applied or rejected, trials, services, updates - the
  ones it kept since it booted, then live (`since_id` resumes after an
  event).
- **Logs**: `SystemService/Logs` streams a service's output - janusd's,
  HAProxy's.

There's no shell and no "run a command" call, by design: what a node
does is what its API offers.
