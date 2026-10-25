# Replacing a node

A node can hand its state to a new node - its HAProxy configuration,
certificates and keys, kernel parameters, network configuration and, if
you want, its identity - so the new one serves as the old one did. What
it's for: a node installed before a feature it lacks (the LOGS
partition, say), a disk to grow, a VM to move to other hardware.

An update can't do this: it replaces the system, never the disk's
layout. The state goes over as files, not as a disk image, so the two
nodes' disks can differ.

## What goes over, and what doesn't

| Goes over | Stays behind |
|---|---|
| `haproxy.cfg` and its history | The event log and the audit |
| HAProxy's files (maps, error pages, certificates) | The logs - the new node starts its own |
| The certificates uploaded at runtime | A pending boot confirmation |
| The letsencrypt extension's certificates, account and settings | A one-time registration token |
| The TLS ticket key ring | The kernel parameters' observations (the old hardware's) |
| The modules' configurations: VRRP, BGP, firewall, Consul, exporters, log forwarding, console budget | A cluster membership: a member is replaced by another member ([cluster.md](../cluster.md)) |
| The kernel parameters you set | |
| The network configuration (held back by default, below) | |
| The Controller registration and the fleet trust | |
| With `-identity`: the node's CA and keys, its Consul node ID | |

The export holds private keys. It never goes through the Controller - the
node refuses a call the Controller relays: call it yourself, with the
node's own credentials or a fleet certificate.

## Identity: the same node, or a new one

With `-identity` on both ends, the new node takes the old one's CA and
keys. It is then the old node to everything that knows it: the
Controller (which pins the CA), your janusctl contexts, the credentials
the old node issued, your monitoring's certificate checks. The new
node's own first-boot credentials stop working once it restarts into
the old state - use the old node's from then on.

Without `-identity`, the new node keeps its own CA: it is a new node,
with the old one's configuration. The Controller sees a new node to
admit.

With the identity, **the old node must be off before the new one takes
its place**: two nodes with one CA and one address answer for each
other.

## Doing it

The new node must be unused - a fresh install, or one reset with
`janusctl system reset -state`. Then, from a machine with both nodes'
credentials:

```sh
janusctl -n old system state export -identity - \
  | janusctl -n new system state import -identity -save-network old-net.json -
```

The pipe keeps the state off your disk. To go through a file instead -
written `0600`, never over an existing one:

```sh
janusctl -n old system state export -identity old-state.tar
janusctl -n new system state import -identity -save-network old-net.json old-state.tar
rm old-state.tar
```

The new node checks every file against the export's manifest, stages
the state and restarts into it: janusd puts it in place as it starts,
before anything reads it. Its console then says `state: <old node>'s
state put in place`.

### Node to node

Or the new node pulls the state from the old one itself, and it never
passes through your machine at all:

```sh
janusctl -n new system state import -from old -identity -save-network old-net.json
```

janusctl asks the new node for a key made for the purpose - kept in
its memory, used once - and has the old node sign its request: a
certificate of ten minutes, from the old node's own CA, that opens
**one** export of its state - with its identity only when you ask for
`-identity` - and nothing else. The new node then reaches the old one
with it, checks it by its CA, and takes the state as above. Only a
signing request and a certificate go through janusctl; the old node's
console logs the grant (`state: <new node> may pull this node's state
once`), and a janusd restart there ends a grant not used yet.

`-from` names a node of your context. With the nodes' own credentials
instead, give the old node's address and its credentials:
`-from 192.0.2.10:9505 -from-ca old-ca.crt -from-cert old-admin.crt
-from-key old-admin.key` (they default to `-ca`, `-cert`, `-key`). The
new node reaches the old one where janusctl does, unless
`-from-address` says otherwise - when janusctl goes through a tunnel or
a NAT the nodes don't. The new node must reach the old one's API, TCP
9505.

The Controller does the same when it makes a [cluster from a
node](clusters.md#from-a-node) - without the identity, always: through
the Controller, a node grants an export of its state but never of its
CA and keys, and `StateExport` and `StateImport` themselves stay
refused. It gives the Controller nothing a cluster didn't already: a
cluster's member certificates come from a CA the Controller holds, and
a member pulls another's secrets with one.

## Taking the address over

By default the network configuration is held back - `-save-network`
keeps it - so the new node doesn't take the old one's address while the
old one still serves. The new node's HAProxy can't bind an address it
doesn't have yet: a configuration that binds the old node's address
(`bind 192.0.2.10:443`) waits until the takeover, and HAProxy retries by
itself. To have it start at once, set `net.ipv4.ip_nonlocal_bind` to 1
on the old node before exporting ([kernel tuning](kernel-tuning.md)).

When you're ready - the gap lasts from the old node stopping to the new
one binding, a few seconds:

```sh
janusctl -n old system shutdown
janusctl -n new network apply old-net.json
janusctl -n new haproxy reload
```

The network configuration matches interfaces by MAC address when it
names one: give the new VM the old one's MAC addresses, or edit the
saved file to name the interfaces (`eth0`, `eth1`) instead. A
configuration that names the interfaces alone applies anywhere.

With `-network` on the import instead, the new node takes the old one's
addresses as it restarts - for a new node on another network segment,
or once the old one is already off.

## Stick tables

They start empty on the new node: the old and the new HAProxy never run
side by side. A replacement that must keep them goes through a cluster
of two, whose new member learns the tables before the old one leaves
([cluster.md](../cluster.md)).

## From the API

`SystemService.StateExport` streams the state - a tar, its
`MANIFEST.json` first - and `SystemService.StateImport` takes it, its
first message carrying the options ([the API catalog](../api-routes.md#systemservice)).
Node to node: `CertificateRequest` (purpose `STATE_TRANSFER`) on the new
node, `StateExportGrant` with its request on the old one, then
`StateImportFrom` on the new one with the old one's address, CA and the
granted certificate. All are for administrators, and refused when the
Controller relays them.
