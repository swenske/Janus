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
  traffic has moved to another node before any connection is refused.
  keepalived is then stopped before the machine goes down - it sends
  its priority 0, and no peer waits for adverts that won't come. A
  rolling update therefore needs no manual step to keep the service up.
  A janusd restart (the node page's **Restart janusd**, `janusctl system
  restart`) and a shutdown by the hypervisor through the guest agent
  (Proxmox's Shutdown) do the same: the node gives its virtual IPs up and
  waits the drain delay before keepalived stops with janusd - last of the
  extensions - and the restarted janusd starts it again: the IPs come
  back to it by priority.
- The interface needs an address of its own on the VRRP network - set it
  with the [network configuration](network-configuration.md). Applying a
  network configuration leaves the virtual IPs where keepalived put them.
- HAProxy may bind the virtual IP itself (`bind 192.0.2.100:443`) on
  every node, the one holding it or not: `net.ipv4.ip_nonlocal_bind` and
  its IPv6 twin are on by default, as HAProxy advises
  ([kernel tuning](guide/kernel-tuning.md)).
- With the [firewall](firewall.md), accept VRRP: `ip protocol 112 accept`
  (by number: nft can't name it here).
- keepalived is built for VRRP only: no LVS/IPVS, no iptables or nftables
  integration (the firewall ruleset is janusd's), no VRRP authentication
  (removed from VRRPv3), no scripts.
- An IPv6 virtual IP can go with an IPv4 instance, in
  `virtual_ipaddress_excluded`: it isn't advertised by VRRP, it moves
  with the instance and is announced by unsolicited neighbour
  advertisements, as the IPv4 one by gratuitous ARP. The interface then
  needs an IPv6 address of its own and a `gateway6` - static: a Janus
  node takes no address or route from router advertisements. Proven by
  `make qemu-vrrp-ipv6-test` (unicast adverts, a neighbour's table
  following a failover):

  ```text
  vrrp_instance VI_1 {
      ...
      virtual_ipaddress {
          192.0.2.100/24
      }
      virtual_ipaddress_excluded {
          2001:db8::100/64
      }
  }
  ```

- No VMAC or IPVLAN interfaces: the check refuses `use_vmac`,
  `use_ipvlan` and `disable_local_igmp` (their companion). The kernel has
  neither macvlan nor ipvlan, a VMAC needs `net.ipv4.conf.all.rp_filter`
  at 0 - which the [CIS benchmark](guide/kernel-tuning.md#the-cis-benchmark)
  forbids - and keepalived may change no kernel parameter on a Janus node
  (SELinux). The virtual IPs go on the interface itself, with its own MAC
  address - announced by gratuitous ARP when they move.

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
(`validate_only`). See [api-routes.md](api-routes.md). Reading the
configuration takes `os:operator`: it may hold `auth_pass`, which a
reader doesn't get.

## Metrics

The [Janus exporter](metrics.md) reports `janus_vrrp_instance_state`
(1 for each instance's current state), `janus_vrrp_instance_effective_priority`
and keepalived's own counters for each instance. janusd reads them from
the same state dump as `status`: nothing else on the node signals
keepalived. They count since keepalived started - a reload keeps them.
The last column is [keepalived-exporter](https://github.com/mehdy/keepalived-exporter)'s
name for the same counter, for dashboards made for it: its labels are
`iname`, `intf` and `vrid`, Janus's the instance's name in `instance`
(`exported_instance` once Prometheus has scraped it).

| Metric | keepalived's counter | keepalived-exporter's |
| --- | --- | --- |
| `janus_vrrp_instance_became_master_total` | `become_master` | `keepalived_become_master_total` |
| `janus_vrrp_instance_left_master_total` | `release_master` | `keepalived_release_master_total` |
| `janus_vrrp_instance_adverts_sent_total` | `advert_sent` | `keepalived_advertisements_sent_total` |
| `janus_vrrp_instance_adverts_received_total` | `advert_rcvd` | `keepalived_advertisements_received_total` |
| `janus_vrrp_instance_priority_zero_sent_total` | `pri_zero_sent` | `keepalived_priority_zero_sent_total` |
| `janus_vrrp_instance_priority_zero_received_total` | `pri_zero_rcvd` | `keepalived_priority_zero_received_total` |
| `janus_vrrp_instance_advert_errors_total{error="packet_length"}` | `packet_len_err` | `keepalived_packet_length_errors_total` |
| `janus_vrrp_instance_advert_errors_total{error="ttl"}` | `ip_ttl_err` | `keepalived_ip_ttl_errors_total` |
| `janus_vrrp_instance_advert_errors_total{error="packet_type"}` | `invalid_type_rcvd` | `keepalived_invalid_type_received_total` |
| `janus_vrrp_instance_advert_errors_total{error="address_list"}` | `addr_list_err` | `keepalived_address_list_errors_total` |
| `janus_vrrp_instance_advert_errors_total{error="authentication_type"}` | `invalid_authtype` | `keepalived_authentication_invalid_total` |

A master's advertisements sent grow by one every `advert_int`, its
backups' received with them. Received flat on every node of a pair:
they don't hear each other - each is MASTER (split brain). A master
whose tracking fails gives the virtual IPs up with two priority 0
advertisements, and keepalived counts one in its priority 0 sent - the
backups count both. The errors
are packets keepalived drops - shorter or longer than they say
(`packet_length`), a TTL other than 255: routed (`ttl`), not an
advertisement (`packet_type`), with an authentication type
(`authentication_type`: the extension's keepalived is built without
VRRP authentication and takes only adverts without) - and
advertisements for other virtual IPs than the instance's own
(`address_list`). Some counters are left out rather than shown as a 0
that means nothing: keepalived 2.4.3 never counts an advertisement
interval mismatch (it logs `advertisement interval mismatch` and drops
the packet), and the authentication counters don't exist without VRRP
authentication.

Alerts on a node that can't hold its virtual IPs - with a negative
track weight, a node whose HAProxy is down is BACKUP, not FAULT, so
`janus_haproxy_up` is the one that says so -, and on advertisements
with errors: a router on the LAN speaks VRRP for the instance
differently, and a dropped advertisement can leave two masters:

```yaml
- alert: JanusHAProxyDown
  expr: janus_haproxy_up == 0
  for: 1m
- alert: JanusVRRPFault
  expr: janus_vrrp_instance_state{state="FAULT"} == 1
  for: 1m
- alert: JanusVRRPAdvertErrors
  expr: sum by (instance, exported_instance, error) (increase(janus_vrrp_instance_advert_errors_total[15m])) > 0
```
