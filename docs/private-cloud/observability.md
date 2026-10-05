# Observability

A Janus node is watched the way any server is - Prometheus metrics and
alerts - plus what an immutable node can't offer and what replaces it:
no shell to look around in, so its logs, processes, files and packets
are read through its API.

## Metrics

Three exporters, each with what only it knows:

| Exporter | Where | What |
|---|---|---|
| **Janus** | every node, `:10056/metrics` | The node as Janus runs it: version and schematic, the slot it booted, certificates' expiry, HAProxy up and restarts, configuration applies, extension services, NTP, SELinux denials, VRRP and BGP state, Let's Encrypt renewals |
| **node_exporter** | with the `prometheus-node-exporter` extension, `:9100/metrics` | CPU, memory, disks, filesystems, network |
| **HAProxy's own** | where your configuration serves it | Frontends, backends, servers: traffic, errors, latency, health |

```yaml title="examples/prometheus/prometheus.yml"
# Prometheus scraping a fleet of Janus nodes: the Janus exporter every
# node serves (:10056), node_exporter where the image has the
# prometheus-node-exporter extension (:9100), and HAProxy's own
# exporter where the configuration serves it (:8405, as in
# ../haproxy/web.cfg). The alerts: janus-rules.yml. Checked by promtool
# (make examples-check).
global:
  scrape_interval: 15s

rule_files:
  - janus-rules.yml

scrape_configs:
  - job_name: janus
    static_configs:
      - targets: ['lb1.example.net:10056', 'lb2.example.net:10056']

  - job_name: node
    static_configs:
      - targets: ['lb1.example.net:9100', 'lb2.example.net:9100']

  - job_name: haproxy
    static_configs:
      - targets: ['lb1.example.net:8405', 'lb2.example.net:8405']
```

- **The alerts**: [`examples/prometheus/janus-rules.yml`](../../examples/prometheus/janus-rules.yml)
  - certificates expiring, HAProxy down or crashing, SELinux denials,
  OOM kills, errors on STATE, the clock, an update waiting for its
  health confirmation, refused API calls, VRRP faults, BGP sessions
  down, Let's Encrypt renewals failing. Every metric:
  [metrics](../metrics.md).
- **HAProxy's exporter** is a frontend of your configuration, like
  `:8405` in [`examples/haproxy/web.cfg`](../../examples/haproxy/web.cfg).
- None of these ports has authentication, like any Prometheus exporter:
  keep them on the management network, or behind the node's
  [firewall](../firewall.md). The Janus exporter's port can move, or the
  exporter be turned off: `janusctl system metrics -port ...`.

## Logs

A node keeps the last 5000 lines of each service's output in memory -
janusd, HAProxy (`log stdout` in its global section) and every extension
service - plus its event log and the kernel's messages, and streams them
through its API:

```sh
janusctl -n lb1 system logs -f haproxy      # follow HAProxy's output
janusctl -n lb1 system logs -n 200 janusd   # its last 200 lines
janusctl -n lb1 system events               # the node's event log, live
janusctl -n lb1 system dmesg -f             # the kernel: AVC denials, OOM kills...
```

The Controller's **Logs** page shows the same, with filters, pause and
download. To keep logs longer than the node does, a collector on the
management network can follow them through the API - `janusctl ...
system logs -f haproxy` into the collector's input.

**Who did what**: each node logs every call that changes something with
the account behind it (`api: <Method>: <caller>`), the Controller
included - it acts on a node for the account using its page. The
Controller keeps its own audit log of every change made through it
(`audit.jsonl` in its data directory).

## When something's wrong

- **On a node's page** - or with `janusctl` - processes and their memory,
  sockets, interfaces and their counters, mounts, disk use, and a
  read-only file browser: what `ps`, `ss`, `ip`, `df` and `cat` would
  show, without a shell.
- **Packet captures** stream to a `.pcap` file, with tcpdump's filter
  language: [packet capture](../packet-capture.md).
- **A node that won't register** says why on its console, and the
  Controller shows that line on the machine it created
  ([first boot](first-boot.md)).
- **The Controller's own state**: its main page warns when backups fail
  or are late, when a newer release exists, and which nodes are offline
  or have an update waiting - with a 🔒 for a security update.
