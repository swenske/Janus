# BGP: the bird extension

Nodes built with the **bird** [extension](image-factory.md) run
[BIRD](https://bird.nic.cz) 2: BGP to announce the node's addresses to
your routers - typically an anycast address several Janus nodes serve,
withdrawn from a node whose HAProxy stops answering - and OSPF, BFD,
static routes and BIRD's other protocols. The configuration is BIRD's own
`bird.conf`, managed through the API, `janusctl` and the Controller's
**Apps › BGP** page. Without the extension, the image has no BIRD at all.

## A configuration

```
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
  receive its traffic: add it to an interface (as a `/32`) with the
  [network configuration](network-configuration.md), and bind HAProxy to it.
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

## Why BIRD 2

BIRD 3 (multithreaded) aborted on an assertion when reconfigured with a
protocol disabled from its control socket - exactly what the HAProxy gate
does. The 2.x branch is single-threaded, maintained, and what most
distributions ship.

## API

`NetworkService`: `BGPStatus`, `BGPGetConfig`, `BGPApplyConfig`
(`validate_only`). See [api-routes.md](api-routes.md).

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
