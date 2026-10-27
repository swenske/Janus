# BGP: the bird extension

Nodes built with the **bird** [extension](image-factory.md) run
[BIRD](https://bird.nic.cz) 2: BGP to announce the node's addresses to
your routers - typically an anycast address several Janus nodes serve,
withdrawn from a node whose HAProxy stops answering - and OSPF, BFD,
static routes and BIRD's other protocols. The configuration is BIRD's own
`bird.conf`, managed through the API, `janusctl` and the Controller's
**Apps › BGP** page. Without the extension, the image has no BIRD at all.

## A configuration

```bird2
router id 192.0.2.1;                 # this node's own address

protocol device {}

# The anycast address HAProxy serves, withdrawn while HAProxy doesn't answer.
protocol static haproxy_anycast {
    ipv4;
    route 192.0.2.100/32 blackhole;
}

protocol bgp upstream {
    local 192.0.2.1 as 65001;
    neighbor 192.0.2.254 as 65000;
    password "secret";               # TCP MD5, if the peer asks for one
    bfd on;                          # with "protocol bfd {}", for fast failure detection
    ipv4 {
        import none;
        export where proto = "haproxy_anycast";
    };
}
```

- **HAProxy's health**: every protocol whose name starts with
  `haproxy_` is kept down while this node's HAProxy doesn't answer - on
  its stats socket, and from the moment janusd begins stopping it. janusd
  checks every second - and at once when HAProxy starts, stops or exits -
  and disables those protocols over BIRD's control socket; once HAProxy
  answers again, it enables the ones it disabled. A
  `haproxy_*` static protocol therefore takes its routes - and the BGP
  announcements exporting them - off the network while HAProxy can't
  serve them. A `haproxy_*` BGP session would itself go down. A protocol
  your `bird.conf` disables (`disabled;`) is left alone.
- **A deliberate stop drains first**: stopping HAProxy, rebooting or
  shutting the node down, and the reboot that ends an update or a
  rollback withdraw the `haproxy_*` routes at once, then keep HAProxy's
  listeners open for `janusd -haproxy-drain` (2 seconds) before the soft
  stop - the routers have moved the traffic before any connection is
  refused.
- The anycast address must also be an address of the node, for HAProxy to
  receive its traffic: add it to an interface (as a `/32`, or a `/128` -
  added without duplicate address detection, so every node keeps it, and
  never used as the source of the node's own connections) with the
  [network configuration](network-configuration.md), and bind HAProxy to
  it.
- Without a `log` statement, janusd adds `log stderr all;` to the file
  BIRD reads (not to the one it saves), so BIRD's messages are its
  service's log: `janusctl system logs bird`.
- With the [firewall](firewall.md), accept BGP (`tcp dport 179 accept`
  from your peers), and BFD (`udp dport { 3784, 3785 }`) or OSPF (`ip
  protocol 89`) if you use them.
- BIRD here is built without its client (`birdc`), RPKI over SSH, or MPLS
  in the kernel protocol. There's no shell to run `birdc` from: the API
  reads what you need over BIRD's control socket.

A protocol `kernel` puts what BIRD learns into the node's routing table
(`janusctl network status` shows it); without one, BIRD only announces.

## Applying

```sh
janusctl network bgp check bird.conf   # BIRD checks it (bird -p), nothing changes
janusctl network bgp apply bird.conf   # check, save, reconfigure
janusctl network bgp status
janusctl network bgp apply /dev/null   # remove it: BIRD stops
```

BIRD checks the file itself before anything is replaced; refused, its
message comes back with the line and column. Applied, the file is saved on
the node and BIRD reconfigures - sessions whose configuration doesn't
change stay up. A reconfiguration brings disabled protocols back as the
file says; janusd puts the `haproxy_*` ones down again at once if HAProxy
still doesn't answer. At boot, BIRD starts as soon as janusd has put the
saved file in place; with none saved, it doesn't run (`janusctl system
services` shows it `waiting`).

`status` lists every protocol: its type, state, since when, BIRD's info
(`Established`, `Active`, the last error), the BGP neighbor and AS, the
routes it imported and exported per channel, and whether janusd holds it
down.

## Active/active behind RouterOS: BGP multipath

Two nodes announcing the same anycast address give a router two routes
to it. RouterOS installs only the best one by default: one node takes
all the traffic, the other waits for it to fail - active/standby.
RouterOS 7.22 and later install several (**multipath**), and spread the
flows over the nodes - active/active, like an ECMP route to each.

On the router - its own address `192.0.2.254`, the nodes `192.0.2.1` and
`192.0.2.2` in one AS (65001), the anycast `192.0.2.100/32`:

```text
/routing bfd configuration add interfaces=vlan-front min-rx=200ms min-tx=200ms multiplier=3
/routing filter rule add chain=janus-in rule="if (dst in 192.0.2.100/32 && dst-len == 32) { accept } else { reject }"
/routing bgp instance add name=edge as=65000 router-id=192.0.2.254 multipath=2
/routing bgp connection add name=lb-1 instance=edge local.role=ebgp remote.address=192.0.2.1 remote.as=65001 input.filter=janus-in hold-time=9s keepalive-time=3s use-bfd=yes
/routing bgp connection add name=lb-2 instance=edge local.role=ebgp remote.address=192.0.2.2 remote.as=65001 input.filter=janus-in hold-time=9s keepalive-time=3s use-bfd=yes
/ip settings set ipv4-multipath-hash-policy=l4
/ip firewall filter add chain=forward action=accept dst-address=192.0.2.100 place-before=[find action=fasttrack-connection]
/ip firewall filter add chain=forward action=accept src-address=192.0.2.100 place-before=[find action=fasttrack-connection]
```

- **`multipath` is a number** - how many routes RouterOS keeps for a
  prefix: at least the number of nodes. RouterOS 7.25 refuses `yes`, and
  **`0` leaves the prefix without any active route** - the anycast
  address unreachable (measured); `1`, or leaving it out, is one path.
  The nodes announce the same attributes - one AS, as above and in the
  lab: the routes are equal (nodes in different ASes weren't tried).
- **BFD** (`bfd on;` and `protocol bfd` in each node's `bird.conf`, as in
  the configuration above): a node lost leaves the router's routes after
  BFD's three missed packets - 0.6 s at 200 ms - not the hold time.
- **How the flows spread**: by default RouterOS hashes on the addresses
  only (`ipv4-multipath-hash-policy=l3`) - every connection from one
  client, or from behind one NAT, goes to the same node. `l4` adds the
  ports: connections from one client spread too. Then keep the anycast
  address out of FastTrack, as above, or about one connection in four is
  reset (RouterOS 7.25, measured: 56 of 240) - none once out of it.
- **What a route change does to open connections**: ECMP keeps no state.
  A route withdrawn - a node lost, its HAProxy stopped, the node drained
  for an update - or announced again moves the connections hashed to it
  to the other node, which resets them; new connections lose nothing.
  With one path, every open connection moves each time.
- **Which node the router uses**: a cluster member's page says what
  the node announces ("BGP announcing", "withdrawn", "down"); the
  router's choice - multipath, master, backup - only the router knows:
  a [read-only RouterOS hook](guide/clusters.md#routeros) reads it for
  the members' pages.

Measured in the test lab (RouterOS 7.25 on an RB4011, two nodes, BFD at
200 ms × 3, 16 keep-alive connections under load and a new connection
every 50 ms), one path against multipath with `l4`:

| | One path (the default) | Multipath |
|---|---|---|
| New connections, from four clients | all on one node | each client's spread over both (20 to 40 of 60 on each) |
| A node lost (the virtual machine killed) | 13 lost in 1,177 - all of them for 0.57 s, and 2 when the node came back and took the route again | 5 lost in 1,180 - those hashed to it during BFD's 0.6 s; none when it came back |
| Its HAProxy stopped for 10 s | 3 lost | none |
| Withdrawn, then announced again | 5 lost | none |
| Open connections reset by a withdrawal / an announcement | 16 of 16 / 16 of 16 | 10 of 16 / 7 of 16 |
| An update of both nodes through the Controller, each drained first | 3 lost in 3,471 (a pre-release's campaign) | none lost in 2,909 |

A virtual IP ([VRRP](vrrp.md)) fails over in about the same time - 0.6 s
at 0.2 s adverts - but one node serves at a time.

## Why BIRD 2

BIRD 3 (multithreaded) aborted on an assertion when reconfigured with a
protocol disabled from its control socket - exactly what the HAProxy gate
does. The 2.x branch is single-threaded, maintained, and what most
distributions ship.

## API

`NetworkService`: `BGPStatus`, `BGPGetConfig`, `BGPApplyConfig`
(`validate_only`). See [api-routes.md](api-routes.md). Reading the
configuration takes `os:operator`: it may hold session passwords
(`password "..."`), which a reader doesn't get.

## Metrics

The [Janus exporter](metrics.md) reports `janus_bgp_protocol_up`,
`janus_bgp_session_established` (per neighbor), `janus_bgp_routes`
(imported and exported, per protocol and channel) and
`janus_bgp_protocol_held_down`. An alert on a session down:

```yaml
- alert: JanusBGPSessionDown
  expr: janus_bgp_session_established == 0
  for: 2m
```
