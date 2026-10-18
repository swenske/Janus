# Stick tables, sessions and errors

What the running HAProxy holds - its stick tables' entries, the
sessions it's processing, the last requests and responses it refused as
invalid - read on a node's page in the Controller or with janusctl,
without a shell; and which map entry or ACL pattern a value matches,
with HAProxy's own matching.

Everything here is read from the running process over its runtime API
(`show table`, `show sess`, `show errors`, `get map`, `get acl`) and
never kept: a reload starts a new process with empty tables and no
captured errors.

## Who may do what

| | Reader | Operator |
|---|---|---|
| Stick tables: list, entries, filters | ✅ | ✅ |
| Stick tables: clear an entry, the matching ones, all | - | ✅ |
| Sessions: list | ✅ | ✅ |
| Sessions: end one | - | ✅ |
| Errors: the refused requests and responses | - | ✅ |
| Map and ACL lookups | ✅ | ✅ |

Errors take an operator for the same reason as service logs: they're
the bytes clients and servers sent. Their credential headers' values are
masked anyway (see [Errors](#errors)).

## Stick tables

A stick table counts per key - a client's address, a header's value -
what the configuration tells it to store: request rates, connections,
general purpose counters. A rate limit is the usual reason for one:

```haproxy title="examples/haproxy/rate-limit.cfg"
# Rate limiting on a Janus node: each client's request rate over 10
# seconds in a stick table, 429 above 100 requests, 403 for a client
# flagged by hand (gpc0). HAProxy › Stick tables shows who is counted
# and clears an entry to let a client through
# (docs/guide/haproxy-runtime.md). Checked by the node's own HAProxy
# (make examples-test).
global
    log stdout format raw local0
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
    chroot /var/empty
    uid 1000
    gid 1000
    fd-hard-limit 65536

defaults
    mode http
    log global
    option httplog
    timeout connect 5s
    timeout client 30s
    timeout server 30s

frontend web
    bind :80
    stick-table type ip size 100k expire 10m store http_req_rate(10s),gpc0
    http-request track-sc0 src
    http-request deny deny_status 403 if { sc_get_gpc0(0) gt 0 }
    http-request deny deny_status 429 if { sc_http_req_rate(0) gt 100 }
    default_backend app

backend app
    balance roundrobin
    option httpchk GET /healthz
    default-server check inter 2s fall 3 rise 2
    server app1 10.0.10.11:8080
    server app2 10.0.10.12:8080
```

**HAProxy › Stick tables** lists the tables - key type, entries used
over size - and a table's entries with what each stores, a rate with its
period (`http_req_rate /10s`). A key shows one entry; up to four filters
(`http_req_rate > 100`, `gpc0 ≥ 1`) show the entries whose data match -
HAProxy's own `data.<type> <op> <value>`. An operator clears an entry,
the entries the filters match, or the whole table: HAProxy forgets them,
their counters start again from zero - a client the rate limit blocked
is let through until it reaches the limit again.

```sh
janusctl haproxy tables
janusctl haproxy table web -filter http_req_rate:gt:100
janusctl haproxy table-clear web -key 203.0.113.7
```

An entry a session is still tracking ("in use") stays until the session
ends. 500 entries are shown unless asked (`-limit`, 5000 at most): past
that, the page says more match and filters narrow them.

## Sessions

**HAProxy › Sessions** lists what HAProxy is processing: a request it
hasn't answered yet, a TCP connection it relays - the client, the
frontend, the backend and server once chosen, its age and its next
timeout. An idle keep-alive connection, or a request still arriving, is
no session yet. An operator ends one: HAProxy closes its client and
server connections, the client sees the connection cut - a stuck
WebSocket, a client holding a slot.

```sh
janusctl haproxy sessions -backend app
janusctl haproxy session-kill 0x7702b96d66a0
```

The runtime API's own session (janusd asking) isn't listed.

## Errors

When HAProxy refuses a request or a response as invalid - it answers
400 or 502 "protocol error" - it keeps the bytes it read, the last of
each proxy and direction. **HAProxy › Errors** shows them, the refused
byte marked, with HAProxy's buffer and parser state below:

```sh
janusctl haproxy errors -proxy web -direction request
```

The values of `Authorization`, `Proxy-Authorization`, `Cookie`,
`Set-Cookie`, `X-Api-Key` and `X-Auth-Token` are replaced by
`<redacted>`. The rest is shown as sent: a secret in a URL or another
header is in it.

## Testing a map or an ACL

**HAProxy › Maps & ACLs** tests a value against a map or a file-backed
ACL list: which entry or pattern it matches, with the method the
configuration reads it with - `dom`, `beg`, `reg`, `ip`... -, the way a
request's value would. A map's own entries only show exact keys:
`shop.example.org` matches `.example.org` in a `map_dom`.

```sh
janusctl haproxy map-lookup /etc/haproxy/files/hosts.map shop.example.org
janusctl haproxy acl-lookup /etc/haproxy/files/blocked.acl 203.0.113.7
```

Both exit with status 1 when nothing matches.
