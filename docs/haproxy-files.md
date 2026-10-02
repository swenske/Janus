# HAProxy's files

A `haproxy.cfg` often references more than itself: error pages
(`errorfile`), maps and ACL lists (`map(...)`, `acl ... -f`), Lua scripts,
certificates the [letsencrypt extension](letsencrypt.md) doesn't obtain
(an internal CA's, a commercial one). A Janus node has no shell to copy
them with: they're managed through the API, `janusctl` and the
Controller's **HAProxy › Files** tab, and kept on the node (its STATE
partition) across reboots and updates.

Each file is `/etc/haproxy/files/<name>` for `haproxy.cfg`; a name is
letters, digits, `.`, `-` and `_`, with at most one subdirectory
(`errors/503.http`, `certs/admin.pem`). Up to 256 files of 1 MiB each.

```
defaults
    errorfile 503 /etc/haproxy/files/errors/503.http

frontend admin
    bind :8443 ssl crt /etc/haproxy/files/certs/admin.pem
```

A subdirectory can serve as a `crt` directory (`crt
/etc/haproxy/files/certs/`) - keep only certificates in it then.

## Safe by construction

- **A change that would break `haproxy.cfg` is refused**: the node checks
  the current configuration with the new file (or without the removed
  one) before keeping the change, and gives HAProxy's own messages back.
  The next reload - or the next boot - can't fail because of a file.
- **A private key is never read back**: a file holding one (a
  certificate's PEM bundle) is listed as such, can be replaced or
  removed, but its content never leaves the node.
- Writing a file doesn't change the running HAProxy by itself: ask for a
  reload with it (`-reload`, or the checkbox in the Controller), or reload
  later.

## janusctl

```sh
janusctl haproxy files                                  # name, size, date, "private key"
janusctl haproxy file-put -reload errors/503.http ./503.http
janusctl haproxy file-put -reload certs/admin.pem ./admin.pem   # chain and key, PEM
janusctl haproxy file-get errors/503.http               # never a private key
janusctl haproxy file-delete -reload errors/503.http    # refused while haproxy.cfg needs it
```

To add a file and the configuration that uses it, put the file first,
then apply the configuration. To stop using one, apply the configuration
without it first, then remove it.

## Certificates: which mechanism

- **Let's Encrypt or another ACME CA**: the [letsencrypt
  extension](letsencrypt.md) obtains and renews them, in
  `/etc/haproxy/acme/`.
- **A certificate you renew yourself** (an internal CA's, a commercial
  one): a file here. Replacing it takes a reload.
- **A certificate changed often, at runtime** (by a tool, per tenant):
  `janusctl haproxy cert-upload`, bound into a `crt-list`, swapped without
  a reload.
