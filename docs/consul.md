# Consul: the consul extension

Nodes built with the **consul** [extension](image-factory.md) run the
[Consul](https://developer.hashicorp.com/consul) agent with your own
configuration: HAProxy can take its servers from Consul's catalog, and the
node can register its own services. The configuration is Consul's own
(HCL or JSON), managed through the API, `janusctl network consul` and the
Controller's **Apps › Consul** page. Without the extension, the image has
no Consul at all.

The agent is HashiCorp's own release binary (checked against the
release's signed checksums when it's pinned in `versions.mk`), stripped
of its debug information. Consul is under the **Business Source License
1.1** - check that your use of it fits HashiCorp's terms.

## A configuration

A client joining an existing cluster, with gossip encryption and TLS:

```hcl
datacenter = "dc1"
bind_addr  = "192.0.2.10"     # this node's address on the cluster's network
retry_join = ["consul-1.example.com", "consul-2.example.com", "consul-3.example.com"]
encrypt    = "<the cluster's gossip key>"

tls {
  defaults {
    ca_file         = "/run/janus/consul/files/consul-agent-ca.pem"
    verify_outgoing = true
  }
  internal_rpc {
    verify_server_hostname = true
  }
}

acl {
  enabled = true
  tokens {
    agent = "<this agent's token>"
  }
}
```

- **What Janus adds**, after your configuration: `data_dir` (in `/run` -
  nothing the agent keeps needs to outlive a reboot when the rest comes
  from the configuration) and `node_id`, made once and kept on the node,
  so the node stays the same member of its cluster across reboots. The
  node name is the node's hostname unless you set `node_name`.
- **Files** the configuration names - CA, certificates, keys - are given
  with it and land in `/run/janus/consul/files/<name>` (see below).
- **bind_addr**: with several private addresses (one per network), the
  agent can't pick one: name it, or use a template (`{{ GetInterfaceIP
  "eth1" }}`).
- **Tokens**: put the agent's ACL tokens in the configuration - `consul
  acl set-agent-token` stores them in the data directory, which a reboot
  clears.
- **Health checks**: HTTP, TCP, gRPC, TTL. Script checks can't run: there
  is no shell on the node.
- **Clients, not servers**: the agent can run as a server (`server =
  true`), but its data directory is in `/run` - a server would lose its
  Raft state at every reboot. Keep the servers elsewhere; Janus nodes are
  their clients.

`consul validate` checks the configuration (with Janus's part) before
anything changes; applying one restarts the agent.

## HAProxy: service discovery

The agent answers DNS on `127.0.0.1:8600` (Consul's default
`client_addr` and port): a backend takes its servers from a service's SRV
records, and follows them as instances come and go or fail their checks.

```
resolvers consul
    nameserver consul 127.0.0.1:8600
    accepted_payload_size 8192
    hold valid 5s

backend web
    server-template web 5 _web._tcp.service.consul resolvers consul resolve-prefer ipv4 init-addr none check
```

`server-template` creates 5 server slots, filled with the service's
healthy instances - their addresses and ports.

## The node's services

Register what the node serves in your configuration, with checks HAProxy
answers:

```hcl
services {
  name = "lb-public"
  port = 443
  check {
    http     = "http://127.0.0.1:8080/"
    interval = "5s"
  }
}
```

## Files

The files a configuration names are given with it - `janusctl network
consul apply -file NAME=PATH ...`, or **Files** in the Controller - and
are `/run/janus/consul/files/<name>` for the agent. The node keeps them on
STATE (mode 0600: keys among them) and never gives their content back:
`janusctl network consul get` lists their names. A file applied empty
keeps the saved one of that name; janusctl keeps the saved files unless
`-only-files`.

## Applying

```sh
janusctl network consul check consul.hcl                     # consul validate, nothing changes
janusctl network consul apply -file consul-agent-ca.pem=./ca.pem consul.hcl
janusctl network consul status                               # the agent, its cluster's leader and members
janusctl network consul get                                  # the saved configuration, the files' names
janusctl network consul apply /dev/null                      # remove it all: the agent stops
```

The status asks the agent's HTTP API on `127.0.0.1:8500` without a token:
with ACLs denying anonymous reads, it shows the service's state only. The
agent's log is the `consul` service log (`janusctl system logs consul`).

## Network

The agent gossips on 8301 (TCP and UDP) with every member, and talks to
the servers on 8300; servers also use 8302 across datacenters. With the
[firewall](firewall.md), accept them from the cluster's network:

```
tcp dport { 8300, 8301 } ip saddr 192.0.2.0/24 accept
udp dport 8301 ip saddr 192.0.2.0/24 accept
```

## Size

The agent is about 30 MB of the image (compressed), next to about 14 MB
for the base: it fits the 64 MiB boot slots with room for the other
extensions.
