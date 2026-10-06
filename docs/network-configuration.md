# Network configuration

A Janus node's hostname, interfaces (physical and 802.1Q VLANs), DNS
resolvers and NTP servers are one JSON document, managed through the API
(`NetworkService.NetworkConfig*`), `janusctl network`, and the
Controller's **System › Network** page. The same document can be given
when the node is created, so it boots straight into it.

With no configuration at all, a node behaves as before: the kernel's DHCP
client configures one interface at boot, and NTP comes from DHCP, else
`pool.ntp.org`.

## The document

```json
{
  "hostname": "lb1",
  "interfaces": [
    {"name": "wan", "mac": "52:54:00:12:34:01", "mode": "ADDRESSING_MODE_DHCP"},
    {"name": "eth1", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.0.2.10/24", "2001:db8::10/64"],
     "gateway": "192.0.2.1", "gateway6": "2001:db8::1", "mtu": 9000},
    {"name": "eth2", "mode": "ADDRESSING_MODE_NONE"},
    {"name": "eth2.100", "vlan": {"parent": "eth2", "id": 100}, "mode": "ADDRESSING_MODE_STATIC",
     "addresses": ["10.100.0.5/24"]}
  ],
  "dns": {"servers": ["192.0.2.53"], "search": ["example.net"]},
  "ntp": {"servers": ["ntp1.example.net", "ntp2.example.net:123"]}
}
```

Every field is optional. `janusctl network get` prints the one in effect,
in this exact format.

| Field | Meaning | When absent |
|---|---|---|
| `hostname` | RFC 1123 name, at most 64 characters | The DHCP hostname, else `janus-` and the last three bytes of the first Ethernet MAC |
| `interfaces` | What to configure, exactly | Interfaces stay as the kernel's boot DHCP left them |
| `dns.servers` | Up to 3 resolver IPs, written to `/etc/resolv.conf` | The resolvers from the boot DHCP lease |
| `dns.search` | Up to 6 search domains | The DHCP domain, if any |
| `ntp.servers` | Up to 2 servers, `host` or `host:port` | The NTP servers from the boot DHCP lease, else `pool.ntp.org` |

### Interfaces

- **`name`** - the kernel name (`eth0`), or the name a VLAN gets
  (`eth0.100`, any name up to 15 characters).
- **`mac`** - physical interfaces only: match the interface by MAC address
  and rename it to `name`. Names depend on probe order; MACs don't.
- **`vlan`** - `{"parent": "eth2", "id": 100}` creates an 802.1Q VLAN
  interface on `parent` (IDs 1-4094). A parent that isn't itself listed is
  brought up without an address.
- **`mode`**:
  - `ADDRESSING_MODE_DHCP` (the default) - the lease the kernel obtained
    at boot. See the limitation below.
  - `ADDRESSING_MODE_STATIC` - `addresses` (CIDR, IPv4 and/or IPv6, at
    least one), optional `gateway` (IPv4, on one of the interface's
    subnets) and `gateway6` (link-local, or on one of its subnets).
  - `ADDRESSING_MODE_NONE` - up, no address: a VLAN trunk.
  - `ADDRESSING_MODE_DISABLED` - down.
- **`mtu`** - 0 or absent leaves it unchanged.
- **`route_metric`** - metric of this interface's default routes. Absent:
  1024 plus the interface's position, so the first listed interface with a
  gateway is preferred.

Once `interfaces` lists anything, it is the whole picture: physical
interfaces not listed are brought down (unless they carry a listed VLAN),
and VLANs not listed are deleted. IPv6 link-local addresses are left to
the kernel; IPv6 is otherwise static: a node accepts no router
advertisement (the CIS benchmark's 3.3.2.7 - see
[kernel tuning](guide/kernel-tuning.md#the-cis-benchmark)), so it gets no
SLAAC address and no route from a router - give its `addresses` and its
`gateway6`.

### DHCP is the kernel's, for now

DHCP is done once, at boot, by the kernel itself (`ip=dhcp`). That means:

- it configures a **single interface**, the first to get an answer, so
  `ADDRESSING_MODE_DHCP` is only accepted on that interface - use static
  addressing on the others;
- the lease is **never renewed**: a DHCP server must hand the node a
  stable (reserved) address, or the node keeps using an expired lease.

A userspace DHCP client, with renewal and DHCP on any interface, is a
planned improvement.

## Applying a change: trial, confirm or revert

A configuration that cuts the node off must not strand it - there is no
console to fix it from. So applying is a **trial**:

1. The node switches to the new configuration at once.
2. It keeps it only if a `NetworkConfigConfirm` arrives within the confirm
   window (30 s by default, at most 300 s) - over a connection that
   reaches the node on an address the new configuration keeps. That
   connection *is* the proof the node is still reachable.
3. Otherwise it reverts to the previous configuration by itself. Only a
   confirmed configuration is written to disk, so a reboot during the
   trial also comes back on the previous one.

`janusctl network apply` and the Controller confirm for you: they reach
the node again - on the address in use if it's kept, else on the new
addresses it reports - and confirm there.

```sh
janusctl network get > net.json
$EDITOR net.json
janusctl network apply net.json            # applies, then confirms (or fails and the node reverts)
janusctl network apply -timeout 2m net.json
janusctl network apply -no-confirm net.json   # confirm yourself, from where you need to reach it:
janusctl -endpoint 192.0.2.10:9505 network confirm
janusctl network status                     # interfaces, lease, routes, DNS, clock
```

In the Controller, **System › Network** shows the same status and edits
the document as a form or as JSON; the diff is shown before applying.
When the node answers on a new address after the change, the Controller
records it and keeps managing the node there.

The node's TLS server certificate follows its addresses and hostname: it
is reissued (from the node's own CA) whenever they change, so clients
dialing the new address verify it. It is also renewed 30 days before it
expires.

## At creation time

The same document can be given when the node is created:

- **Install**: `janusctl lifecycle install -network-config net.json DISK BUNDLE_DIR`
  (`InstallRequest.network_config`).
- **An already-built raw image**, offline:
  `janusctl image seed-network -config net.json disk.raw` (like
  `seed-controller`, see [provisioning-a-node.md](provisioning-a-node.md);
  convert a qcow2 to raw and back around it).
- **NoCloud**: a `network` object in the `cidata` volume's `user-data`,
  next to or instead of `controller_address`/`controller_ca_cert`:

  ```json
  {"controller_address": "controller.example.net:8443", "controller_ca_cert": "-----BEGIN CERTIFICATE-----\n...",
   "network": {"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.0.2.10/24"], "gateway": "192.0.2.1"}]}}
  ```

The Controller's **Provision new nodes** panel builds all three for you.
A configuration given at creation is never overwritten by a later boot's
NoCloud volume: a node keeps what it has.

## Time

`janusd` is the NTP client - no extra daemon: it queries the servers
(SNTP), steps the clock when it's off by more than 128 ms and otherwise
lets the kernel slew it, then marks the clock synchronized, which also
lets the kernel write the time back to the hardware clock (x86). The
status shows the servers in use and where they came from
(`configured`, `dhcp` or `default`), the last offset and any error.

A node whose clock reads before 2026 at its first boot - a board with no
battery-backed clock, like a Raspberry Pi - waits up to two minutes for
NTP before generating its certificates, since they're dated by that
clock. SNTP isn't authenticated.
