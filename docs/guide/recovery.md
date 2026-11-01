# When the Controller is down or lost

What keeps working without the Controller, reaching its nodes meanwhile,
and how to get it back - a backup restored on the same host or another
one, the fleet's recovery kit when there's no backup.

## What keeps working

The Controller isn't in the traffic's path: every node keeps serving
with HAProxy, its network, its firewall, VRRP and BGP as they are, and
keeps its own logs. What waits for the Controller:

| What | Until the Controller is back |
|---|---|
| Its page, the node pages, its API (Terraform) | unreachable |
| janusctl through a Controller context | works while the context's certificate lasts - 12 hours from a sign-in, an hour from a token -, then with the recovery kit ([below](#the-controller-is-down)) |
| New nodes registering | they keep trying, and register once it's back |
| Let's Encrypt through the Controller (relay, ordering) | not renewed meanwhile - renewals start a third of a lifetime early: weeks of margin ([Let's Encrypt through the Controller](letsencrypt-controller.md)) |
| Clusters | their members keep serving and replicating; no change is applied, their member certificates (30 days) are renewed once it's back - renewals start 10 days early |
| Auto-scaling, backups, log forwarding and Consul profiles | stand still; a backup missed shows as late once it's back |

## The Controller is down

Its nodes are reached meanwhile with the fleet's recovery kit - the
file and the passphrase stored when the fleet was made:

```sh
janusctl login -kit janus-recovery-kit-....age     # the passphrase asked, or JANUS_KIT_PASSPHRASE
janusctl -all version
janusctl -n edge-1 haproxy apply-config haproxy.cfg
```

It signs the current context (`-context NAME` another) a certificate for
an hour with the fleet's root - for the context's own key, named
`kit:<account>` in each node's log, `os:admin` unless `-role` says
otherwise. The Controller isn't asked: the nodes are the ones it listed
at the context's last sign-in - their addresses and CAs as they were
then: a node whose CA was rotated, or that moved, since is reached with
`-endpoint` and its CA - and each node takes a certificate its fleet's
root signs. Nothing changes on them - their bundle stays the
Controller's -, so there's nothing to undo: once the Controller is back,
`janusctl login` signs in as before. After an hour, `janusctl login
-kit` again.

A node in a cluster the Controller manages refuses changes to the
cluster's configuration from anyone but the Controller: `janusctl -n
NODE cluster release haproxy` (or the domain concerned) hands that node
back to you first, and the cluster reports it drifted once the
Controller is back ([clusters](clusters.md)).

## Restore it

Restoring a backup brings the same Controller back - its accounts,
second factors, fleet, nodes and machines: the nodes keep trusting it,
nothing is done to them ([backups](../../dashboard/README.md#backups)).
On the same host, nothing else changes.

On **another host**, it follows the new host by itself:

- its certificate - the one nodes register against and reach the
  Let's Encrypt relay with - is renewed at its first start for the new
  host's addresses, still naming the old ones, signed by the same CA:
  what trusts the Controller keeps trusting it;
- nodes reaching Let's Encrypt through its relay at a URL it gave them
  are given the new one within a minute (one an admin typed on a node
  for itself is left alone).

What names the old host's address can't follow by itself - its first
start says so in its log (`restore:` lines):

- **nodes provisioned but not registered yet**, and the **hypervisors'**
  Controller address, name the address they were given: keep that
  address for the Controller - a DNS name, or the old IP moved to the
  new host - or provision them again;
- the **relay's address**, if one is set on its tab;
- **janusctl contexts** name the Controller's `host:port`: `janusctl
  login` again if it changed;
- **passkeys** work on the page's same host name only; everyone signs
  in again - sessions don't survive a restore.

A name for the Controller (a DNS record, `-advertise-address`) rather
than an IP makes all of that moot: give it from the start.

## No backup: the recovery kit

The fleet's recovery kit and its passphrase bring the nodes under
janusctl without a Controller: `janusctl fleet recover -kit KIT`, then
`janusctl fleet adopt NODE -endpoint ADDRESS -ca-fingerprint FP -kit
KIT` for each node - its fingerprint is on its console
([a fleet without a Controller](../fleet-without-controller.md#lost-machines-lost-controller)).
The bundle it makes lists only janusctl's issuing CA: the lost
Controller's certificates stop working on each node it reaches.

## Neither

Without a backup nor the kit, a node is reached again with a
certificate of its own CA (`janusctl access trust-reset` forgets the
fleet, then the node joins a new one).
