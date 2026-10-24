# Clusters

A cluster is a group of nodes the Controller keeps alike: one HAProxy
configuration on every member, their stick tables replicated between
them, their certificates and keys carried from member to member, VRRP
configured from one template, and each member locked against changes
made behind the cluster's back. This page is how to run one through the
Controller - its **Clusters** tab, or its API; how it works is [the
cluster design](../cluster.md).

![The Clusters tab's page of cluster edge-par: in sync, version 1, managing HAProxy's configuration and files; its two members, edge-par-1 (where the secrets come from) and edge-par-2, each running version 1 with its peer up and the stick tables in sync](../assets/screenshots/controller-cluster-light.webp)

## What a cluster gives

- **One configuration**, versioned: `haproxy.cfg`, HAProxy's files (maps,
  error pages), and templates for `keepalived.conf`, `bird.conf` and
  `firewall.nft` rendered for each member - its VRRP priority, its
  router ID, its peers' addresses. A version is applied one member at a
  time, checked on the first before any is touched.
- **Stick tables replicated** over HAProxy's `peers` protocol, mutual TLS
  between the members: sessions, counters and rate limits follow a
  client from one member to another, and survive a reload or a failover.
- **Secrets that never leave the nodes for the Controller**: certificates
  and their keys, the Let's Encrypt certificates, the TLS ticket key ring
  go from one member to the others over their own mutual TLS.
- **Locks**: what the cluster manages can only be changed through it. A
  member's own administrator can take a domain back for an emergency -
  the cluster then reports the member as drifted, and never overwrites
  it by itself.

## Before you start

- The nodes trust the Controller's fleet
  ([securing the fleet](../../dashboard/README.md#securing-the-fleet)):
  the cluster acts on its members through it.
- They run the same image - the same extensions, HAProxy branch and
  kernel; `keepalived` in it for VRRP, `bird` for BGP.
- They reach each other on TCP 9505 (the API, which secrets are pulled
  over) and 10057 (HAProxy's peers). A member's firewall must let the
  others in on both.
- Your `haproxy.cfg` names `peers janus` on the stick tables to replicate
  ([your haproxy.cfg](../haproxy-config.md)):

```text
frontend fe
    bind :443 ssl crt /etc/haproxy/files/certs/www.pem tls-ticket-keys /etc/haproxy/cluster/tls-ticket-keys
    stick-table type ip size 100k expire 30m store http_req_rate(10s) peers janus
    http-request track-sc0 src
```

## Creating one

From nodes the Controller has, one of them the **source**: its
configuration becomes the cluster's first version, its certificates and
keys every member's. On the page: **Clusters**, **New cluster** - its
name, then the nodes that join it, each with its member name and the
address the others reach it on, and the source. Through the API:

```sh
curl -X POST https://controller:8080/api/clusters -H "Authorization: Bearer $JANUS_TOKEN" -d '{
  "name": "edge",
  "source": "6f1c2e3a9b7d0451",
  "members": [
    {"node_id": "6f1c2e3a9b7d0451", "name": "lb-1", "address": "192.0.2.11"},
    {"node_id": "a03be1c47d5e9f20", "name": "lb-2", "address": "192.0.2.12"}
  ]
}'
```

- `name`: a label - letters, digits, dashes.
- A member's `name` is HAProxy's peer name; `address` is where the other
  members reach it - left out, the address the Controller reaches it at,
  when that isn't a loopback or a NAT.
- `managed` - what the cluster manages from the start - is
  `["haproxy", "files"]` by default; VRRP, BGP and the firewall come with
  a version carrying their template (below).

The answer is `202`: the cluster is created in the background - checks
(the fleet, the image, HAProxy healthy, each member reaching the others),
a member certificate for each (its key made on the node), the first
version taken from the source, the secrets pulled - first, as the
version may name a file holding a key - and the version applied. `GET
/api/clusters/{id}` follows it: `busy` while it runs, `error` if it
failed, `problems` for whatever wants a look.

## The configuration

On the page, a cluster's **Configuration** tab: its versions, newest
first, and each one's files; **Edit** makes a new version from the one
shown, **Apply** shows what each file changes from the version the
members run and what the cluster will manage, and asks first.

Through the API, a new version starts from the cluster's current one
(`base`: another), adds or replaces `files` and takes away `delete`:

```sh
curl -X POST https://controller:8080/api/clusters/$CLUSTER/versions -H "Authorization: Bearer $JANUS_TOKEN" -d '{
  "files": {"keepalived.conf": "...", "files/maps/hosts.map": "example.test be_web\n"},
  "note": "VRRP"
}'
```

Its files: `haproxy.cfg`, `files/<name>` (HAProxy's files that hold no
private key - those go through the cluster's certificates, below),
`keepalived.conf`, `bird.conf`, `firewall.nft`. The last three are
templates ([Go's](https://pkg.go.dev/text/template)), rendered for each
member:

| In a template | What it is |
|---|---|
| `{{ .Member.Name }}`, `{{ .Member.Index }}` | The member's name, and its rank from 1 |
| `{{ .Member.Address }}` | Its address |
| `{{ .Member.Vars.priority }}` | One of its variables - set with `PATCH /api/clusters/{id}` `{"vars": {"lb-1": {"priority": "150"}}}` |
| `{{ range .Peers }}{{ .Address }}{{ end }}` | The other members, each with the same fields |
| `{{ .Cluster.ID }}`, `{{ .Cluster.Name }}` | The cluster |

A variable a member lacks is an error, not an empty value. A VRRP
instance for a cluster, each member its priority and its peers:

```text
vrrp_instance front {
    state BACKUP
    interface eth1
    virtual_router_id 51
    priority {{ .Member.Vars.priority }}
    advert_int 0.2
    unicast_src_ip {{ .Member.Address }}
    unicast_peer {
    {{- range .Peers }}
        {{ .Address }}
    {{- end }}
    }
    track_file {
        haproxy weight -100
    }
    virtual_ipaddress {
        192.0.2.100/24
    }
}
```

Then apply it - `managed` when the cluster starts or stops managing a
domain:

```sh
curl -X POST https://controller:8080/api/clusters/$CLUSTER/apply -H "Authorization: Bearer $JANUS_TOKEN" \
  -d '{"version": 2, "managed": ["haproxy", "files", "vrrp"]}'
```

The version is rendered for every member first, checked on the first
member (`haproxy -c`, each daemon's own check), then applied one member
at a time: the files, HAProxy, the files the version no longer has, the
firewall on trial and confirmed, VRRP, BGP. The first failure stops it:
the members before run the new version, the others the one they ran,
and the cluster says which. Going back is applying an older version.

## Locks and drift

On a member, a write to what the cluster manages is refused, whoever
makes it, unless the Controller makes it for that cluster:

```text
HAProxy's configuration is managed by cluster edge: change it on the cluster, or release this node's lock first (janusctl cluster release haproxy) - the cluster then reports this node as drifted
```

For an emergency on one node, its administrator releases the domain
(`janusctl -n lb-2 cluster release haproxy`) and changes it. The cluster
sees it at its next look - every 10 minutes, or `POST
/api/clusters/{id}/check` - and reports the member as drifted, its
configuration left as it is. The next apply puts it back and locks it
again. Runtime changes - a server's state, maps and ACLs, stick table
entries, the firewall's sets - are each node's and never locked.

## Certificates and keys

A certificate goes to the cluster, never to a member - on the page, the
cluster's **Certificates** tab; through the API:

```sh
curl -X POST https://controller:8080/api/clusters/$CLUSTER/certificates -H "Authorization: Bearer $JANUS_TOKEN" \
  -d "$(jq -n --rawfile pem www.pem '{name: "www", pem: $pem}')"
```

It is relayed to the member the secrets come from (`secrets_from`, the
source by default) - never kept by the Controller - and the others pull
it from there over their own mutual TLS. `POST /api/clusters/{id}/pull`
has them pull everything again: the uploaded certificates, the files
holding a key, the Let's Encrypt certificates, the TLS ticket key ring.

The ticket key ring rotates every day on that member, and the others
pull it: a client resumes its TLS session on any member - as long as
they serve it the same certificate.

## Members

On the page, the cluster's **Members** tab: each member's state, and
for an admin **Drain**, its **Variables**, remove, **Add a member**,
and the member the secrets come from. Through the API:

- `POST /api/clusters/{id}/members` `{"node_id": ..., "name": ..., "address": ...}`:
  checked, given its certificate, the others told, the secrets pulled,
  the cluster's version applied.
- `DELETE /api/clusters/{id}/members/{name}`: drained first - its virtual
  IPs and routes move to the others -, out of the cluster, the others
  told. `?force=true` for a node that doesn't answer any more.
- `POST /api/clusters/{id}/members/{name}/drain` and `/undrain`: out of
  service and back, without leaving.
- A node that is a member can't be removed from the Controller: it
  leaves its cluster first.

## Watching it

The page shows what wants a look at the top of a cluster's page, and
what was done in its **Journal** tab; **Check now** looks at every
member at once. Through the API:

- `GET /api/clusters/{id}`: each member's applied version, drift, released
  domains, peers up, stick tables in sync, certificate expiry, drain -
  and `problems`, worded for you.
- `GET /api/clusters/{id}/journal`: what was done, by whom, member by
  member.
- On a node: `janusctl cluster status` shows its peers as HAProxy sees
  them; its exporter has `janus_cluster_info`, `janus_cluster_peer_up`
  and `janus_cluster_peer_table_synced` ([metrics](../metrics.md)).

## The API

Readers see clusters, administrators change them.

| Call | What it does |
|---|---|
| `GET`, `POST /api/clusters` | list; create (`{name, source, members: [{node_id, name, address, api_address, vars}], managed, peers_port}`) |
| `GET`, `PATCH`, `DELETE /api/clusters/{id}` | one; change `secrets_from` or members' `vars`; delete (every member leaves; `?force=true`: even those that don't answer) |
| `POST /api/clusters/{id}/members`, `DELETE .../members/{name}` | add one; remove one (`?force=true`) |
| `POST .../members/{name}/drain`, `.../undrain` | out of service, back |
| `GET`, `POST /api/clusters/{id}/versions`, `GET .../versions/{n}` | the versions; a new one (`{base, files, delete, note}`); one, with its files |
| `POST /api/clusters/{id}/apply` | apply a version (`{version, managed}`) |
| `POST /api/clusters/{id}/certificates` | a certificate for every member (`{name, pem, crt_list, sni}`) |
| `POST /api/clusters/{id}/pull` | the members pull the secrets again |
| `POST /api/clusters/{id}/check` | look at every member now |
| `GET /api/clusters/{id}/journal` | what was done (`?n=`) |

What takes a while answers `202` and runs in the background, `409` while
another operation runs on the same cluster.
