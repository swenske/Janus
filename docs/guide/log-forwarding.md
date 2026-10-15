# Log forwarding

A node sends its logs to your log servers - syslog (RFC 5424 or 3164,
over UDP, TCP or TLS) - itself: janusd's log, HAProxy's messages and
access logs, the extensions', the kernel's, the event log. What it keeps
on disk is sent at least once, even across a server outage or a reboot.

There's no syslog daemon to configure on a node, and no agent to install:
janusd sends the logs (`internal/logship`), set through the API -
`janusctl system log-forwarding`, or the `SystemService.LogForwarding*`
calls ([API reference](../private-cloud/api-reference.md)).

## What's sent

Each target - a log server - takes the sources it names:

| Source | What | Kept on the node |
|---|---|---|
| `janusd` | janusd's log: what the node does, every API call that changes something (`api: <Method>: <caller>`) | LOGS partition |
| `haproxy` | HAProxy's own messages: `[NOTICE]`, `[WARNING]` (a server down), `[ALERT]` | LOGS partition |
| `access` | HAProxy's access logs - its global section's `log stdout` | memory, or the LOGS partition with `access_logs_on_disk` |
| `extensions` | The extensions' services: keepalived, BIRD, Consul, node_exporter, the guest agent, Let's Encrypt | LOGS partition |
| `kernel` | The kernel's messages: AVC denials, OOM kills, devices | LOGS partition |
| `events` | The [event log](../private-cloud/observability.md#logs): configuration applied, reboots, updates, failovers... | STATE |

A target that names no source takes them all but `access`: HAProxy logs
a line per request, thousands a second under load - ask for them.

## Setting it up

A configuration lists the targets:

```json title="examples/logforward/syslog.json"
{
  "labels": {"env": "prod"},
  "targets": [
    {
      "name": "nas",
      "syslog": {"address": "logs.example.com:6514", "transport": "tls"},
      "tls": {
        "fingerprint": "3F:1C:9A:52:07:E4:B8:6D:21:F0:A3:94:5C:7E:18:B2:60:D9:4F:A7:13:8E:C5:02:6B:F1:39:DA:84:57:2E:C0"
      }
    },
    {
      "name": "access-logs",
      "sources": ["access"],
      "syslog": {"address": "10.0.0.20", "transport": "udp", "format": "rfc3164", "facility": "local1"}
    }
  ]
}
```

```sh
janusctl -n lb1 system log-forwarding set syslog.json   # applied at once, kept on STATE
janusctl -n lb1 system log-forwarding                   # each target: sent, dropped, waiting, the last error
janusctl -n lb1 system log-forwarding test nas          # one record now - or why it can't
janusctl -n lb1 system log-forwarding get               # the configuration, without its secrets
```

A new target starts with what's logged from then on. A target's name is
where it keeps its place: rename it, and it starts again from now.

### Syslog

| Field | Values |
|---|---|
| `address` | `host[:port]` - 514 by default, 6514 with `tls` |
| `transport` | `udp` (the default), `tcp`, `tls` |
| `format` | `rfc5424` (the default) or `rfc3164` (BSD) |
| `framing` | over `tcp` and `tls`: `octet-counting` (RFC 6587, the default with `rfc5424`) or `lf` (a message a line, the default with `rfc3164`) |
| `facility` | the services' lines: `local0` (the default) to `local7`, `daemon`, `user`, `auth`. The kernel's messages are `kern` |

A message carries the node's hostname, the service as its app name
(`janusd`, `haproxy`, `keepalived`, `kernel`...), the source as its
message ID (RFC 5424), and the severity the line says - HAProxy's
`[WARNING]` is a warning, Consul's `[ERROR]` an error, an `api:` line a
notice, the rest info.

A **Synology** NAS receives them in Log Center › Log Receiving: a rule in
IETF format (`rfc5424`) or BSD (`rfc3164`), over UDP, or TCP with SSL -
`transport` `tls`, the port the rule says, and the certificate Log Center
exports as `tls.ca_pem` (or its fingerprint).

### Trusting the server

A `tls` target checks the server's certificate:

- by default, against the node's trust store - the usual public CAs;
- `tls.ca_pem`: against your CA (PEM), the one that signed the server's
  certificate;
- `tls.fingerprint`: the SHA-256 fingerprint of the server's certificate,
  or of a CA in the chain it presents - colons or not;
- `tls.server_name`: the name the certificate carries, when it isn't the
  address's;
- `tls.client_cert_pem` and `tls.client_key_pem`: a client certificate,
  for a server that asks for one.

The secrets - a client key - are kept on STATE, readable by janusd only:
they never come back through the API (`log-forwarding get` shows a
target without them, and `status` names those it has), nor in a
Controller's backups. A configuration sent without a target's secret
keeps the one saved under its name.

## Two ways for HAProxy's access logs

**Through janusd** (the `access` source): HAProxy's global section logs
to its stdout - `log stdout format raw local0`, what Janus's examples
do - and janusd sends each line to the targets that take `access`, with
the node's metadata. A target that can't keep up loses them - counted -
from its queue in memory (20 000 lines), unless `access_logs_on_disk` is
`always`: then they're kept on the LOGS partition too and sent at least
once, at the cost of writing every request to the disk.

**From HAProxy itself**: its own `log` lines, without janusd -

```haproxy title="examples/haproxy/log-direct.cfg"
# HAProxy sending its own access logs, without janusd: every request
# straight to a syslog server over UDP, and through a ring to another
# one over TLS. docs/guide/log-forwarding.md says when to prefer this to
# janusd's log forwarding (no Elasticsearch, no Janus metadata, nothing
# kept on the node; the least CPU under heavy load).
global
    # UDP: one datagram a request, lost when the server is down.
    log 192.0.2.10:514 local0
    # TLS: kept in the ring while the server is away (its size).
    log ring@remote-logs local0
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000

ring remote-logs
    format rfc5424
    size 1048576
    timeout connect 5s
    timeout server 10s
    # The server's certificate checked against the system's CAs - or a
    # CA of yours put in HAProxy's files (janusctl haproxy file-put
    # certs/logs-ca.pem ca.pem): ca-file /etc/haproxy/files/certs/logs-ca.pem
    server logs 192.0.2.11:6514 ssl verify required ca-file /etc/ssl/certs/ca-certificates.crt verifyhost logs.example.com log-proto octet-count

defaults
    mode http
    log global
    option httplog
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend web
    bind :80
    http-request return status 200 content-type text/plain string "ok\n"
```

| | Through janusd | From HAProxy |
|---|---|---|
| Elasticsearch | yes | no |
| The node's metadata, labels | yes | HAProxy's hostname only |
| A server down | a queue in memory per target, or the LOGS partition | UDP: lost; a ring: its size |
| CPU | HAProxy writes to a pipe, janusd reads, formats and sends | HAProxy alone |
| Also in `janusctl system logs haproxy` | yes | only with `log stdout` too |

Under heavy load, both cost something: HAProxy formats a line per
request either way. Fewer lines cost less - `option dontlog-normal` logs
only the errors, `log ... sample 1:100` one request in a hundred. A
line HAProxy can't write at once (janusd behind) is dropped by HAProxy,
never waited for: its `DroppedLogs` counter (`janusctl haproxy
show-info`) says how many.

## What's guaranteed

- **From disk, at least once**: what the LOGS partition and STATE keep -
  janusd's log, HAProxy's messages, the extensions', the kernel's, the
  event log - each target reads behind a cursor saved as it goes. A
  server down, then back: what was logged meanwhile goes then, in order.
  A reboot, janusd restarted: each target goes on from its cursor. As
  long as the partition still holds it: the oldest boots go when it's
  full.
- **From memory**: HAProxy's access logs (unless `access_logs_on_disk` is
  `always`), and everything on a node installed before the LOGS
  partition existed - a queue per target, the oldest dropped when it's
  full, counted (`dropped`, `janus_logship_records_dropped_total`).
- **UDP** has no acknowledgement: a server down loses what's sent. Over
  **TCP and TLS**, syslog has none either - a server that goes away in
  the middle of a batch loses it; one that went away between two is
  noticed, and the batch sent again once it's back.
- Nothing waits for a target: a line is queued, written to disk or
  dropped - HAProxy, janusd and the API never slow down for a log
  server.

## Watching it

`janusctl system log-forwarding` - each target's state (`ok`,
`failing`), records sent, dropped, waiting, the last error. A target
failing for 30 seconds is said so in the event log
(`logship.failing`, then `logship.recovered`) and janusd's log, and in
the [metrics](../metrics.md): `janus_logship_target_up`,
`janus_logship_records_dropped_total`; the Grafana dashboard's *Kernel
and STATE* tile counts the targets failing and the records lost.
