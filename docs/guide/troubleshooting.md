# Troubleshooting

A node has no shell: what you'd look at on a server is read through its
API - from its page on the Controller, or with `janusctl` - and its
console says what happens before the API is up. The usual problems, and
where each one shows.

## Where to look

| What | How |
|---|---|
| The console (serial and screen) | The first boot's credentials, the network, registration retries, init's messages - the hypervisor's console, or **Console** on a machine the Controller created |
| Event log | `janusctl -n lb1 system events` - updates, network and firewall trials, reverts, HAProxy reloads |
| Services' output | `janusctl -n lb1 system logs -n 100 janusd`, `... haproxy`; `-f` follows |
| Kernel messages | `janusctl -n lb1 system dmesg` - SELinux denials, OOM kills, drivers |
| State | `janusctl -n lb1 system services`, `system info`, `network status`, `haproxy show-info` |

The Controller's node pages show the same: **Logs**, **Monitoring**,
**System**.

## A node doesn't register

The node keeps trying, a little longer between tries (up to two
minutes), and its console says why on each one: `selfregister: <the
error> - retrying in ...`. On a machine the Controller created, its card
shows that line with the likely cause:

- **No route to the Controller** - often a DHCP server that hands out no
  gateway: give the interface a static address and a gateway.
- **No DNS for the Controller's name** - give the node a DNS server, or
  provision it with the Controller's address.
- **A certificate that doesn't name the address** - the node checks the
  Controller's certificate against the address it was given: provision
  the address the Controller's **Provision new nodes** panel shows.
- **Nothing answers on 8443** - a firewall between them, or a
  Controller listening elsewhere (`-register-addr`).

Once the network is fixed the node registers on its own - no reboot.

**It waits for approval despite its token**: the enrollment token is
used up, expired or revoked - approve it by hand after comparing its
CA's fingerprint with the one on its console, or make a new token.

## janusctl can't reach a node

- **A timeout or a refused connection**: port 9505 isn't reachable from
  where `janusctl` runs - the node's firewall, the network, or the wrong
  address (`janusctl nodes` lists them from the Controller).
- **A certificate signed by an unknown authority**: `-ca` isn't the
  node's CA - or the node doesn't trust the fleet yet, and only its own
  certificates work.
- **`PermissionDenied`**: the certificate's role (or its scope) doesn't
  allow the call - `os:reader` reads only, `os:operator` runs the node,
  `os:admin` does everything.
- **An expired certificate**: a fleet certificate lasts 12 hours (an
  hour with an API token) - `janusctl login` again.

## HAProxy

- **A configuration refused**: HAProxy's own message comes back, with
  the line - `parsing [haproxy.cfg:3] : unknown keyword ...` - and
  nothing changed. Fix it, apply again. What differs from a
  distribution's `haproxy.cfg`: [your haproxy.cfg](../haproxy-config.md).
- **Accepted by the check, but HAProxy wouldn't start** - a `maxconn` or
  `ulimit-n` above the node's limit, a port already in use: the node
  reports HAProxy's reason and keeps the previous configuration and
  process ([connections and memory](../haproxy-config.md#connections-and-memory)).
- **Backend servers by name** are resolved when HAProxy starts, through
  the node's DNS: a name it can't resolve fails the start - give the
  node DNS servers, or use addresses.

## The network

- **A change reverted** - the node went back to its previous
  configuration because no confirmation reached it in time: the new
  addresses weren't reachable from where `janusctl` (or the Controller)
  confirms. Apply with `-no-confirm`, then `janusctl -endpoint <a new
  address>:9505 network confirm` from where it is reachable; or a longer
  `-timeout`.
- **DHCP on a second interface** is refused: the kernel's DHCP
  configures one interface, once, at boot. Give the others static
  addresses ([network configuration](../network-configuration.md)).
- **The clock**: `janusctl network status` shows the NTP servers in use,
  where they come from, and the last offset. A node whose clock reads
  before 2026 on its first boot waits up to two minutes for NTP before
  making its certificates.

## VRRP and BGP

- **Both nodes MASTER**: their VRRP adverts don't reach each other - the
  [firewall](../firewall.md) must accept `ip protocol 112` - or their
  `virtual_router_id` or interface differ. `janusctl network vrrp status`
  on each.
- **A node BACKUP when it should hold the address**: its HAProxy isn't
  healthy, and the track weight dropped its priority - see
  `janus_haproxy_up` and HAProxy's output.
- **A BGP session down, or a route withdrawn**: `janusctl network bgp
  status` - a `haproxy_*` protocol is held down while HAProxy doesn't
  answer, by design ([BGP](../bgp.md)).

## Updates

**An update reverted**: the node is back on the previous release, and
its event log says why - most often a HAProxy configuration the new
release refuses ([updating nodes](updates.md#when-an-update-reverted)).

## Lost access

- **A node's admin credential, lost**: day to day, the fleet's
  certificates replace it - from the Controller, or `janusctl login`.
- **The Controller, lost**: restore its backup on a new one - the nodes
  keep trusting it ([backups](../../dashboard/README.md#backups)); with no
  backup, the fleet's recovery kit brings the nodes under `janusctl`
  ([a fleet without a Controller](../fleet-without-controller.md)).

## Worth reporting

- **A SELinux denial** (`janus_selinux_denials_total` above 0, `avc:
  denied` in `dmesg`): the policy should allow everything Janus does
  and nothing else - a denial is a bug.
- **A crash** - `janus_haproxy_unexpected_exits_total` growing, a
  service restarting in a loop.

[GitHub issues](https://github.com/swenske/Janus/issues) - or, for a
security problem, privately: [security policy](../../SECURITY.md).
