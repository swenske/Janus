# Packet capture

A Janus node has no shell and no `tcpdump`. `SystemService.PacketCapture`
replaces them: the node captures on one interface, applies a tcpdump-style
filter **in the kernel**, and streams a standard pcap file back over the
usual mTLS gRPC connection. Anything that reads pcap files (tcpdump,
Wireshark, tshark, Zeek…) can open the result.

It requires the `os:admin` role: a capture on a load balancer can contain
plaintext traffic (HAProxy terminates TLS, so everything it sends to your
backends is visible), so read-only `os:reader` certificates are refused.

## Quick start

Capture HTTP traffic to HAProxy for 30 seconds into a file:

```sh
janusctl -endpoint 172.16.1.78:9505 -ca ca.crt -cert admin.crt -key admin.key \
  system pcap -i eth0 -f 'tcp port 80 or tcp port 443' -duration 30s -o capture.pcap
```

Watch live in Wireshark (output goes to stdout when `-o` isn't given):

```sh
janusctl ... system pcap -i eth0 -f 'not port 9505' | wireshark -k -i -
```

Or read it as text with a local tcpdump:

```sh
janusctl ... system pcap -i eth0 -f 'host 10.1.0.50' | tcpdump -nn -r -
```

### Options

| Flag | Default | Meaning |
|---|---|---|
| `-i IFACE` | required | Interface to capture on (`eth0`, `lo`, …). |
| `-f FILTER` | none (everything) | Filter expression, see below. |
| `-duration D` | until Ctrl-C | Stop after `D` (e.g. `30s`, `5m`), rounded up to whole seconds. |
| `-snaplen N` | 65535 | Bytes kept per packet (max 262144). `-snaplen 128` keeps just the headers. |
| `-promisc` | off | Put the interface in promiscuous mode for the capture only. |
| `-o FILE` | `-` (stdout) | Where to write the pcap file. |

`-duration` is enforced by the node itself: it flushes every packet it
captured and then ends the stream cleanly. Stopping with Ctrl-C instead
works too, but the last ~200 ms of packets may be lost with the
cancellation.

Promiscuous mode is tied to the capture: the kernel turns it back off as
soon as the capture ends, even if the client disconnects abruptly.

### From the Janus Controller

Each node's page in the Controller has a **Packet capture** section:
interface, filter (pre-filled with `not port 9505`), duration, optional
snaplen and promiscuous mode, and a **Capture & download .pcap** button.
The Controller relays the capture from the node with its own service
credential and your browser saves the result as
`janus-<node>-<interface>-<UTC time>.pcap`.

The duration is required there (1 to 300 seconds): a browser download
has no Ctrl-C, so every capture is bounded, and the node ends it itself
once every packet is flushed. A bad filter or an unknown interface shows
up as an error next to the button, not as a broken file. The whole
capture is held in the browser's memory until it's saved, so on a busy
node prefer a precise filter or a small snaplen over a long unfiltered
capture.

## Filter language

Filters use tcpdump's syntax, restricted to the subset below. Anything
outside it is **refused with an error**, never approximated: a filter that
silently matched more than you asked for could flood the capture (or
capture the gRPC connection carrying it).

| Kind | Examples |
|---|---|
| Protocols | `ip`, `ip6`, `arp`, `tcp`, `udp`, `icmp`, `icmp6` |
| Hosts | `host 10.1.0.50`, `src host 10.1.0.50`, `dst fd00::5`, `src 10.1.0.50` |
| Networks | `net 10.0.0.0/8`, `src net 192.168.1.0/24`, `net 2001:db8::/32` |
| Ports | `port 443`, `dst port 53`, `tcp port 8080`, `udp src port 123` |
| Port ranges | `portrange 8000-8999`, `tcp dst portrange 1-1023` |
| Operators | `and` / `&&`, `or` / `||`, `not` / `!`, parentheses |

The semantics match tcpdump:

- `and` and `or` have **equal precedence and are evaluated left to right**,
  exactly like tcpdump (not like most programming languages):
  `port 53 or tcp and src 10.0.0.1` means `(port 53 or tcp) and src 10.0.0.1`.
  Use parentheses when in doubt.
- `host`/`net` match IPv4, IPv6 and ARP (sender or target) addresses.
  `ip host X` restricts to IPv4 only.
- `port`/`portrange` match TCP, UDP and SCTP over IPv4 and IPv6. Like
  tcpdump, non-first IPv4 fragments never match a port, and IPv6 packets
  with extension headers aren't looked into.
- No direction means source **or** destination.

Not supported (yet): byte-offset expressions (`tcp[tcpflags] & tcp-syn != 0`),
`ether host`, `vlan`, host names (`host example.com`), service names
(`port https`), `greater`/`less`, `ip proto N`.

The compiler (`internal/pcapfilter`) is tested by running every compiled
program in a BPF virtual machine against a set of crafted frames, and the
same test suite compares each expression's results against real
tcpdump/libpcap when it's installed (`TestAgainstTcpdump`).

## Things to know

- **Exclude your own capture stream.** Capturing on the interface that
  carries the gRPC connection without a filter also captures the capture
  itself - in practice most of the packets. Add `not port 9505` (or a
  more specific filter).
- Only Ethernet interfaces and loopback are supported, the pcap link type
  is always Ethernet (`LINKTYPE_ETHERNET`).
- Timestamps are taken by `janusd` when it reads each packet, with
  microsecond resolution: accurate for troubleshooting, not for
  sub-millisecond latency analysis.
- Packets are batched: data reaches the client every 200 ms or every
  32 KiB, whichever comes first.

## How it works

`janusd` opens an `AF_PACKET` raw socket with protocol 0 (which receives
nothing), attaches the compiled classic-BPF filter, and only then binds it
to the interface with `ETH_P_ALL` - so no packet is ever queued before the
filter applies. Non-matching packets are dropped by the kernel and never
reach `janusd`. The SELinux policy grants `janusd_t` exactly the
`packet_socket` permissions this needs.

The pcap file format is the classic libpcap one (little-endian, version
2.4, microsecond timestamps), split across the stream's `Data` messages:
concatenating their bytes gives the file.

With other gRPC clients, call `janus.v1alpha1.SystemService/PacketCapture`
with `interface`, `bpf_filter`, `promiscuous`, `snap_len` and
`duration_seconds`, and write every received `Data.bytes` to a file in order.
