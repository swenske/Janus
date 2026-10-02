# Bringing your haproxy.cfg to a Janus node

A Janus node runs a stock HAProxy (3.4, OpenSSL, the Prometheus exporter
built in) under `janusd`: an existing configuration mostly works as is.
What differs is the machine around it - no syslog, no users, no shell to
copy files with. This is what to change, then apply it with `janusctl
haproxy apply-config FILE` or the Controller's **HAProxy › Configuration**.

## The global section

| On a distribution | On a Janus node | Why |
|---|---|---|
| `log /dev/log local0` | `log stdout format raw local0` | No syslog daemon. janusd keeps HAProxy's output: `janusctl system logs haproxy`, the Controller's logs page |
| `user haproxy` / `group haproxy` | `uid 1000` / `gid 1000` | No `/etc/passwd` to resolve names against |
| `chroot /var/lib/haproxy` | `chroot /var/empty` | The empty directory janusd prepares |
| `daemon` | (remove it) | janusd supervises HAProxy in the foreground |
| `stats socket /run/haproxy/admin.sock ...` | `stats socket /run/janus/haproxy-admin.sock mode 660 level admin` | janusd's own runtime API goes through this socket - it must be there, at this path, with `level admin` |
| `crt-base`, `ca-base` | (remove, use full paths) | Your files live in `/etc/haproxy/files/` |
| `ssl-dh-param-file` | (remove, or upload the file) | Only DHE ciphers use it; ECDHE ones don't |

## Files the configuration references

Error pages, maps, ACL lists, Lua, certificates you renew yourself: upload
them as [HAProxy's files](haproxy-files.md) and reference
`/etc/haproxy/files/<name>`. Let's Encrypt certificates: the
[letsencrypt extension](letsencrypt.md) obtains them, in
`/etc/haproxy/acme/`; its HTTP-01 rule uses `${JANUS_ACME_THUMBPRINT}`
rather than a thumbprint written in the configuration.

Backend server names (`server app app.example.net:80`) are resolved when
HAProxy starts, through the node's resolvers (its network configuration,
or DHCP's).

## Connections and memory

HAProxy may use up to 524288 open files - the limit systemd gives a
service on a mainstream distribution. Without `maxconn` or `ulimit-n` in
the global section, it sizes itself from that: about 262000 connections,
and around 50 MB of memory for the tables, used or not. On a small node,
`fd-hard-limit 65536` (32000 connections, about 20 MB) or an explicit
`maxconn` keeps it lighter. A `ulimit-n` or `maxconn` above that limit is
refused: `haproxy -c` accepts it, but HAProxy wouldn't start - the node
reports HAProxy's reason and keeps the configuration and the process it
had.

## What the node checks

Every configuration goes through `haproxy -c` first, then through a real
start: a configuration HAProxy refuses at either step changes nothing.
Reloads are seamless (`-sf`), and the configuration is kept on the node
across reboots and updates.
