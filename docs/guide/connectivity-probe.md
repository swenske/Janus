# Connectivity test

The network as a node sees it - its routes, its resolver, its addresses:
a DNS lookup, a TCP connection, a TLS handshake, an HTTP request, ICMP
echo requests and a traceroute, run by the node and each step timed. A
node has no shell, no `ping` and no `curl`: this is what answers "why is
this backend down?" from where HAProxy stands. It takes the operator
role.

## From the Controller

**Tools › Connectivity test** on a node's page: pick a kind, type a
target, **Run**. The steps come as the node takes them - the lookup, the
connection (and from which of the node's addresses), the handshake with
the certificate chain and whether it's trusted, the HTTP answer with its
headers and the first KiB of its body; ping's replies and traceroute's
hops in a table. **Options** sets the source address, a DNS server, a
CA, the SNI, the HTTP method and headers, the echo requests' count, size
and DF bit.

## From janusctl

```sh
janusctl system probe http https://backend.example:8443/health
janusctl system probe http -connect-to 10.0.0.10 -ca-file ca.pem -v https://app.example/health
janusctl system probe tls -sni www.example.com 127.0.0.1:443
janusctl system probe dns -server 10.0.0.53 -type SRV _api._tcp.service.consul
janusctl system probe tcp -source 192.0.2.10 10.0.0.10:5432
janusctl system probe ping -count 10 10.0.0.1
janusctl system probe ping -df -size 1472 10.0.0.1
janusctl system probe traceroute backend.example
```

Each step prints as it comes, `ok` or `FAIL`, how long it took and what
it found; the exit status is 1 when the probe failed. `-all` runs it
from every node of a context, each line prefixed with the node's name.

## The kinds

| Kind | Target | What it tells |
|---|---|---|
| `dns` | a name | its records: A and AAAA, or the type asked (`CNAME`, `SRV`, `TXT`, `MX`, `NS`, `PTR`), from the node's resolver or a server given |
| `tcp` | `host:port` | whether a connection opens, how fast, from which address |
| `tls` | `host[:port]`, 443 by default | the handshake: version, cipher, ALPN, the chain - and whether it's trusted, against the system's roots or a CA given; an OCSP response stapled |
| `http` | an `http://` or `https://` URL | the answer: status, protocol (HTTP/2 when negotiated), headers, the body's size and first KiB, the time to its first byte; a redirect is shown, not followed |
| `ping` | a host | echo replies, their TTL and round trip; with the DF bit, whether a size gets through - and the MTU a router or the node's route says when it doesn't |
| `traceroute` | a host | the routers on the way, three probes per hop, named when they resolve |

`-connect-to` (**Connect to**) makes `tls` and `http` connect to an
address of your choosing while the target still gives the SNI and the
`Host` header - curl's `--resolve`: one backend server behind a name,
or which certificate HAProxy serves for a name on the node itself
(`-connect-to 127.0.0.1`).

`-source` sends from one of the node's addresses - a VRRP virtual
address, a BGP anycast address -, refused when the node doesn't have it
at that moment; the DNS queries a name needs leave from it too. The
answers come back the way the network routes that address: to an
anycast address, to the node the routers send it to - test from that
one.

## Limits

A probe is a test, not a tool to load a network:

- 60 seconds at most, each step 10 seconds at most (5 by default; ping
  2, a traceroute hop 1);
- 20 echo requests, 200 ms apart at least; 9216 bytes of payload; 30
  hops;
- one HTTP request, GET, HEAD or OPTIONS - no body sent -, 16 headers,
  1 MiB of the answer read;
- 4 probes at once per node.

Every probe is in the node's events (`probe.run`, with who ran it) and
its audit log.

## How it works

janusd runs it (`internal/probe`): Go's own resolver, dialer, TLS and
HTTP client over a connection it opened itself - so each step is timed
apart -, and a raw ICMP socket for ping and traceroute (janusd's
`CAP_NET_RAW`, the SELinux rules `janusd_t` has for it measured on an
enforcing node). It never runs a program.
