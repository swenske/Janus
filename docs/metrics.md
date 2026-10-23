# Metrics: the Janus exporter

Every node serves its own Prometheus metrics: what only Janus knows about
the node - certificate expiry, which slot it booted, pending upgrades,
HAProxy as janusd runs it, extension services, time sync, SELinux. It
doesn't repeat what other exporters already give:

| Exporter | Where | What |
|---|---|---|
| **Janus** (this page) | built into every node, `:10056/metrics` | the node as Janus manages it |
| HAProxy | your HAProxy configuration (below) | frontends, backends, servers, traffic |
| [prometheus-node-exporter](#the-node-exporter) | optional extension, `:9100/metrics` | CPU, memory, disks, filesystems, network |

## Turning it on and off

The exporter is **on by default**, plain HTTP on port **10056** - the
first port free after the [Prometheus exporters' default
allocations](https://github.com/prometheus/prometheus/wiki/Default-port-allocations).
Its settings are kept on the node across reboots and upgrades:

```sh
janusctl system metrics                       # show
janusctl system metrics -port 10100           # move it
janusctl system metrics -address 192.0.2.10   # listen on one address only
janusctl system metrics -address '*'          # on every address
janusctl system metrics -address auto         # back on the main interface
janusctl system metrics -disable              # turn it off
janusctl system metrics -enable
```

In the Controller: **Apps › Janus exporter**.
A port that can't be bound is refused, and the exporter stays where it
was.

Like node_exporter, it has no authentication: anyone who reaches the port
can read the metrics (versions, certificate names and expiry dates, API
call counts - no secrets). It listens on the node's main interface
unless told otherwise ([Where it listens](#where-it-listens)): on a node
with a management address, `-address` keeps the metrics off the
networks HAProxy serves. Restrict who reaches it in your network, or
with the node's firewall.

```yaml
scrape_configs:
  - job_name: janus
    static_configs:
      - targets: ['node1.example.net:10056', 'node2.example.net:10056']
```

## Where it listens

By default both exporters - this one and [the node
exporter](#the-node-exporter) - listen on the node's **main interface**:
the one its IPv4 default route leaves by (the lowest route metric
among the interfaces with a gateway), else the interface of the address
the kernel got by DHCP at boot, else the one with an IPv6 gateway. On
it, one address per IP version: the one whose subnet holds the gateway,
else the first that isn't a single host's (a `/32` or `/128` - an
anycast or service address, BGP's), else the first. Keepalived's virtual
addresses never count. They follow the network: a configuration applied
on trial, confirmed or reverted moves them with it. A node without any
such interface listens on every address.

The other choices: `-address '*'` for every address, `-address IP` for
one address (a management address). `janusctl system metrics` and the
Controller's page say the addresses in use:

```text
Exporter: http://10.0.2.15:10056/metrics (main interface)
```

> [!NOTE]
> A node set up before this default (v2026.10.22-2 and earlier) keeps
> listening on every address: its first boot on a newer version saves
> that as its setting. `-address auto` moves it to the main interface.
> When the default route goes out by the interface HAProxy serves - a
> front network with the gateway, a management network without -, set
> the management address instead.

## The node exporter

A node built with the `prometheus-node-exporter` extension
([image-factory.md](image-factory.md)) also runs Prometheus's
node_exporter: the host's CPU, memory, disks, filesystems and network.
Its settings are kept on the node, like the Janus exporter's: whether it
runs, the address and port it listens on (the [main
interface](#where-it-listens) and **9100** by default), and its
collectors - from a fixed list, each one known to work
on a Janus node. The default ones: `cpu`, `diskstats`, `filefd`,
`filesystem`, `loadavg`, `meminfo`, `netdev`, `netstat`, `os`,
`pressure`, `sockstat`, `stat`, `time`, `timex`, `uname`, `vmstat`. More
can be turned on: `arp`, `conntrack`, `cpufreq`, `dmi`, `entropy`,
`interrupts`, `netclass`, `nvme`, `softirqs`, `softnet`,
`thermal_zone`, `udp_queues`.

```sh
janusctl system node-exporter                                  # show
janusctl system node-exporter -address 192.0.2.10 -port 9200   # listen elsewhere
janusctl system node-exporter -address auto                    # the main interface (the default)
janusctl system node-exporter -collectors cpu,meminfo,netdev,softnet
janusctl system node-exporter -collectors default
janusctl system node-exporter -disable                         # stop it, and keep it stopped
```

In the Controller: **Apps › Node exporter**. A change restarts
node_exporter with it. Like a stock node_exporter, it has no
authentication: restrict who reaches the port.

## Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `janus_build_info` | gauge | `version`, `go_version`, `arch`, `schematic` | Always 1: the release and [image schematic](image-factory.md) the node runs |
| `janus_extension_info` | gauge | `extension`, `version` | Always 1, per extension in the image |
| `janus_component_info` | gauge | `component` (`haproxy`, `kernel`), `variant`, `version`, `pinned` | Always 1, per component: the HAProxy branch and kernel track the image is built with, and their versions - `pinned="true"` when its schematic names the variant, else it follows each release's default ([image-factory.md](image-factory.md)) |
| `janus_boot_info` | gauge | `slot`, `kernel` | Always 1: the A/B slot booted, and the kernel |
| `janus_daemon_start_time_seconds` | gauge | | When janusd started - it changes when janusd restarts |
| `janus_boot_stage_seconds` | gauge | `stage` (`kernel`, `init`, `haproxy`, `api`) | Seconds after the kernel started at which each stage of this boot completed: the kernel handed over to init, init started janusd, HAProxy served, the API listened. The firmware's own time (POST) comes before and isn't counted |
| `janus_upgrade_pending_confirmation` | gauge | | 1 while an upgrade waits for its health confirmation; the node reverts if it doesn't come |
| `janus_certificate_expiry_timestamp_seconds` | gauge | `source`, `certificate`, `cn` | When a certificate expires: the node API's CA and server certificates (`source="api"`), and every certificate HAProxy has loaded (`source="haproxy"`, by file or store name) |
| `janus_haproxy_up` | gauge | | 1 if HAProxy answers on its stats socket - HAProxy's own metrics can't report it down |
| `janus_haproxy_starts_total` | counter | | HAProxy processes janusd started, reloads included |
| `janus_haproxy_reloads_total` | counter | | Seamless reloads |
| `janus_haproxy_unexpected_exits_total` | counter | | HAProxy processes that exited without being stopped or replaced - crashes, the kernel's OOM killer |
| `janus_haproxy_restarts_total` | counter | | Times janusd started HAProxy again after it exited on its own |
| `janus_haproxy_untracked_processes` | gauge | | HAProxy processes that listen but aren't the one janusd runs - each one also an `haproxy.untracked` event. Should stay 0: such a process may serve another configuration |
| `janus_haproxy_config_applies_total` | counter | `result` (`accepted`, `rejected`) | Configurations applied through the API |
| `janus_haproxy_config_last_apply_timestamp_seconds` | gauge | | When the last configuration was applied through the API |
| `janus_haproxy_dropped_logs_total` | counter | | Log lines the running HAProxy process dropped rather than wait - its stdout or a log server couldn't take them at once, or two of its threads wrote to its stdout together ([log forwarding](guide/log-forwarding.md)); 0 again with each process |
| `janus_service_state` | gauge | `service`, `extension`, `state` | 1 for the current state (`running`, `waiting`, `restarting`, `stopped`) of each extension service |
| `janus_service_restarts_total` | counter | `service`, `extension` | Times an extension service exited and was restarted |
| `janus_time_synchronized` | gauge | | 1 once janusd's NTP client has set the clock |
| `janus_time_last_sync_timestamp_seconds` | gauge | | Last NTP synchronization |
| `janus_time_offset_seconds` | gauge | | The clock offset measured then |
| `janus_time_stratum` | gauge | | The server's stratum |
| `janus_network_trial_pending` | gauge | | 1 while a network configuration is on trial (it reverts unless confirmed) |
| `janus_network_trial_revert_timestamp_seconds` | gauge | | When it reverts, while on trial |
| `janus_firewall_configured` | gauge | | 1 if a firewall ruleset is saved ([firewall](firewall.md), the nftables extension) |
| `janus_firewall_trial_pending` | gauge | | 1 while a firewall ruleset is on trial: it reverts unless confirmed |
| `janus_firewall_set_elements` | gauge | `family`, `table`, `set` | Elements in each named set of the live ruleset |
| `janus_vrrp_instance_state` | gauge | `instance`, `interface`, `state` | 1 for each VRRP instance's current state (`MASTER`, `BACKUP`, `FAULT`, `INIT`, `STOP`) - [VRRP](vrrp.md), the keepalived extension |
| `janus_vrrp_instance_effective_priority` | gauge | `instance` | Its priority after tracking |
| `janus_vrrp_instance_became_master_total` | counter | `instance` | Times it became master since keepalived started |
| `janus_bgp_protocol_up` | gauge | `protocol`, `proto` | 1 if each BIRD protocol is up - [BGP](bgp.md), the bird extension |
| `janus_bgp_session_established` | gauge | `protocol`, `neighbor` | 1 if each BGP session is established |
| `janus_bgp_routes` | gauge | `protocol`, `channel`, `direction` (`imported`, `exported`) | Routes each protocol imported and exported |
| `janus_bgp_protocol_held_down` | gauge | `protocol` | 1 while janusd keeps a `haproxy_*` protocol down: HAProxy doesn't answer |
| `janus_acme_account_registered` | gauge | | 1 once the ACME CA knows the account - [Let's Encrypt](letsencrypt.md), the letsencrypt extension |
| `janus_acme_certificate_state` | gauge | `name`, `state` | 1 for each certificate's current state (`pending`: not obtained yet, `valid`, `due`, `expired`) |
| `janus_acme_certificate_failures` | gauge | `name` | Failed attempts in a row to obtain it |
| `janus_acme_certificate_renew_timestamp_seconds` | gauge | `name` | When it becomes due for renewal (0 until obtained) |
| `janus_acme_certificate_last_success_timestamp_seconds` | gauge | `name` | When it was last obtained (0: never) |
| `janus_cluster_info` | gauge | `cluster_id`, `cluster_name`, `member` | Always 1: the cluster the node is a member of and its member name ([cluster.md](cluster.md)); absent on a node in none |
| `janus_cluster_peer_up` | gauge | `peer` | 1 while HAProxy's connection to the peer is established: the stick tables replicate |
| `janus_cluster_peer_table_synced` | gauge | `peer`, `table` | 1 while the peer acknowledged every update this node made to the stick table |
| `janus_selinux_enforcing` | gauge | | 1 if SELinux is enforcing |
| `janus_selinux_denials_total` | counter | | SELinux denials in the kernel log since boot - there should be none |
| `janus_kernel_oom_kills_total` | counter | | Processes the kernel killed for lack of memory since boot |
| `janus_state_filesystem_errors` | gauge | | Errors the kernel recorded on STATE (PKI, configuration) - it should be 0 |
| `janus_logs_volume_size_bytes` | gauge | | The LOGS partition's size - the node's logs across reboots; absent on a node without one |
| `janus_logs_volume_written_bytes_total` | counter | | Bytes written to the LOGS partition since janusd started |
| `janus_logs_volume_dropped_records_total` | counter | | Log records the LOGS partition couldn't take (more printed than the disk wrote) - it should stay 0 |
| `janus_logs_volume_errors_total` | counter | | Failed writes to the LOGS partition - it should stay 0 |
| `janus_logship_target_up` | gauge | `target`, `kind` | 1 when the log target's last send worked, 0 while it fails ([log forwarding](guide/log-forwarding.md)) |
| `janus_logship_records_sent_total` | counter | `target` | Log records sent to the target |
| `janus_logship_bytes_sent_total` | counter | `target` | Bytes those records took: syslog messages with their framing, Elasticsearch request bodies as sent (gzipped when the target says so) |
| `janus_logship_records_dropped_total` | counter | `target` | Log records the target lost - its queue in memory was full, or the LOGS partition wrote over them before they were sent - it should stay 0 |
| `janus_logship_send_errors_total` | counter | `target` | Failed sends to the target (each is retried) |
| `janus_console_budget_bytes_per_second` | gauge | | What the node's console gets of the services' output, bytes a second - 0: none of it ([the console](guide/log-forwarding.md#the-console)) |
| `janus_console_budget_burst_bytes` | gauge | | At most this much of the services' output on the console at once |
| `janus_console_lines_left_out_total` | counter | `service` | Lines of the service's output the console didn't get since janusd started - kept whole in its log, the LOGS partition and the log targets |
| `janus_logship_queued_records` | gauge | `target` | Log records waiting for the target: in memory, and on the LOGS partition behind its place |
| `janus_api_requests_total` | counter | `method`, `code` | Calls to the node's API since janusd started, refused ones included |

A metric that doesn't apply is absent rather than 0: no NTP
synchronization yet, no configuration applied yet, no extension.

The VRRP metrics' `instance` label names the VRRP instance. Prometheus
keeps its own `instance` label, the target's, and stores the node's as
`exported_instance`.

## Alerts

```yaml title="examples/prometheus/janus-rules.yml"
groups:
  - name: janus
    rules:
      - alert: JanusCertificateExpiresSoon
        expr: janus_certificate_expiry_timestamp_seconds - time() < 14 * 86400
        labels: {severity: warning}
        annotations:
          summary: 'Janus certificate expires soon (instance {{ $labels.instance }})'
          description: '{{ $labels.certificate }} ({{ $labels.cn }}, source {{ $labels.source }}) on {{ $labels.instance }} expires in {{ $value | humanizeDuration }}.'
      - alert: JanusHAProxyDown
        expr: janus_haproxy_up == 0
        for: 1m
        labels: {severity: critical}
        annotations:
          summary: 'Janus HAProxy down (instance {{ $labels.instance }})'
          description: 'HAProxy on {{ $labels.instance }} has not answered on its stats socket for a minute.'
      - alert: JanusHAProxyCrashed
        expr: increase(janus_haproxy_unexpected_exits_total[15m]) > 0
        labels: {severity: warning}
        annotations:
          summary: 'Janus HAProxy crashed (instance {{ $labels.instance }})'
          description: 'An HAProxy process on {{ $labels.instance }} exited without being stopped or replaced in the last 15 minutes; janusd restarted it.'
      - alert: JanusExtensionServiceDown
        expr: janus_service_state{state="restarting"} == 1
        for: 5m
        labels: {severity: warning}
        annotations:
          summary: 'Janus extension service restarting (instance {{ $labels.instance }})'
          description: '{{ $labels.service }} ({{ $labels.extension }} extension) on {{ $labels.instance }} keeps exiting and being restarted.'
      - alert: JanusSELinuxDenials
        expr: increase(janus_selinux_denials_total[1h]) > 0
        labels: {severity: warning}
        annotations:
          summary: 'Janus SELinux denials (instance {{ $labels.instance }})'
          description: '{{ $labels.instance }} logged {{ $value | humanize }} SELinux denials in the last hour - there should be none (janusctl system dmesg).'
      - alert: JanusOOMKill
        expr: increase(janus_kernel_oom_kills_total[1h]) > 0
        labels: {severity: warning}
        annotations:
          summary: 'Janus OOM kill (instance {{ $labels.instance }})'
          description: 'The kernel of {{ $labels.instance }} killed a process for lack of memory in the last hour.'
      - alert: JanusStateErrors
        expr: janus_state_filesystem_errors > 0
        labels: {severity: critical}
        annotations:
          summary: 'Janus STATE filesystem errors (instance {{ $labels.instance }})'
          description: 'The kernel recorded {{ $value }} errors on the STATE partition of {{ $labels.instance }} (PKI, configuration).'
      - alert: JanusClockNotSynchronized
        expr: janus_time_synchronized == 0 or time() - janus_time_last_sync_timestamp_seconds > 3600
        for: 15m
        labels: {severity: warning}
        annotations:
          summary: 'Janus clock not synchronized (instance {{ $labels.instance }})'
          description: 'janusd on {{ $labels.instance }} has not synchronized its clock over NTP for more than an hour.'
      - alert: JanusUpgradeUnconfirmed
        expr: janus_upgrade_pending_confirmation == 1
        for: 10m
        labels: {severity: warning}
        annotations:
          summary: 'Janus upgrade unconfirmed (instance {{ $labels.instance }})'
          description: 'An upgrade of {{ $labels.instance }} has waited for its health confirmation for 10 minutes; the node reverts if it does not come.'
      - alert: JanusAPIRefusals
        expr: sum by (instance) (increase(janus_api_requests_total{code=~"PermissionDenied|Unauthenticated"}[15m])) > 10
        labels: {severity: warning}
        annotations:
          summary: 'Janus API refusals (instance {{ $labels.instance }})'
          description: 'The API of {{ $labels.instance }} refused {{ $value | humanize }} calls in the last 15 minutes (PermissionDenied/Unauthenticated).'
      - alert: JanusVRRPFault
        expr: janus_vrrp_instance_state{state="FAULT"} == 1
        for: 1m
        labels: {severity: critical}
        annotations:
          summary: 'Janus VRRP instance in FAULT (instance {{ $labels.instance }})'
          description: 'VRRP instance {{ $labels.exported_instance }} on {{ $labels.instance }} ({{ $labels.interface }}) has been in FAULT for a minute.'
      - alert: JanusBGPSessionDown
        expr: janus_bgp_session_established == 0
        for: 2m
        labels: {severity: critical}
        annotations:
          summary: 'Janus BGP session down (instance {{ $labels.instance }})'
          description: 'BGP session {{ $labels.protocol }} with {{ $labels.neighbor }} on {{ $labels.instance }} has not been established for 2 minutes.'
      - alert: JanusACMERenewalFailing
        expr: janus_acme_certificate_failures >= 3
        labels: {severity: warning}
        annotations:
          summary: "Janus Let's Encrypt renewal failing (instance {{ $labels.instance }})"
          description: '{{ $labels.name }} on {{ $labels.instance }}: {{ $value }} failed attempts in a row.'
      - alert: JanusACMECertificateNotObtained
        expr: janus_acme_certificate_state{state=~"pending|expired"} == 1
        for: 1h
        labels: {severity: critical}
        annotations:
          summary: "Janus Let's Encrypt certificate not obtained (instance {{ $labels.instance }})"
          description: '{{ $labels.name }} on {{ $labels.instance }} is {{ $labels.state }}: HAProxy serves a stand-in.'
```

## Grafana dashboard

[`grafana/janus-nodes.json`](../grafana/janus-nodes.json) shows these
metrics in Grafana (11.6 or later):

- **At the top**: nodes answering, HAProxy serving, the next
  certificate to expire, upgrades and trials awaiting confirmation,
  SELinux, OOM kills, errors on STATE - then a line per node: release,
  schematic, HAProxy and kernel, the slot it booted, when janusd
  started, how long it took to boot, the last configuration applied,
  NTP, SELinux.
- **HAProxy** serving over time, its reloads, crashes and
  configurations; **certificates** by expiry, and Let's Encrypt's;
  **extension services** and their versions; trials, the clock, API
  calls and errors; **VRRP, BGP and the firewall's sets** in a row that
  starts collapsed.
- janusd starting, configurations applied and HAProxy crashing mark
  every graph.

Its variables choose a Prometheus data source, the job that scrapes
the Janus exporter (`janus` above) and some or all of its nodes. Import
it (**Dashboards › New › Import**, upload the file), or let Grafana load
it with your other dashboards from a [file
provider](https://grafana.com/docs/grafana/latest/administration/provisioning/#dashboards).
A node's name shows without its port and domain: `lb1.example.net:10056`
is `lb1`.

## HAProxy's own metrics

Janus's HAProxy is built with its Prometheus exporter. Serve it from any
frontend of your configuration:

```haproxy
frontend prometheus
  bind :8405
  http-request use-service prometheus-exporter if { path /metrics }
  no log
```
