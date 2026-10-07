# Kernel tuning (sysctl)

A node's kernel parameters: the ones HAProxy depends on, which you can
change - tested on trial, kept once applied, back to Janus's defaults any
time - and the CIS benchmark's, which nothing changes.

There's no shell and no `sysctl` command on a node: the parameters go
through the API - the node page's **System › Sysctl**, `janusctl system
sysctl`, or the `SystemService.Sysctl*` calls
([API reference](../private-cloud/api-reference.md)).

## What you can change

Only these parameters, each within its bounds: the node refuses any other
key and any other value, whoever asks. Janus's defaults are HAProxy's own
recommendations
([HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system)),
unless the CIS benchmark or Janus's own listeners say otherwise - the
source port range starts at 10240, above janusd's 9505 and the Janus
exporter's 10056, as HAProxy's guidance asks - and the kernel's own value
for the parameters that depend on the machine's memory, read at boot.

<!-- generated: editable - go test ./internal/sysctl -run TestKernelTuningDoc -update -->

| Parameter | Janus's default | Allowed | Takes effect | HAProxy recommends |
|---|---|---|---|---|
| `net.core.somaxconn` | `60000` | 4096-65535 connections | HAProxy's next reload | 60000 |
| `net.ipv4.ip_local_port_range` | `10240 65023` | low 1024-32768, high 5119-65535 ports | new connections | 1024 65023 |
| `net.ipv4.ip_local_reserved_ports` | (none) | up to 32 ports or ranges, 1024-65535 | new connections | - |
| `net.ipv4.tcp_tw_reuse` | `1` | 0, 1, 2 | new connections | 1 |
| `net.ipv4.tcp_fin_timeout` | `30` | 30-120 seconds | at once | 30 |
| `net.ipv4.tcp_synack_retries` | `3` | 1-5 retries | at once | 3 |
| `net.ipv4.ip_nonlocal_bind` | `1` | 0, 1 | HAProxy's next reload | 1 |
| `net.ipv6.ip_nonlocal_bind` | `1` | 0, 1 | HAProxy's next reload | 1 |
| `net.core.netdev_max_backlog` | `10000` | 1000-65536 packets | at once | 10000 |
| `net.ipv4.tcp_rmem` | the kernel's: 4096 131072, maximum from the memory (32 MiB at most) | min 4 KiB-64 KiB, default 4 KiB-4 MiB, max 64 KiB-64 MiB | new connections | 4096 16060 262144 (optional) |
| `net.ipv4.tcp_wmem` | the kernel's: 4096 16384, maximum from the memory (4 MiB at most) | min 4 KiB-64 KiB, default 4 KiB-4 MiB, max 64 KiB-64 MiB | new connections | 4096 16384 262144 (optional) |
| `fs.file-max` | the kernel's: about 10% of the memory, at 1 KiB a file | 8192-67108864 files | at once | - |
| `net.ipv4.tcp_keepalive_time` | `7200` | 60-7200 seconds | at once | - |
| `net.ipv4.tcp_keepalive_intvl` | `75` | 10-75 seconds | at once | - |
| `net.ipv4.tcp_keepalive_probes` | `9` | 3-20 probes | at once | - |
| `net.ipv4.tcp_fastopen` | `1` | 0, 1, 2, 3 | at once | - |
| `net.netfilter.nf_conntrack_max` | the kernel's: from the memory: 1 per 128 KiB up to 1 GiB, then 65536, 262144 above 4 GiB | 16384-4194304 entries | at once | - |

What each one does:

- **`net.core.somaxconn`** - Upper limit of every listening socket's accept queue - the backlog listen() asks for. HAProxy: HAProxy asks for a backlog equal to the frontend's maxconn (or its backlog setting) and the kernel caps it here. SYN cookies being always on (CIS 3.3.1.18), it also bounds the SYN queue before cookies take over. HAProxy reads it when it opens its listeners: a change reaches it at its next reload. Risk: A full queue holds connections HAProxy hasn't accepted yet, per listener: memory, on a small node. A very high limit also hides saturation - clients wait instead of failing fast. The kernel's default: 4096. Sources: [HAProxy configuration: backlog](https://docs.haproxy.org/3.4/configuration.html#4.2-backlog), [HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system), [HAProxy: doc/linux-syn-cookies.txt](https://github.com/haproxy/haproxy/blob/master/doc/linux-syn-cookies.txt).
- **`net.ipv4.ip_local_port_range`** - Source ports of outgoing connections: HAProxy's connections to its servers. HAProxy: Bounds the connections (TIME_WAIT ones included) HAProxy can have at once to one server address and port; when they run out, it fails with "Out of local source ports on the system". Janus starts at 10240, above its own listeners (janusd 9505, the Janus exporter 10056), as HAProxy's guidance asks. Risk: An outgoing connection can take a listening port inside the range, and a reload binding that port then fails: a change is refused while a port the node listens on is inside the range without being reserved (net.ipv4.ip_local_reserved_ports). The kernel's default: 32768 60999. Sources: [HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system), [HAProxy configuration: source](https://docs.haproxy.org/3.4/configuration.html#4.2-source).
- **`net.ipv4.ip_local_reserved_ports`** - Ports never handed out as source ports. HAProxy: Keeps a port HAProxy - or another service - listens on, inside the source port range, from being taken by an outgoing connection. Risk: Each reserved port is one source port less. The kernel's default: (none). Sources: [Linux: IP sysctl](https://docs.kernel.org/networking/ip-sysctl.html).
- **`net.ipv4.tcp_tw_reuse`** - Reuse of a TIME_WAIT socket for a new outgoing connection when TCP timestamps make it safe: 0 off, 1 on, 2 loopback only. HAProxy: Frees the source ports of HAProxy's connections to its servers - needed above a few hundred connections per second. Risk: Low: a server that sends no TCP timestamps just gets no reuse. The kernel's default: 2. Sources: [HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system).
- **`net.ipv4.tcp_fin_timeout`** - How long an orphaned connection stays in FIN_WAIT_2. HAProxy: HAProxy closes connections itself: a shorter timeout releases dead ones sooner. Risk: HAProxy warns of problems below 25 to 30 seconds - hence 30 at least. The kernel's default: 60. Sources: [HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system).
- **`net.ipv4.tcp_synack_retries`** - SYN-ACK retransmissions for an incoming connection attempt. HAProxy: The amplification factor of a SYN flood: with 3, a half-open connection is dropped after about 15 seconds instead of 63. Risk: Too low and clients on lossy networks can't connect - 1 only during an attack. The kernel's default: 5. Sources: [HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system).
- **`net.ipv4.ip_nonlocal_bind`** - Binding to an IPv4 address the node doesn't have (yet). HAProxy: HAProxy starts and reloads with a bind on a virtual IP (VRRP) on every node of the group, not only the one holding it - HAProxy's advice is to always leave it on. Risk: A mistyped bind address no longer fails HAProxy's start, where Janus would put the previous configuration back. The kernel's default: 0. Sources: [HAProxy management: well-known traps to avoid](https://docs.haproxy.org/3.4/management.html#11), [HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system).
- **`net.ipv6.ip_nonlocal_bind`** - Binding to an IPv6 address the node doesn't have (yet). HAProxy: As for IPv4: a bind on an IPv6 virtual IP works on every node of the group. Risk: A mistyped bind address no longer fails HAProxy's start. The kernel's default: 0. Sources: [HAProxy management: well-known traps to avoid](https://docs.haproxy.org/3.4/management.html#11), [HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system).
- **`net.core.netdev_max_backlog`** - Received packets waiting for the network stack, per CPU. HAProxy: Absorbs bursts of traffic - an effect HAProxy calls minimal. Risk: Memory and latency while the queue is full. The kernel's default: 1000. Sources: [HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system).
- **`net.ipv4.tcp_rmem`** - Receive buffer of each TCP socket: minimum, default and maximum, auto-tuned in between. HAProxy: Kernel memory per connection: smaller buffers let a node hold more connections at once. Risk: A low maximum caps a connection's throughput at about the maximum divided by the round-trip time - 256 KiB at 50 ms is about 40 Mbit/s. The kernel's default: 4096 131072, maximum from the memory (32 MiB at most). Sources: [HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system), [HAProxy configuration: tune.rcvbuf.client](https://docs.haproxy.org/3.4/configuration.html#3.2-tune.rcvbuf.client).
- **`net.ipv4.tcp_wmem`** - Send buffer of each TCP socket: minimum, default and maximum, auto-tuned in between. HAProxy: Kernel memory per connection: smaller buffers let a node hold more connections at once. Risk: A low maximum caps a connection's throughput at about the maximum divided by the round-trip time. The kernel's default: 4096 16384, maximum from the memory (4 MiB at most). Sources: [HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system), [HAProxy configuration: tune.sndbuf.client](https://docs.haproxy.org/3.4/configuration.html#3.2-tune.sndbuf.client).
- **`fs.file-max`** - Open files on the whole system. HAProxy: HAProxy runs as uid 1000 without CAP_SYS_ADMIN, so it's held to it: at the limit, accept() and socket() fail with ENFILE ("Too many sockets on the system"). Risk: More descriptors, more memory they can take. The kernel's default: about 10% of the memory, at 1 KiB a file. Sources: [HAProxy management: file-descriptor limitations](https://docs.haproxy.org/3.4/management.html#5).
- **`net.ipv4.tcp_keepalive_time`** - Idle time before TCP probes a connection that has keepalive on. HAProxy: HAProxy uses it for option tcpka, clitcpka and srvtcpka when clitcpka-idle, -intvl and -cnt (srvtcpka-*) aren't set. Risk: Aggressive values add traffic and drop connections over unstable links. The kernel's default: 7200. Sources: [HAProxy configuration: clitcpka-idle](https://docs.haproxy.org/3.4/configuration.html#4.2-clitcpka-idle).
- **`net.ipv4.tcp_keepalive_intvl`** - Time between two keepalive probes. HAProxy: HAProxy uses it for option tcpka, clitcpka and srvtcpka when clitcpka-idle, -intvl and -cnt (srvtcpka-*) aren't set. Risk: Aggressive values add traffic and drop connections over unstable links. The kernel's default: 75. Sources: [HAProxy configuration: clitcpka-intvl](https://docs.haproxy.org/3.4/configuration.html#4.2-clitcpka-intvl).
- **`net.ipv4.tcp_keepalive_probes`** - Unanswered keepalive probes before the connection is dropped. HAProxy: HAProxy uses it for option tcpka, clitcpka and srvtcpka when clitcpka-idle, -intvl and -cnt (srvtcpka-*) aren't set. Risk: Few probes drop connections over unstable links. The kernel's default: 9. Sources: [HAProxy configuration: clitcpka-cnt](https://docs.haproxy.org/3.4/configuration.html#4.2-clitcpka-cnt).
- **`net.ipv4.tcp_fastopen`** - TCP Fast Open: 1 as a client, 2 as a server, 3 both. HAProxy: A bind line's tfo only works with the server bit (2): data in the SYN saves a round trip on repeat connections. The flags without a cookie or for every listener (0x4, 0x200, 0x400) aren't offered. Risk: Some middleboxes drop SYNs that carry data, and that data can be replayed - HAProxy advises enabling it only once well tested. The kernel's default: 1. Sources: [HAProxy configuration: tfo](https://docs.haproxy.org/3.4/configuration.html#5.1-tfo), [Linux: IP sysctl](https://docs.kernel.org/networking/ip-sysctl.html).
- **`net.netfilter.nf_conntrack_max`** - Size of the connection-tracking table the firewall's stateful (ct) rules use. HAProxy: With the nftables extension and stateful rules, each of HAProxy's client and server connections takes an entry: a full table drops the packets of new connections. Risk: About 320 bytes of memory an entry. Without conntrack rules it has no effect. The kernel's default: from the memory: 1 per 128 KiB up to 1 GiB, then 65536, 262144 above 4 GiB. Sources: [Linux: netfilter conntrack sysctl](https://docs.kernel.org/networking/nf_conntrack-sysctl.html).
<!-- end: editable -->

## Shown, not changed

The page and `janusctl system sysctl list` also show these, for
diagnosis - none can change:

<!-- generated: readonly - go test ./internal/sysctl -run TestKernelTuningDoc -update -->

| Parameter | Why it doesn't change | Sources |
|---|---|---|
| `net.ipv4.tcp_max_syn_backlog` | Remembered connection requests that haven't completed their handshake, per listener. No effect on Janus: SYN cookies are always on (CIS 3.3.1.18) and the kernel only reads it with them off - the SYN queue is bounded by the backlog, so net.core.somaxconn is the one to tune. | [HAProxy Enterprise: tune the operating system](https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system), [HAProxy: doc/linux-syn-cookies.txt](https://github.com/haproxy/haproxy/blob/master/doc/linux-syn-cookies.txt) |
| `fs.nr_open` | Highest open-files limit a process may be given. No effect: Janus's init gives janusd and HAProxy a limit of 524288, below it - HAProxy derives its maxconn from that limit. | [HAProxy management: file-descriptor limitations](https://docs.haproxy.org/3.4/management.html#5), [HAProxy configuration: fd-hard-limit](https://docs.haproxy.org/3.4/configuration.html#3.1-fd-hard-limit) |
| `net.ipv4.tcp_congestion_control` | TCP congestion control algorithm of new connections. Only cubic is built into Janus's kernel - no BBR. | [HAProxy configuration: cc](https://docs.haproxy.org/3.4/configuration.html#5.1-cc) |
| `net.ipv4.tcp_available_congestion_control` | The algorithms a bind or server line's cc may name. The kernel's list - what Janus builds in. | [HAProxy configuration: cc](https://docs.haproxy.org/3.4/configuration.html#5.1-cc) |
| `kernel.core_pattern` | Where the kernel writes a crashed process's core dump. Forbidden. A pattern starting with \| would run a program as root on every crash: never changed on Janus. | [HAProxy configuration: set-dumpable](https://docs.haproxy.org/3.4/configuration.html#3.1-set-dumpable) |
<!-- end: readonly -->

## The CIS benchmark

Janus's baseline follows the **CIS Debian Linux 13 Benchmark v1.0.0**,
profile **Level 2 - Server**, for its controls on kernel parameters
(sections 1.5 and 3.3). It's the newest CIS Linux benchmark, and these
controls only check `/proc/sys` values, so they apply to Janus as they
are; the only Level 2 ones (1.5.2 and 3.3.1.1) hold anyway. 1.3.1.4
(AppArmor) doesn't apply: Janus confines its daemons with SELinux.

Every boot writes these values first, and again after the values you
saved; no call changes them, every change is refused while one doesn't
hold, and the values a change writes are checked against them before it
stays. The kernel uses each interface's own value of a few keys, and
IPv6 doesn't copy `all` or `default` onto the interfaces that already
exist - the boot DHCP's comes up before Janus's init runs - so those are
written, and checked, on every interface too.

<!-- generated: cis - go test ./internal/sysctl -run TestKernelTuningDoc -update -->

| Control | Level | Key | Janus writes | Compliant |
|---|---|---|---|---|
| 1.5.1 | 1 | `fs.protected_hardlinks` | `1` | 1 |
| 1.5.2 | 2 | `fs.protected_symlinks` | `1` | 1 |
| 1.5.3, 1.5.10 | 1 | `kernel.yama.ptrace_scope` | `2` | 1 or 2 or 3 |
| 1.5.4 | 1 | `fs.suid_dumpable` | `0` | 0 |
| 1.5.5 | 1 | `kernel.dmesg_restrict` | `1` | 1 |
| 1.5.8 | 1 | `kernel.kptr_restrict` | `2` | 1 or 2 |
| 1.5.9 | 1 | `kernel.randomize_va_space` | `2` | 2 |
| 3.3.1.1 | 2 | `net.ipv4.ip_forward` | `0` | 0 |
| 3.3.1.2 | 1 | `net.ipv4.conf.all.forwarding` | `0` | 0 |
| 3.3.1.3 | 1 | `net.ipv4.conf.default.forwarding` | `0` | 0 |
| 3.3.1.4 | 1 | `net.ipv4.conf.all.send_redirects`, and on every interface | `0` | 0 |
| 3.3.1.5 | 1 | `net.ipv4.conf.default.send_redirects` | `0` | 0 |
| 3.3.1.6 | 1 | `net.ipv4.icmp_ignore_bogus_error_responses` | `1` | 1 |
| 3.3.1.7 | 1 | `net.ipv4.icmp_echo_ignore_broadcasts` | `1` | 1 |
| 3.3.1.8 | 1 | `net.ipv4.conf.all.accept_redirects`, and on every interface | `0` | 0 |
| 3.3.1.9 | 1 | `net.ipv4.conf.default.accept_redirects` | `0` | 0 |
| 3.3.1.10 | 1 | `net.ipv4.conf.all.secure_redirects`, and on every interface | `0` | 0 |
| 3.3.1.11 | 1 | `net.ipv4.conf.default.secure_redirects` | `0` | 0 |
| 3.3.1.12 | 1 | `net.ipv4.conf.all.rp_filter` | `1` | 1 |
| 3.3.1.13 | 1 | `net.ipv4.conf.default.rp_filter` | `1` | 1 |
| 3.3.1.14 | 1 | `net.ipv4.conf.all.accept_source_route` | `0` | 0 |
| 3.3.1.15 | 1 | `net.ipv4.conf.default.accept_source_route` | `0` | 0 |
| 3.3.1.16 | 1 | `net.ipv4.conf.all.log_martians` | `1` | 1 |
| 3.3.1.17 | 1 | `net.ipv4.conf.default.log_martians` | `1` | 1 |
| 3.3.1.18 | 1 | `net.ipv4.tcp_syncookies` | `1` | 1 |
| 3.3.2.1 | 1 | `net.ipv6.conf.all.forwarding` | `0` | 0 |
| 3.3.2.2 | 1 | `net.ipv6.conf.default.forwarding` | `0` | 0 |
| 3.3.2.3 | 1 | `net.ipv6.conf.all.accept_redirects`, and on every interface | `0` | 0 |
| 3.3.2.4 | 1 | `net.ipv6.conf.default.accept_redirects` | `0` | 0 |
| 3.3.2.5 | 1 | `net.ipv6.conf.all.accept_source_route` | `0` | 0 |
| 3.3.2.6 | 1 | `net.ipv6.conf.default.accept_source_route` | `0` | 0 |
| 3.3.2.7 | 1 | `net.ipv6.conf.all.accept_ra`, and on every interface | `0` | 0 |
| 3.3.2.8 | 1 | `net.ipv6.conf.default.accept_ra` | `0` | 0 |
<!-- end: cis -->

What it means for HAProxy and the node:

- **No router advertisements**: IPv6 addresses and their gateway are
  static, set in the [network configuration](../network-configuration.md) -
  no SLAAC address, no default route learned from a router.
- **No IP forwarding**: HAProxy's full transparent proxy mode (`source ...
  usesrc`, which needs the servers to route back through the node) isn't
  possible.
- **No core dumps after HAProxy drops its privileges**
  (`fs.suid_dumpable`): `set-dumpable` can't bring them back.
- **SYN cookies always on**: `net.ipv4.tcp_max_syn_backlog` has no effect,
  so it's read-only - `net.core.somaxconn` bounds the SYN queue.
- **Martian packets are logged** in the kernel's messages, at the
  kernel's own rate limit.

## Testing a change, then keeping it

A change goes **on trial**: the node checks it, writes it, checks the
benchmark still holds, and puts the previous values back by itself
unless you apply it in time.

On the node page, **System › Sysctl**: change values in the table (the
page checks them as you type), then **Test on trial…**. The banner counts
down: **Apply** keeps them, **Cancel now** puts the previous values back.
**Reset** puts one parameter back to Janus's default, **Reset all to
defaults…** every one - on trial too.

With janusctl:

```sh
janusctl system sysctl list                      # values, defaults, the benchmark
janusctl system sysctl get net.core.somaxconn    # one parameter in full
janusctl system sysctl set -no-confirm net.core.somaxconn=30000
janusctl system sysctl confirm                   # keep it - or: cancel
janusctl system sysctl reset -all                # back to the defaults
janusctl system sysctl history                   # who changed what, when
```

Without `-no-confirm`, `set` and `reset` confirm at once over a new
connection - the trial then only protects against a change that would cut
the node off.

- **The trial** lasts 10 minutes by default (`-timeout`, 1 minute to 1
  hour). Testing more while a trial runs adds to it; its end - applied,
  cancelled or run out - concerns everything it holds, and a revert goes
  back to the values from before it began.
- **Applying** must come over a connection opened after the latest test,
  proof that the node still takes new connections.
- **HAProxy reloads** - seamlessly - for `net.core.somaxconn` and the
  `ip_nonlocal_bind` parameters, which it only reads when it opens its
  listeners, and again when they go back (`-no-reload`, or the page's
  checkbox, leaves it alone: they reach it at its next reload).
- **A reboot or a restart of janusd** during a trial ends it: the node
  comes back with the values you applied before.

## How the node keeps them

The values you apply are saved on STATE, in
`/etc/janus/config/sysctl.d/90-haproxy-tuning.conf` - sysctl.d's syntax,
only what differs from Janus's defaults. At boot, Janus's init writes:

1. the baseline: the benchmark's values, Janus's own, then the defaults
   above;
2. the saved file, line by line against the same whitelist and bounds as
   the API - a line that fails is skipped, logged on the console
   (`init: sysctl: sysctl.d/90-haproxy-tuning.conf line N: ...`) and
   recorded in the history;
3. the benchmark's values again, then an audit on the console:
   `init: sysctl: CIS Debian Linux 13 Benchmark v1.0.0 (Level 2 - Server): 33/33 controls compliant`.

All of it happens before janusd starts HAProxy, which opens its listeners
with these values.

SELinux enforces the same whitelist underneath: only these parameters'
files are labelled `sysctl_tunable_t`, the only `/proc/sys` files janusd
may write - the benchmark's stay out of its reach, and only init writes
them. No other daemon - HAProxy, an extension's - writes a kernel
parameter at all. A janusd that doesn't
run a Janus node (`-manage-host` off, as in local-dev) shows the
parameters and changes none.

## The history

Every test, apply, cancel and revert is recorded on STATE - the newest
1000 - with who asked (the certificate's name and roles, and the
Controller user it acted for), when, and each value before and after;
the lines a boot refused too. The node page shows it, as does `janusctl
system sysctl history`. The Controller's own audit log records the
requests it relays as well.
