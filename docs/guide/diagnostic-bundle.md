# Diagnostic bundle

Everything a node's API says about it, in one file to attach to an issue
or hand to whoever helps: its version and image, its events, its
services' and kernel's logs of this boot and the one before, HAProxy's
state and configuration, the network, the kernel parameters, processes,
mounts - read with your own permissions, without the node's secrets.

## Making one

On the Controller, **Tools › Diagnostic bundle** on the node's page:
the page lists what the bundle holds for your role, **Download** saves
`janus-support-<node>-<time>.tar.gz`. The Controller's audit records
each download.

With janusctl:

```sh
janusctl -n lb1 system support
janusctl -n lb1 system support -o lb1.tar.gz
janusctl -all system support
```

The file is `janus-support-<hostname>-<time>.tar.gz` unless `-o` names
it (`-` for stdout), readable by you only (it holds logs). `-all` writes
one per node.

## What it holds

| Files | Read with | Who may |
|---|---|---|
| `version.json`, `system/` - image, services, processes, mounts, memory, CPU, disks, kernel parameters and their history, log forwarding (secrets left empty by the node) | `Version`, `ServiceList`, `Processes`, `Mounts`, `SysctlList`... | readers |
| `network/` - state and configuration, the neighbour table, sockets, the firewall's ruleset, VRRP, BGP and Consul states | `NetworkStatus`, `NetworkConfigGet`, `NeighborList`, `FirewallGetRuleset`... | readers |
| `haproxy/` - `show info`, `show stat`, backends, certificates (names and expiry), files, maps, Let's Encrypt; `haproxy.cfg` masked | `ShowInfo`, `Stats`, `GetConfig`... | readers |
| `events.jsonl` | `Events` | readers |
| `logs/<service>.log`, `logs/<service>.boot-1.log` | `Logs` | operators |
| `kernel/dmesg.log`, `kernel/dmesg.boot-1.log`, `kernel/avc-denials.log` | `Dmesg` | admins |
| `audit.jsonl` | `Audit` | admins |

The boot before is there when the node keeps it (a LOGS partition).
`MANIFEST.json` lists every file, and each part left out with why: not
for your role, not in the node's version, a module the image doesn't
have. What your role may not read is never asked for - the node's audit
gets no refusal from it. Each file keeps its last 8 MiB.

## What it never holds

- The node's credentials, and any PEM private key - cut out of every
  file.
- The configurations whose passwords the node doesn't mask: BGP's,
  VRRP's, Consul's - their states are in it.
- `haproxy.cfg`'s secrets: `userlist` and `stats auth` passwords,
  credential headers' values, `setenv` values - masked whoever makes the
  bundle (see [haproxy-config.md](../haproxy-config.md#who-reads-it)).

Logs are as the node wrote them: a URL with a token in its query string,
a client's address, are in them. Read them before sharing a bundle
outside your organization.
