# VRRP: the keepalived extension

Nodes built with the **keepalived** [extension](image-factory.md) share
virtual IPs with VRRP: one node holds each virtual IP, and another takes
it over when that node fails - or when its HAProxy stops answering. The
configuration is keepalived's own `keepalived.conf`, managed through the
API, `janusctl` and the Controller's **Apps › VRRP** page. Without the
extension, the image has no keepalived at all.

## A configuration

Every node of a group gets the same file, but for its `priority` - the
highest one holds the virtual IP:

```text
global_defs {
    vrrp_version 3          # for sub-second adverts
}

track_file haproxy {
    # 0 while this node's HAProxy answers, 1 when it doesn't - kept by janusd.
    file /run/janus/keepalived/haproxy-health
}

vrrp_instance VI_1 {
    state BACKUP
    interface eth1
    virtual_router_id 51
    priority 150            # 100 on the other node
    advert_int 0.2          # a node that dies is replaced in ~3 adverts
    track_file {
        haproxy weight -100 # HAProxy not answering: priority 50, the other node takes over
    }
    virtual_ipaddress {
        192.0.2.100/24
    }
}
```

- **HAProxy's health**: janusd checks every 2 seconds - and at once when
  HAProxy starts, stops or exits - that HAProxy answers on its stats
  socket and isn't being stopped, and writes `0` or `1` into
  `/run/janus/keepalived/haproxy-health`. keepalived can't run scripts
  on a Janus node (there's no shell) - this file is how it follows
  HAProxy.
- **The track weight**: a negative weight larger than the gap between the
  nodes' priorities drops a node whose HAProxy doesn't answer below the
  others, which preempt it - the new holder takes the virtual IP before
  this node lets it go, so no request falls in between. `weight 0`
  puts the instance in FAULT instead: the node drops the virtual IP at
  once and the other takes it after its skew time - a short gap (~0.1 s
  with 0.2 s adverts). With a negative weight that node shows BACKUP,
  not FAULT: alert on HAProxy itself (below).
- **The advert interval**: a node that dies is noticed after about three
  missed adverts. Measured with a client every 50 ms: ~0.5 s with
  VRRPv3 and `advert_int 0.2`, ~3.5 s with VRRPv2's 1 second.
- **A deliberate stop drains first**: stopping HAProxy (`janusctl system
  service stop haproxy`), rebooting or shutting the node down, and the
  reboot that ends an update or a rollback all mark HAProxy unhealthy at
  once - the node gives its virtual IPs up - then keep its listeners open
  for `janusd -haproxy-drain` (2 seconds) before the soft stop: the
  traffic has moved to another node before any connection is refused. A
  rolling update therefore needs no manual step to keep the service up.
- The interface needs an address of its own on the VRRP network - set it
  with the [network configuration](network-configuration.md).
- With the [firewall](firewall.md), accept VRRP: `ip protocol 112 accept`
  (by number: nft can't name it here).
- keepalived is built for VRRP only: no LVS/IPVS, no iptables or nftables
  integration (the firewall ruleset is janusd's), no VRRP authentication
  (removed from VRRPv3), no scripts.

## Applying

```sh
janusctl network vrrp check keepalived.conf   # keepalived checks it, nothing changes
janusctl network vrrp apply keepalived.conf   # check, save, reload
janusctl network vrrp status
janusctl network vrrp apply /dev/null         # remove it: keepalived stops
```

keepalived checks the file itself (`keepalived --config-test`) before
anything is replaced; refused, its messages come back with their line
numbers. Applied, the file is saved on the node and keepalived reloads it
- instances whose state doesn't change keep their virtual IPs. At boot,
keepalived starts as soon as janusd has put the saved file in place;
with none saved, it doesn't run (`janusctl system services` shows it
`waiting`).

`status` shows each instance's state (MASTER, BACKUP, FAULT...), its
interface, virtual router ID, effective and configured priority,
virtual IPs and when it last changed state - read from keepalived's own
state dump.

## API

`NetworkService`: `VRRPStatus`, `VRRPGetConfig`, `VRRPApplyConfig`
(`validate_only`). See [api-routes.md](api-routes.md).

## Metrics

The [Janus exporter](metrics.md) reports `janus_vrrp_instance_state`
(1 for each instance's current state), `janus_vrrp_instance_effective_priority`
and `janus_vrrp_instance_became_master_total`. Alerts on a node that
can't hold its virtual IPs - with a negative track weight, a node whose
HAProxy is down is BACKUP, not FAULT, so `janus_haproxy_up` is the one
that says so:

```yaml
- alert: JanusHAProxyDown
  expr: janus_haproxy_up == 0
  for: 1m
- alert: JanusVRRPFault
  expr: janus_vrrp_instance_state{state="FAULT"} == 1
  for: 1m
```
