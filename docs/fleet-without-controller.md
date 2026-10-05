# A fleet without a Controller

janusctl can keep a fleet itself: no Controller, no server to run -
nodes trusted by a fleet whose root's key stays offline, in a recovery
kit, and machines (a laptop, a CI) that sign themselves short
certificates and reach the nodes directly. It's the same fleet as a
Controller's ([dashboard/README.md](../dashboard/README.md#securing-the-fleet)):
the same kit, the same bundle the nodes apply
(`internal/pki/fleet.go`, [api-routes.md](api-routes.md) AccessService).

```
recovery kit (offline)          root ──signs──> issuing CA "alice-laptop" ──signs──> alice's 12 h certificates
  root's key + passphrase         │             issuing CA "ci"           ──signs──> the CI's 12 h certificates
                                  └──signs──> bundle (version, the issuing CAs) ──> every node
```

- The **root** signs only issuing CAs and bundles. Its key is in the
  kit: an armored age file, encrypted with a passphrase shown once -
  store both together, apart from the machines (a password manager's
  note).
- Each **machine** has its own **issuing CA**: its key is a file
  (`issuing.key`, 0600) in janusctl's configuration. janusctl signs
  itself a 12-hour certificate with it whenever the last one ends - no
  network, no prompt.
- The **bundle** lists the issuing CAs the nodes accept. Taking a
  machine out is a bundle without its issuing CA: once a node has it,
  the machine's certificates stop working there.
- Each node keeps **its own CA** - the first boot's admin credential,
  the way in when nothing else works. janusctl pins it to verify the
  node (its fingerprint is on the node's console).

## Make the fleet

```sh
janusctl -context lab fleet init -name lab -issuer alice-laptop janus-kit.age
```

It prints the kit's passphrase once and asks it back. The context
`lab` (`janusctl context list`) is this machine's: the fleet's root,
this machine's issuing CA and key, the bundle, the nodes.

## Nodes in the fleet from their first boot

```sh
janusctl fleet export provision/     # root.crt, bundle.json, user-data.json
```

Give a new node the fleet with any of:

- `janusctl image seed-fleet DISK` - a built raw image (the current
  fleet context's, or `-fleet-root provision/root.crt -fleet-bundle
  provision/bundle.json`);
- `janusctl lifecycle install -fleet-root ... -fleet-bundle ... DISK
  BUNDLE_DIR`;
- a NoCloud volume (`cidata`) whose `user-data` is `user-data.json` -
  `fleet_root_cert` and `fleet_bundle`, next to a `network` if you have
  one ([provisioning-a-node.md](provisioning-a-node.md#method-3-nocloud-volume-cidata)).

The node trusts the fleet from its first boot. Its console shows its CA's
fingerprint:

```
ca sha256 9bc33016 3d392669 a236a5b6 e747f07c
          f0bcfafd fa39f46d 22c5a3b4 a38098cd
```

Adopt it on that fingerprint - nothing to copy from the node:

```sh
janusctl fleet adopt edge-1 -endpoint 192.0.2.10 -ca-fingerprint 9bc330163d39...
janusctl -n edge-1 haproxy show-info
janusctl -all version
```

janusctl checks the node's CA against the fingerprint, and the node's
server certificate against its CA, before it sends anything else; a
wrong fingerprint is refused. Space, colons and case don't matter.

A node that runs already, without the fleet: adopt it with its first
boot's admin credential, once - `janusctl fleet adopt edge-2 -endpoint
192.0.2.11 -ca ca.crt -cert admin.crt -key admin.key`.

Each node logs what the fleet's certificates do as theirs: `api:
SystemService/ServiceRestart: alice (os:admin, fleet)`.

## Another machine

On the new machine (a CI, a colleague's laptop):

```sh
janusctl -context lab fleet issuer request -issuer ci -user ci -role os:operator ci.json
```

Where the kit is:

```sh
janusctl fleet issuer sign -kit janus-kit.age ci.json ci-grant.json
janusctl fleet sync                  # the nodes take the new bundle
```

Back on the new machine:

```sh
janusctl fleet issuer accept ci-grant.json   # the fleet, its nodes
janusctl -all haproxy show-info
```

The request carries no secret (a CSR); the grant carries none either
(the issuing CA's certificate, the bundle, the nodes). The role is the
machine's choice for its certificates (`-role`); a CI that only runs
HAProxy takes `os:operator`.

## Take a machine out

```sh
janusctl fleet issuer list
janusctl fleet issuer revoke -kit janus-kit.age ci
janusctl fleet sync
```

A node lets the machine in until it has the new bundle: `fleet sync`
reaches every node of the context, `fleet status` shows each one's
bundle. Any machine of the fleet can sync: it takes the newest bundle it
finds - here or on a node - and gives it to the nodes that haven't it.

## Lost machines, lost Controller

With the kit and its passphrase, a machine makes the fleet again:

```sh
janusctl -context lab fleet recover -kit janus-kit.age -issuer carol-desk
janusctl fleet adopt edge-1 -endpoint 192.0.2.10 -ca-fingerprint 9bc3... -kit janus-kit.age
```

The recovered bundle lists only this machine's issuing CA: every node it
reaches drops the others - a lost laptop's, a lost Controller's. With
`-kit`, janusctl reaches a node with a certificate the root signs for an
hour (a node accepts the root's), and its bundle wins, signed above the
node's.

A Controller's recovery kit works the same: `fleet recover -kit
janus-recovery-kit-....age` brings its nodes under janusctl when the
Controller is gone and no backup restores it
([dashboard/README.md](../dashboard/README.md#backups)).

## What each loss means

| Lost | Consequence | What to do |
|---|---|---|
| a machine (laptop, CI) | its issuing CA's key, its 12 h certificates | `fleet issuer revoke` + `fleet sync` from another machine |
| every machine | nothing on the nodes | `fleet recover -kit`, then `adopt -kit` each node |
| the kit, not its passphrase | an encrypted file | nothing - keep them apart |
| the kit and its passphrase | the fleet: whoever has both can sign a bundle | `janusctl access trust-reset` on each node with its own CA's credential, then a new fleet |
| a node's own admin credential | the way into that node | `janusctl access rotate-ca` (or the node page's Access) |

## Files

`~/.config/janus/janusctl.json` (`JANUSCONFIG` elsewhere) and, per fleet
context, a directory next to it: `root.crt`, `issuing.crt`,
`issuing.key` (0600), `bundle.json`, `key.pem` and `cert.pem` (the 12 h
certificate). The kit's passphrase is asked on the terminal, or read
from `JANUS_KIT_PASSPHRASE` (scripts).

Bundle versions are milliseconds since 1970: newer than any other
machine's, without knowing it. A node refuses an older bundle, and a
different one of the same version (`fleet sync -kit` from the machine
whose bundle should win).
