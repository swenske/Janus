# Consul through the Controller

The Controller can manage the Consul agent of every node with the
[consul extension](../consul.md): it makes each node's own ACL token in
your Consul cluster, gives the node what makes it a client of that
cluster - the datacenter, the servers, the gossip key, the servers' CA -
and the node registers its own services there: janusd, its exporters,
each HAProxy frontend, and one instance per URL its maps and Host ACLs
route - for Prometheus to find and probe.

You don't type the datacenter, the servers or the gossip key: the
Controller reads them from Consul. Your own configuration of a node's
agent (`janusctl network consul apply`, [consul.md](../consul.md)) stays
as it is: the Controller's part is a layer of its own, loaded after
yours.

## Before you start

The Consul cluster is yours - Janus nodes are only its clients. Set up as
Consul recommends for production:

- ACLs on, denying by default (`acl { enabled = true, default_policy =
  "deny" }`);
- gossip encryption (`encrypt`): the Controller hands the key to the
  nodes;
- TLS for the RPC between agents (`tls.defaults` with `verify_incoming`
  and `verify_outgoing`, `tls.internal_rpc.verify_server_hostname`), and
  `auto_encrypt { allow_tls = true }` on the servers: each node's agent
  gets its own certificate from them - you give the Controller only the
  CA;
- the HTTP API over HTTPS, reachable from the Controller (a reverse
  proxy in front is fine).

The Controller's token: a policy that lets it make the nodes' tokens and
their policy, read the gossip key, and take a node it no longer manages
out of the catalog:

```hcl
acl      = "write"
keyring  = "read"
operator = "read"
node_prefix "" {
  policy = "write"
}
service_prefix "" {
  policy = "read"
}
agent_prefix "" {
  policy = "read"
}
```

```sh
consul acl policy create -name janus-controller -rules @janus-controller.hcl
consul acl token create -description "Janus Controller" -policy-name janus-controller
```

The network: every agent gossips with every other on **8301 (TCP and
UDP)** - the nodes with the servers and with each other -, and talks to
the servers on **8300**. With the [firewall](../firewall.md), accept them
from the cluster's network on the nodes ([consul.md](../consul.md#network)).

## The settings

**Settings › Consul** (admins):

- **Address**: the HTTP API, `https://consul.example.net` (or `:8501`).
- **Trust**: how the Controller checks the server's certificate - the
  system's store, a CA, or a certificate's SHA-256 fingerprint.
- **Token**: the Controller's own. Never shown again: left empty, the
  saved one is kept.
- **RPC CA**: the CA of the servers' RPC certificates, given to the
  nodes. Empty: the RPC isn't encrypted.
- **Nodes**: a label selector (`env=prod,dc=lgs`); empty, every node with
  the extension.
- **Services**: which ones the nodes register (below).
- **Node metadata from labels**: a node's labels are its `node_meta` in
  Consul (on by default).

**Test** reads the cluster with the settings as the page shows them -
saved or not -, and says in words what works and what doesn't: the
datacenter and the servers, the gossip key, the token, the nodes' policy
(made or brought up to date: that's what proves `acl = "write"`), the
members. Once saved and on, the Controller checks every node at once, then
every 10 minutes - a minute later after one took its layer, every
minute while one can't -, and
**Run now** has it check again. The table says, for each node, where it
stands, whether its agent is an alive member, how many services and URLs
it has in the catalog.

## What each node gets

**A token of its own**: its node identity (`node "<name>" write`,
`service_prefix "" read`) and the policy `janus-nodes`, which the
Controller makes and keeps:

```hcl
agent_prefix "" {
  policy = "read"
}
node_prefix "" {
  policy = "read"
}
service "janus" {
  policy = "write"
}
service "janus-exporter" {
  policy = "write"
}
service "node-exporter" {
  policy = "write"
}
service_prefix "haproxy-" {
  policy = "write"
}
```

Its description says whose it is: `janus node edge-1 (<node ID>) -
Controller <ID>`. A token deleted in Consul is made again; a node renamed
gets its token's identity changed.

**The managed layer** (`NetworkService/ConsulManagedSet`): on the node,
apart from your configuration, rendered into the agent's configuration
after it - the Controller's keys win, its `retry_join` is added to yours:

| Key | From |
|---|---|
| `datacenter` | Consul's |
| `node_name` | the node's name at the Controller |
| `retry_join` | the servers, as Consul lists them |
| `bind_addr`, `advertise_addr` | the node's address at the Controller, else its main IPv4 address |
| `encrypt` | the gossip key, from Consul's keyring |
| `acl` | on, denying by default, `tokens.agent` = its token |
| `tls`, `auto_encrypt` | the RPC CA, `verify_outgoing`, `verify_server_hostname`, `auto_encrypt.tls` |
| `node_meta` | its labels (`.` and `/` in a key as `_`), `janus_node_id`, `janus_cluster` for a [cluster](clusters.md) member |

`consul validate` checks it with your configuration before anything
changes; the agent restarts only when what it reads changed. A node with
no configuration of yours runs on the layer alone. The token and the gossip
key never come back from the node: `janusctl network consul managed`
says whether they're set.

```text
Managed by: janus-controller 3f2a9c1e consul 5b0e77d2
Datacenter: lgs
Node name:  edge-1
Join:       10.1.0.10
Advertise:  192.0.2.11
Gossip key: set
ACL token:  set
RPC CA:     set (TLS, auto_encrypt)
Node meta:  dc=lgs, janus_node_id=4c1b..., room=lab
Registers:  janus, exporters, frontends, urls
Services:   9 registered, last synced 2026-10-29 10:42:07
```

A managed node is a client: the layer turns `auto_encrypt.tls` on, which
`consul validate` refuses with a server's configuration.

## The node's services

The node registers them itself, in its agent, with its token - only IDs
starting `janus:`: your own `services {}` are never touched.

| Service | What |
|---|---|
| `janus` | janusd's API, `:9505`, a TCP check |
| `janus-exporter` | the [Janus exporter](../metrics.md) while it's on, where it listens: its main address (`auto`), the address it's set to, else the node's; an HTTP check on its metrics |
| `node-exporter` | the [node exporter](../metrics.md#the-node-exporter) while its service runs, where it listens, the same way |
| `haproxy-exporter` | each frontend or listen section that has HAProxy's own exporter answer (`http-request use-service prometheus-exporter`): its first bind's address and port, `metrics_path` from the rule's condition (`path /metrics`, `path_beg /prom`) - else `/metrics` -, `scheme` `https` when that bind has `ssl`; a TCP check |
| `haproxy-<name>` | each frontend and listen section: the port of its first bind, the bind's address when it names one (an anycast address) - else the node's -, tags `haproxy`, its mode, `tls`; a TCP check; meta `frontend`, `maps` |
| `haproxy-url` | one instance per URL the configuration routes, ID `janus:url:<frontend>:<host>`, no check; meta `url`, `frontend`, `backend`, `source` |

Every exporter's instance has the tag `prometheus` and meta
`metrics_path` and `scheme`: one Prometheus job finds them all (below).
Turned off, stopped, moved: the node registers it again, or takes it off,
within seconds. No other extension declares an exporter yet.

The URLs' hosts:

- the keys of the maps a `use_backend %[...map(...)]` reads by the Host
  header (`req.hdr(host)`, `hdr(host)`, through a variable too), by
  `base` (cut to the host), or by the TLS server name (`req.ssl_sni` - a
  TCP frontend passing TLS through);
- the literal values of the Host ACLs a `use_backend`'s condition names,
  or holds between braces: `hdr(host) -i`, `-m str`, `-m dom`;
- full domain names only: IP addresses, wildcards (`*.example.com`),
  regular expressions and partial matches are left out.

The map's content is HAProxy's own, as it is now - an entry added at
runtime (`janusctl haproxy map-set`) counts. `url` is `https://` when the
frontend binds with `ssl` (or passes TLS through by its server name),
`http://` otherwise, with the port when it isn't the scheme's own;
`source` is `map:<path>` or `acl`. A node registers 1000 URLs at most.

With this frontend and `hosts.map` holding `www.example.com be_www`,

```haproxy
frontend fe_https
    bind 192.0.2.151:443 ssl crt /etc/haproxy/files/certs/
    acl host_status hdr(host) -i status.example.com
    use_backend be_status if host_status
    use_backend %[req.hdr(host),lower,map(/etc/haproxy/files/hosts.map,be_default)]
```

the node registers `haproxy-fe-https` on `192.0.2.151:443` (tags
`haproxy`, `http`, `tls`), and two `haproxy-url` instances:
`https://status.example.com` (backend `be_status`, source `acl`) and
`https://www.example.com` (`be_www`, `map:/etc/haproxy/files/hosts.map`).

The node syncs them when its agent starts, when HAProxy runs another
configuration or a map changes, when the layer changes - and every
minute, putting back what someone took off and taking off the `janus:*`
services it no longer has. `janusctl network consul services` lists the
agent's services, its own and yours, with their checks:

```text
NAME              ID                                  ADDRESS          TAGS                  META                      HEALTH   BY
haproxy-exporter  janus:haproxy-exporter:fe_stats     192.0.2.11:8405  haproxy,prometheus    http /metrics, frontend fe_stats  passing  Janus
haproxy-fe-https  janus:haproxy:fe_https              192.0.2.151:443  haproxy,http,tls      frontend fe_https         passing  Janus
janus             janus:janus                         192.0.2.11:9505  janus,grpc            -                         passing  Janus
janus-exporter    janus:janus-exporter                192.0.2.11:10056 janus,prometheus      http /metrics             passing  Janus
haproxy-url       janus:url:fe_https:www.example.com  192.0.2.151:443  haproxy,url,https     https://www.example.com   -        Janus
web               web                                 192.0.2.11:8080  -                     -                         passing  configuration
```

## Drained, stopped, going down

While a node is [drained](clusters.md), its HAProxy stopped, or the node
going down (a reboot, a shutdown, an update), its `janus:*` services are
in maintenance - a probe reading Consul's health leaves them out; they
come back when it's undrained or up again.

## A node no longer managed

A node the selector no longer picks, without the extension any more, or
removed from the Controller: its layer is taken off (its agent stops if
you gave it no configuration of your own - leaving the cluster), its token
deleted, its entry in the catalog deregistered. A node another
Controller manages is left alone. Turning the integration off does the
same on every node.

## Prometheus: the exporters and the URLs

Prometheus's
[Consul service discovery](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#consul_sd_config)
finds the exporters by their tag `prometheus` - each scraped on its own
path and scheme -, and hands each `haproxy-url` instance to the
[blackbox exporter](https://github.com/prometheus/blackbox_exporter) -
its URL in the instance's metadata, the node's labels in the node's:

```yaml title="examples/prometheus/consul.yml"
# Prometheus finding the Janus nodes and what they serve in Consul
# (docs/guide/consul-controller.md): every exporter the nodes run - the
# services tagged prometheus: janus-exporter, node-exporter,
# haproxy-exporter -, each on its own path and scheme, and every URL the
# nodes' maps and Host ACLs route (the haproxy-url service) probed by the
# blackbox exporter. Its token reads the catalog: node_prefix and
# service_prefix "" read. Checked by promtool (make examples-check).
global:
  scrape_interval: 30s

scrape_configs:
  - job_name: consul_exporters
    consul_sd_configs:
      - server: consul.example.net:443
        scheme: https
        datacenter: lgs
        tags: [prometheus]
        authorization:
          credentials_file: /etc/prometheus/consul-token
    # haproxy-exporter on a bind with ssl: its certificate, checked
    # against your CA.
    tls_config:
      ca_file: /etc/prometheus/internal-ca.pem
    relabel_configs:
      # Each exporter where it says: its path, its scheme.
      - source_labels: [__meta_consul_service_metadata_metrics_path]
        target_label: __metrics_path__
      - source_labels: [__meta_consul_service_metadata_scheme]
        target_label: __scheme__
      - source_labels: [__meta_consul_service]
        target_label: exporter
      - source_labels: [__meta_consul_service_metadata_frontend]
        target_label: frontend
      - source_labels: [__meta_consul_node]
        target_label: node
      - source_labels: [__meta_consul_metadata_dc]
        target_label: dc
      - source_labels: [__meta_consul_metadata_janus_cluster]
        target_label: cluster

  - job_name: blackbox_consul
    metrics_path: /probe
    params:
      module: [http_2xx]
    consul_sd_configs:
      - server: consul.example.net:443
        scheme: https
        datacenter: lgs
        services: [haproxy-url]
        authorization:
          credentials_file: /etc/prometheus/consul-token
    relabel_configs:
      # The URL is what the exporter probes, and the target's name - not
      # the node: the members of a cluster register the same URLs, and
      # Prometheus keeps one target for them.
      - source_labels: [__meta_consul_service_metadata_url]
        target_label: __param_target
      - source_labels: [__meta_consul_service_metadata_url]
        target_label: instance
      - source_labels: [__meta_consul_service_metadata_frontend]
        target_label: frontend
      - source_labels: [__meta_consul_metadata_dc]
        target_label: dc
      - source_labels: [__meta_consul_metadata_janus_cluster]
        target_label: cluster
      # A drained or stopping node's instances are in maintenance
      # (critical): the other members serve the same URLs.
      - source_labels: [__meta_consul_health]
        regex: critical
        action: drop
      # A URL meant to answer 401 or 403: left out.
      - source_labels: [__meta_consul_service_metadata_url]
        regex: https://intranet\.example\.net
        action: drop
      - target_label: __address__
        replacement: blackbox.example.net:9115
```

Each target carries its instance's health (`__meta_consul_health`): a
drained node's URLs, in maintenance, are `critical` - dropped above, the
other members of its cluster still probed for them.

## The API

| Call | Role | What |
|---|---|---|
| `GET /api/consul` | admin | the settings (the token as `has_token`), what the last pass read of Consul, its error |
| `PUT /api/consul` | admin, a session | the settings; a token left empty keeps the saved one |
| `POST /api/consul/test` | admin, a session | test settings, saved or not |
| `POST /api/consul/run` | admin | check every node now |
| `GET /api/consul/nodes` | admin | each node: its state, its Consul name, its token's accessor ID, its member state, its services and URLs in the catalog |

The settings, the Controller's token and the nodes' tokens are sealed
with the master key in `<data>/consul/state.json`. On the node, the layer
is `config/consul-managed.json` on STATE (mode 0600, never served by the
file RPCs); a [state transfer](replacing-a-node.md) carries it only with
the node's identity - a replacement node gets its own token from the
Controller.
