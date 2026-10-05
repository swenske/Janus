# Firewall: the nftables extension

A node built with the **nftables** [extension](image-factory.md) filters
traffic itself, with an nftables ruleset managed through the API, `janusctl`
and the Controller. Without the extension, the image has no `nft` at all.

## The ruleset

The ruleset is the node's **whole** nftables ruleset, in
[nft's own syntax](https://wiki.nftables.org/wiki-nftables/index.php/Main_Page):
applying one replaces everything (janusd flushes the previous ruleset
first), and an empty one removes the firewall. A node starts with none -
it filters nothing until you apply one.

A starting point, also the Controller's:

```nft
table inet filter {
	# Addresses to drop - editable live (below).
	set blocklist {
		type ipv4_addr
		flags interval, timeout
	}

	chain input {
		type filter hook input priority filter; policy drop;
		ct state established,related accept
		ct state invalid drop
		iif "lo" accept
		ip saddr @blocklist drop
		icmp type echo-request limit rate 10/second accept
		icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-advert, echo-request } accept
		tcp dport 9505 accept comment "Janus API"
		tcp dport 10056 accept comment "Janus exporter"
		tcp dport { 80, 443 } accept comment "HAProxy - your frontends' ports"
	}
}
```

Keep the API port (9505) reachable from wherever you manage the node, and
add what your HAProxy configuration listens on. For VRRP (the keepalived
extension) accept `ip protocol 112` - write protocols by number when nft
can't name them (its C library knows the common ones only); service names
(`ssh`, `https`...) work.

The kernel has nf_tables for `inet`, `ip`, `ip6` and `netdev`, with
connection tracking, NAT (`masquerade`, `redirect`, `snat`, `dnat`),
`log`, `reject`, `limit`, `quota`, `ct count`, `fib`, `numgen`/`jhash`,
`synproxy`, and `socket`/`tproxy` for HAProxy's transparent proxying. No
iptables.

## Applying: on trial

```sh
janusctl network firewall check firewall.nft   # validate only
janusctl network firewall apply firewall.nft   # apply on trial, then confirm
```

1. The node checks the ruleset with nft (`check` stops there). Errors
   point at the ruleset's own lines.
2. It applies it **on trial**, and puts the previous ruleset back by
   itself after 30 seconds (`-timeout`, 5 to 300) unless the trial is
   confirmed.
3. `janusctl` confirms it over a **new connection**. The connection that
   applied the ruleset is established, and connection tracking keeps it
   open whatever the ruleset says - a confirmation over it would prove
   nothing, so the node refuses it. If a new connection can't get through,
   nothing confirms the trial and the node reverts.
4. Confirmed, the ruleset is saved on the node and applied at every boot.

`-no-confirm` leaves the trial running, to confirm later with `janusctl
network firewall confirm` (a new connection too). The Controller's
**Apps › Firewall** page does the same: it applies, then confirms over a
fresh connection, and tells you if it couldn't.

At boot, janusd applies the saved ruleset as soon as it starts, before it
listens on anything; until then - the kernel booting - the node has no
ruleset.

## Sets: lists edited live

Named sets in the ruleset - a block list, an allow list - can be edited
without reloading the ruleset:

```sh
janusctl network firewall sets
janusctl network firewall set-add inet filter blocklist 192.0.2.7 198.51.100.0/24
janusctl network firewall set-add -timeout 1h inet filter blocklist 203.0.113.9
janusctl network firewall set-del inet filter blocklist 192.0.2.7
```

Elements are written in nft's syntax: an address, a prefix
(`198.51.100.0/24`), a range (`10.0.0.1-10.0.0.9`), a concatenation for a
concatenated set type (`192.0.2.1 . 443`). An element added without a
timeout is **kept**: the node adds it back after every apply and boot. An
element with a timeout (the set needs `flags timeout`) expires, and
doesn't survive re-applying the ruleset or a reboot. Elements written in
the ruleset itself are part of the ruleset.

## API

`NetworkService`: `FirewallList` (the module, the live ruleset, a trial
in progress), `FirewallGetRuleset` (the saved one), `FirewallApplyRuleset`
(`validate_only`, `confirm_timeout_seconds`), `FirewallConfirm`,
`FirewallSets`, `FirewallSetUpdate`. See [api-routes.md](api-routes.md).

## Metrics

The [Janus exporter](metrics.md) reports `janus_firewall_configured`,
`janus_firewall_trial_pending`, and `janus_firewall_set_elements` per set.
