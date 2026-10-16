# Log forwarding

A node sends its logs to your log servers - syslog (RFC 5424 or 3164,
over UDP, TCP or TLS) and Elasticsearch - itself: janusd's log, HAProxy's
messages and access logs, the extensions', the kernel's, the event log.
What it keeps on disk is sent at least once, even across a server outage
or a reboot.

There's no syslog daemon to configure on a node, and no agent to install:
janusd sends the logs (`internal/logship`), set through the API - the
node's page, `janusctl system log-forwarding`, the
`SystemService.LogForwarding*` calls
([API reference](../private-cloud/api-reference.md)) - or for a fleet by
the Controller, which sends its own log and audit too
([below](#from-the-controller)).

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
| `audit` | Every call that changed something or read what a reader may not - who, from where, how it ended - and every call refused ([who did what](../private-cloud/observability.md#logs)) | STATE |

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
janusctl -n lb1 system log-forwarding status            # each target: sent, dropped, waiting, the last error
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
| `facility` | the services' lines: `local0` (the default) to `local7`, `daemon`, `user`, `auth`. The kernel's messages are `kern`, the audit's `authpriv` |

A message carries the node's hostname, the service as its app name
(`janusd`, `haproxy`, `keepalived`, `kernel`...), the source as its
message ID (RFC 5424), and the severity the line says - HAProxy's
`[WARNING]` is a warning, Consul's `[ERROR]` an error, an `api:` line a
notice, the rest info.

A **Synology** NAS receives them in Log Center › Log Receiving: a rule in
IETF format (`rfc5424`) or BSD (`rfc3164`), over UDP, or TCP with SSL -
`transport` `tls`, the port the rule says, and the certificate Log Center
exports as `tls.ca_pem` (or its fingerprint).

### Elasticsearch

```json title="examples/logforward/elasticsearch.json"
{
  "targets": [
    {
      "name": "elastic",
      "sources": ["janusd", "haproxy", "access", "extensions", "kernel", "events", "audit"],
      "elasticsearch": {
        "url": "https://elastic.example.com:9200",
        "namespace": "prod",
        "apiKey": "VnVhQ2ZHY0JDZGJrUW0tZTVhT3g6dWkybHAyYXhUTm1zeWFrdzl0dk5udw==",
        "gzip": true
      },
      "tls": {
        "fingerprint": "93:BA:4A:A9:69:30:C1:B5:0D:DF:89:AD:FC:CA:5A:88:5D:D4:14:62:B9:2A:0F:1F:DF:FB:D9:60:FF:22:56:D9"
      }
    }
  ]
}
```

Each record is a document of the data stream
`logs-janus.<source>-<namespace>` - `logs-janus.janusd-prod`,
`logs-janus.access-prod`... - which Elasticsearch 8 and 9 make on their
own (their built-in `logs-*-*` template). On the cluster, all it takes
is an API key that may create documents there - in Kibana's Dev Tools:

```text
POST /_security/api_key
{
  "name": "janus-nodes",
  "role_descriptors": {
    "janus": {"index": [{"names": ["logs-janus*"], "privileges": ["create_doc", "auto_configure"]}]}
  }
}
```

Its `encoded` value is the target's `api_key` (`id:key` works too); a
user and password instead: `username` and `password`. A cluster whose
HTTPS certificate Elasticsearch made at its first start is trusted by the
fingerprint it printed then ("HTTP CA certificate SHA-256 fingerprint"),
or `openssl x509 -noout -fingerprint -sha256 -in config/certs/http_ca.crt`.
`gzip` sends a fifth of the bytes for some CPU.

The documents have ECS fields: `@timestamp`, `message`, `log.level`,
`host.name` and `host.id` (the node's CA SHA-256: the console's `pki:
this node's CA` line), `service.name`, `event.dataset`, an event's
`event.action` (its type) and `janus.payload`, an audit entry's
`user.name`, `user.roles`, `source.ip`, `event.action` (the method),
`event.outcome` and `event.duration`, `labels.*`, `janus.slot` and
`janus.schematic`. In Kibana, a data view on `logs-janus*` shows them
all.

A batch sent again - a timeout, the cluster busy (429) - is stored once:
each document's ID is the node's and the record's, the copies answered
409. A document the cluster refuses for good (its mapping) is dropped,
said once a minute in janusd's log.

HAProxy's access logs formatted as JSON become fields - `haproxy.status`,
`haproxy.path`... - with no ingest pipeline:

```haproxy
frontend web
    bind :80
    log-format "%{+json}o %(client_ip)ci %(method)HM %(path)HP %(status)ST %(bytes)B %(duration)Ta"
```

### Trusting the server

A `tls` target checks the server's certificate:

- by default, against the node's trust store - the usual public CAs;
- `tls.ca_pem`: against your CA (PEM), the one that signed the server's
  certificate;
- `tls.fingerprint`: the SHA-256 fingerprint of the server's certificate,
  or of a CA in the chain it presents - colons or not; a wrong one is
  refused with the server's own. Many servers (Logstash, rsyslog) present
  their own certificate alone: pin that one, or give the CA as
  `tls.ca_pem`;
- `tls.server_name`: the name the certificate carries, when it isn't the
  address's;
- `tls.client_cert_pem` and `tls.client_key_pem`: a client certificate,
  for a server that asks for one.

The secrets - an API key, a password, a client key - are kept on STATE,
readable by janusd only:
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

### What it costs

Measured on the worst case: a 2-vCPU node of an older Xeon (E3-1220
v3) where HAProxy answers every request itself, saturated by four
clients for a minute - so a log line costs as much as the request it
logs. Behind it, a single-CPU Logstash and a 2-CPU Elasticsearch.

| HAProxy's access logs | Requests/s | janusd's CPU | Lines HAProxy dropped | What the server kept |
|---|---|---|---|---|
| None | 124 900 | - | - | - |
| `log stdout`, sent nowhere | 64 600 | 0.27 | 8.6 % | - |
| `log stdout`, janusd to syslog over UDP | 57 900 | 0.62 | 4.7 % | 3.6 % (Logstash full) |
| `log stdout`, janusd to Elasticsearch | 60 200 | 0.50 | 11 % | ~6 000 a second, the rest dropped by janusd, counted |
| The same, `access_logs_on_disk: always` | 64 000 | 0.48 | 8.8 % | all of them, 7 minutes after the minute (3.5 million) |
| HAProxy's own `log` over UDP | 72 200 | - | 0 | 5.4 % (Logstash full) |
| HAProxy's own `log` through a `ring`, over TLS | 98 600 | - | 0 | 5.2 % (the ring wrote over the rest) |

What it says:

- **Logging every request is HAProxy's own biggest cost**, whichever
  way: it formats and writes a line per request - half the requests/s
  of a node that logs nothing, here. With backends behind it, the
  share is far smaller. Fewer lines cost less: `option
  dontlog-normal` logs only the errors, `log ... sample 1:100` one
  request in a hundred.
- **janusd** reads them for about 5 µs of CPU a line on that Xeon,
  sends them to syslog for about 6 µs more, to Elasticsearch in
  compressed batches.
- **The server caps what arrives**: a server that can't keep up loses
  UDP datagrams on its side; janusd drops what an Elasticsearch
  cluster can't take - counted (`dropped`). HAProxy never slows down
  for them.
- **`access_logs_on_disk: always`** writes every line to the LOGS
  partition - 10 MB/s here, 860 GB a day - and sends them from there at
  the server's pace: Elasticsearch had every one of the minute's 3.5
  million lines 7 minutes after it. But the partition (512 MiB) holds
  about a minute of such traffic - a longer burst loses the oldest,
  counted - and it wears the disk. Keep it for a moderate traffic you
  can't afford to lose.
- **HAProxy drops lines itself** rather than wait: when its stdout is
  full (janusd behind), and when two of its threads write to its
  stdout at once - it gives up after a few tries: about 1 % of the
  lines at 50 000 requests/s on a recent CPU, 8 % on that Xeon. Its
  own `log` to a UDP server shares no pipe: none dropped. `janusctl
  haproxy show-info` says how many it dropped (`Dropped logs`), and the
  metrics too (`janus_haproxy_dropped_logs_total`). A `ring` costs
  HAProxy the least - a copy in memory - but writes over its oldest
  lines, uncounted, when its server can't keep up.

## What's guaranteed

- **From disk, at least once**: what the LOGS partition and STATE keep -
  janusd's log, HAProxy's messages, the extensions', the kernel's, the
  event log, the audit - each target reads behind a cursor saved as it
  goes. A
  server down, then back: what was logged meanwhile goes then, in order.
  A reboot, janusd restarted: each target goes on from its cursor. As
  long as the partition still holds it: the oldest records go when it's
  full - a target that falls that far behind counts the ones it never
  read as dropped, whatever their source.
- **From memory**: HAProxy's access logs (unless `access_logs_on_disk` is
  `always`), and everything on a node installed before the LOGS
  partition existed - a queue per target, the oldest dropped when it's
  full, counted (`dropped`, `janus_logship_records_dropped_total`).
  What's logged as janusd starts, before it reads its configuration -
  the kernel's boot, its first lines - waits for it (8 192 lines), then
  goes under the node's hostname.
- **UDP** has no acknowledgement: a server down loses what's sent. Over
  **TCP and TLS**, syslog has none either - a server that goes away in
  the middle of a batch loses it; one that went away between two is
  noticed, and the batch sent again once it's back. **Elasticsearch**
  confirms each document: a batch is sent again until it's stored, and
  stored once.
- Nothing waits for a target: a line is queued, written to disk or
  dropped - HAProxy, janusd and the API never slow down for a log
  server.

## From the Controller

The Controller's **Log forwarding** page (admins) sets two things:

- **The Controller's own logs**: its log and its audit - every change
  made through it, every sign-in - to its own targets, its audit from
  where it stopped after a restart.
- **Fleet profiles**: each a label selector (`team=web,env=prod`; empty:
  every node) and the log targets of the nodes it picks. A node takes the
  first profile whose labels it has, and the Controller keeps it applied:
  at once when a profile or a node's labels change, every ten minutes
  otherwise - every minute while a node can't take it yet (an older
  release being updated, a node away) - a change made on the node is
  undone, a profile that no longer picks a node is taken off it. A node no profile picks keeps its
  own configuration, set on its page (Logs › Forwarding) or with
  `janusctl`. The page lists each node's state: `applied`, `unmanaged`,
  `removed`, `unsupported` (a release without log forwarding).

A node's page shows what the Controller set as read-only, and its
configuration says by whom (`managed_by`: the Controller and the
profile). The secrets - API keys, passwords, client keys - are sealed
with the Controller's master key and sent to the nodes the profile picks;
neither the page nor the API gives them back. A target is tested as the
page shows it, saved or not, from the Controller (its own) or from one
of the profile's nodes; what's added, changed or removed applies once
the page is saved - it says so until then.

The Controller in a container keeps writing its log on its standard
error too: `docker logs` and Docker's own logging drivers still see it.

## Watching it

`janusctl system log-forwarding status` - each target's state (`ok`,
`failing`), records sent, dropped, waiting, the last error. A target
failing for 30 seconds is said so in the event log
(`logship.failing`, then `logship.recovered`) and janusd's log, and in
the [metrics](../metrics.md): `janus_logship_target_up`,
`janus_logship_records_dropped_total`; the Grafana dashboard's *Kernel
and STATE* tile counts the targets failing and the records lost.
