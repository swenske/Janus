---
name: janusctl
description: Drive Janus nodes - immutable HAProxy appliances with no shell or SSH, reached only through their mTLS API - with the janusctl CLI. Read a node's state, logs, HAProxy (backends, config, maps, stick tables, sessions, certificates), network, firewall, VRRP and BGP; change them the safe way (configurations validated on the node, trials that revert by themselves, one node at a time); update nodes and run clusters. Use when the user mentions Janus, janusctl, a Janus node, fleet, cluster or Controller, or asks about HAProxy on machines janusctl reaches.
---

# Driving Janus nodes with janusctl

A Janus node has no shell, no SSH and no package manager: its gRPC API,
over mutual TLS, is the only way in, and `janusctl` is its client. Each
node checks the certificate's role on every call - `os:reader` reads,
`os:operator` runs HAProxy, services and reboots, `os:admin` does
everything - and logs who did what.

Every command, its arguments and what it does to the node:
[references/commands.md](references/commands.md). The installed
janusctl's own help wins when they differ: `janusctl help COMMAND`.

## Before the first command

1. `janusctl version` - janusctl's version. `janusctl context list` -
   the Controllers and fleets signed in to, the current one marked.
   `janusctl nodes` - the context's nodes: their names are what `-n`
   takes.
2. No context, or a certificate expired that janusctl can't renew: the
   user signs in, not you - it is interactive (a browser page, a device
   code, an SSH key's passphrase). Ask them to run `janusctl login` in
   their terminal, or as `! janusctl login -controller HOST` here for a
   browser or device sign-in. Never ask for, read or handle passwords,
   tokens, recovery kits or private keys.
3. A flag you haven't seen in `janusctl help` for that command: check it
   there first - versions differ.

## Running commands

- Global flags come **before** the command:
  `janusctl -n edge-1 haproxy backends`, `janusctl -context lab -n edge-1,edge-2 version`,
  `janusctl -all version`.
- Always name the nodes with `-n` (or `-all`). Off a terminal janusctl
  never opens its pickers: a missing node or argument is an error that
  names what it wanted - read it, add the argument.
- Several nodes: janusctl runs on each at once, every line prefixed with
  the node's name. Fine for reading; changes go one node at a time
  unless the user asked for all of them.
- The output is plain text off a terminal; the `get` commands of
  configurations (`network get`, `system log-forwarding get`,
  `haproxy acme get`) print JSON, which their `apply`/`set` take back.
- Commands that never end on their own: `system events` and
  `system audit` stream live, `system logs -f` and `system dmesg -f`
  follow. Run them under `timeout` - `timeout 10 janusctl -n edge-1 system events`
  (exit status 124 is the timeout, not a failure) - or take the last
  lines: `janusctl -n edge-1 system logs haproxy -n 200`.
- `janusctl tui` is an interactive dashboard: never run it bare.
  `janusctl -n edge-1 tui -once` prints one frame - a good first look at
  a node (CPU, memory, interfaces, HAProxy, services, events, its log).
- Paths given to `lifecycle` commands are paths **on the node**.
- A refusal (`PermissionDenied`) means the certificate's role doesn't
  allow the call: say so. Never work around it with another context,
  a node's first-boot admin certificate, `-as-user`/`-as-roles` or
  `-endpoint`, unless the user tells you to.
- Write each janusctl command plainly - `janusctl`, its global flags,
  the command - not inside `sh -c`, a variable or `eval`: the plugin's
  hook reads what each one does, and asks the user before a critical
  one or one it can't read. A command asked and refused is the user's
  answer: don't rewrite it to get past the question.

## What you may run

Each command in [references/commands.md](references/commands.md) is
marked with what it does to the node:

- **reads** - nothing changes. Run what helps answer the question.
- **changes** - changes the node, or files on this machine. Run only
  what the user asked for. Before, show what it will change (the diff
  of the configuration, the entry, the command); after, check it took
  (see the workflows below).
- **critical** - reboots, powers off, interrupts or moves traffic,
  wipes, or touches the node's trust and credentials. Only when the user
  asked for that very action on those very nodes in this conversation:
  restate what will happen and wait for their yes. Never as a step you
  chose yourself to fix something - propose it instead.

Never run a **changes** or **critical** command on several nodes (`-all`,
`-n a,b`) unless the user asked for exactly that.

## Diagnosing a node

```sh
janusctl -n edge-1 tui -once                   # everything at a glance
janusctl -n edge-1 system info                 # version, slot, memory, CPU, disks
janusctl -n edge-1 system services             # services and their health
janusctl -n edge-1 haproxy show-info           # HAProxy's version, uptime, connections
janusctl -n edge-1 haproxy backends            # each server's state
janusctl -n edge-1 network status              # interfaces, addresses, routes, DNS, clock
janusctl -n edge-1 system logs janusd -n 200   # the node's daemon
janusctl -n edge-1 system logs haproxy -n 200  # HAProxy's output and its logs
janusctl -n edge-1 system dmesg                # the kernel
timeout 5 janusctl -n edge-1 system events     # the event log, then stop
```

- The network as the node sees it: `janusctl -n edge-1 system probe tcp 10.0.0.5:8080`,
  `system probe http https://app.example.com/health`,
  `system probe dns app.example.com`, `system probe ping HOST`,
  `system probe traceroute HOST`; `-source IP` sends from a virtual IP.
- HAProxy at run time: `haproxy errors` (the last invalid request of
  each proxy), `haproxy sessions`, `haproxy tables` then `haproxy table NAME`.
- Who changed what: `haproxy config-history`, `system sysctl history`,
  `timeout 5 janusctl -n edge-1 system audit` (admins).
- A file for a ticket: `janusctl -n edge-1 system support -o edge-1.tar.gz`
  - no secrets in it. Tell the user where it is.
- Files on the node are read-only and never its secrets:
  `system ls /etc/haproxy`, `system cat /etc/haproxy/haproxy.cfg`.
- A packet capture: `janusctl -n edge-1 system pcap -i eth0 -f "port 443" -duration 30s -o edge-1.pcap`
  - always `-duration` and `-o`; never print it (binary, and clients'
  data): give the user the file for Wireshark.

Logs are for operators, the kernel's messages and the audit for admins:
a reader is refused them.

## Changing HAProxy's configuration

```sh
janusctl -n edge-1 haproxy get-config > haproxy.cfg
# edit, then show the user the diff
janusctl -n edge-1 haproxy apply-config haproxy.cfg
janusctl -n edge-1 haproxy backends            # check it took
```

- The node validates the file with HAProxy first and refuses a
  configuration that doesn't load - nothing changes then; read the
  error, fix, apply again. Applied, it reloads without dropping a
  connection.
- Keep the `global` section as `get-config` printed it: the node
  refuses one without its `chroot`, `uid 1000`/`gid 1000` and stats
  socket, or with `daemon`, `master-worker`, `external-check`, `setenv`...
- Undo: `haproxy config-history`, then `haproxy config-revert ID`.
- Who may only read gets the configuration with its passwords masked;
  don't apply a masked text back.
- Files the configuration names (error pages, maps, CA bundles) are
  in `/etc/haproxy/files`: `haproxy files`, `haproxy file-get NAME`,
  `haproxy file-put NAME FILE` (`-reload` to use it at once).
- `haproxy map-set`, `map-delete`, `acl-add` and `acl-delete` change
  the running HAProxy only: the next reload reads the file again. To
  keep a change, also write the file (`file-get`, edit, `file-put`).
- `haproxy reload` reloads HAProxy seamlessly with haproxy.cfg and its
  files as saved - after a `file-put` without `-reload`, say. It undoes
  what `server-state`, `map-set` and `acl-add` changed: look at
  `haproxy backends` first, and tell the user if a server they drained
  would take traffic again.
- A server out of the traffic: `janusctl -n edge-1 haproxy server-state app app1 drain`
  (new clients go to the other servers, the current ones finish;
  `maint` takes it out altogether), `ready` puts it back,
  `haproxy backends` shows it. It holds until HAProxy's next reload -
  an `apply-config` brings the server back as haproxy.cfg says: tell
  the user before applying a configuration while a server is drained;
  `disabled` on its line keeps it out for good.
- Certificates: `haproxy cert-list`; `haproxy cert-upload NAME FILE`
  takes a PEM with its key - pass the path, never print the file.

A node in a cluster refuses these changes ("managed by cluster ..."):
the cluster's configuration is changed as a whole - on the Controller's
Clusters page, or `janusctl cluster checkout NAME DIR` then
`janusctl cluster apply NAME DIR` from the fleet's context. Say so;
`cluster release` only when the user asks (the cluster then reports
the node as drifted).

## Network, firewall and kernel parameters: trials

```sh
janusctl -n edge-1 network get > network.json
# edit, show the diff
janusctl -n edge-1 network apply network.json
```

`network apply`, `network firewall apply`, `system sysctl set` and
`system sysctl reset` apply **on trial**: the node goes back to what it
had by itself unless the change is confirmed in time (30 s by default,
`-timeout`). janusctl confirms over a new connection - the proof the
node is still reachable - and says `confirmed via ...`. If it says it
couldn't confirm, the node reverts by itself: wait for the timeout, then
`network status`. Never pass `-no-confirm` unless the user asks.

- Firewall: `network firewall check FILE` first; `network firewall sets`,
  `set-add`, `set-del` change named sets live.
- Kernel parameters: `system sysctl get NAME` gives the effect on
  HAProxy, the risk and a suggested value; `system sysctl list`
  everything the node lets change.
- VRRP, BGP, Consul: `check FILE` first, then `apply FILE` - **no
  trial**: keepalived reloads at once (a priority change moves the
  virtual IPs), BIRD reconfigures (routes change), Consul restarts.
  `network vrrp status` / `network bgp status` after.

## Updating a node

```sh
janusctl -n edge-1 version
janusctl -n edge-1 lifecycle upgrade -wait-for-health https://github.com/swenske/Janus/releases/download/VERSION/
janusctl -n edge-1 version                     # the release now running
```

**critical**: the node reboots into the release written to its other
slot; `-wait-for-health` goes back by itself if HAProxy isn't healthy
after. A node with extensions takes its image's own bundle (the image
factory's URL for its schematic) and refuses another schematic's.
`lifecycle rollback` boots the previous slot. One node at a time, the
next only once the first answers again - in a cluster, once
`janusctl -n edge-1 cluster status` shows its peers connected and in
sync.

## Secrets

- The context's keys (`~/.config/janus/`), a node's first-boot admin
  credentials, recovery kits and PEM private keys: never print, copy
  or paste them. Pass files by path.
- `system state export` writes a node's private keys into a file:
  **critical**, only on request; tell the user where the file is.
- `pki generate-client-config` issues a credential: only on request.

## More

The guide: https://janus.sw-servers.net/docs/guide/ - janusctl
(https://janus.sw-servers.net/docs/guide/janusctl/), HAProxy's
configuration on Janus (https://janus.sw-servers.net/docs/guide/haproxy-config/),
troubleshooting (https://janus.sw-servers.net/docs/guide/troubleshooting/).
