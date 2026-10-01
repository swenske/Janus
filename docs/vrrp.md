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

```
track_file haproxy {
    # 0 while this node's HAProxy answers, 1 when it doesn't - kept by janusd.
    file /run/janus/keepalived/haproxy-health
}

vrrp_instance VI_1 {
    state BACKUP
    interface eth1
    virtual_router_id 51
    priority 150            # 100 on the other node
    advert_int 1
    track_file {
        haproxy weight 0    # HAProxy not answering: give the virtual IP up
    }
    virtual_ipaddress {
        192.0.2.100/24
    }
}
```

- **HAProxy's health**: janusd checks every 2 seconds that HAProxy answers
  on its stats socket - and isn't being stopped: a soft stop closes the
  listeners before the process exits - and writes `0` or `1` into
  `/run/janus/keepalived/haproxy-health`. Tracked with `weight 0`, a `1`
  puts the instance in FAULT: the node gives its virtual IPs up until
  HAProxy answers again. keepalived can't run scripts on a Janus node
  (there's no shell) - this file is how it follows HAProxy.
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
and `janus_vrrp_instance_became_master_total`. An alert on a node that
should hold a virtual IP:

```yaml
- alert: JanusVRRPFault
  expr: janus_vrrp_instance_state{state="FAULT"} == 1
  for: 1m
```
