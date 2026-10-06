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
janusctl system metrics                  # show
janusctl system metrics -port 10100      # move it
janusctl system metrics -disable         # turn it off
janusctl system metrics -enable
```

In the Controller: **Apps › Janus exporter**.
A port that can't be bound is refused, and the exporter stays where it
was.

Like node_exporter, it has no authentication: anyone who reaches the port
can read the metrics (versions, certificate names and expiry dates, API
call counts - no secrets). Restrict who reaches it in your network, or
with the node's firewall.

```yaml
scrape_configs:
  - job_name: janus
    static_configs:
      - targets: ['node1.example.net:10056', 'node2.example.net:10056']
```

## The node exporter

A node built with the `prometheus-node-exporter` extension
([image-factory.md](image-factory.md)) also runs Prometheus's
node_exporter: the host's CPU, memory, disks, filesystems and network.
Its settings are kept on the node, like the Janus exporter's: whether it
runs, the address and port it listens on (every address and **9100** by
default), and its collectors - from a fixed list, each one known to work
on a Janus node. The default ones: `cpu`, `diskstats`, `filefd`,
`filesystem`, `loadavg`, `meminfo`, `netdev`, `netstat`, `os`,
`pressure`, `sockstat`, `stat`, `time`, `timex`, `uname`, `vmstat`. More
can be turned on: `arp`, `conntrack`, `cpufreq`, `dmi`, `entropy`,
`interrupts`, `netclass`, `nvme`, `softirqs`, `softnet`,
`thermal_zone`, `udp_queues`.

```sh
janusctl system node-exporter                                  # show
janusctl system node-exporter -address 192.0.2.10 -port 9200   # listen elsewhere
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
| `janus_upgrade_pending_confirmation` | gauge | | 1 while an upgrade waits for its health confirmation; the node reverts if it doesn't come |
| `janus_certificate_expiry_timestamp_seconds` | gauge | `source`, `certificate`, `cn` | When a certificate expires: the node API's CA and server certificates (`source="api"`), and every certificate HAProxy has loaded (`source="haproxy"`, by file or store name) |
| `janus_haproxy_up` | gauge | | 1 if HAProxy answers on its stats socket - HAProxy's own metrics can't report it down |
| `janus_haproxy_starts_total` | counter | | HAProxy processes janusd started, reloads included |
| `janus_haproxy_reloads_total` | counter | | Seamless reloads |
| `janus_haproxy_unexpected_exits_total` | counter | | HAProxy processes that exited without being stopped or replaced - crashes |
| `janus_haproxy_config_applies_total` | counter | `result` (`accepted`, `rejected`) | Configurations applied through the API |
| `janus_haproxy_config_last_apply_timestamp_seconds` | gauge | | When the last configuration was applied through the API |
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
| `janus_selinux_enforcing` | gauge | | 1 if SELinux is enforcing |
| `janus_selinux_denials_total` | counter | | SELinux denials in the kernel log since boot - there should be none |
| `janus_kernel_oom_kills_total` | counter | | Processes the kernel killed for lack of memory since boot |
| `janus_state_filesystem_errors` | gauge | | Errors the kernel recorded on STATE (PKI, configuration) - it should be 0 |
| `janus_api_requests_total` | counter | `method`, `code` | Calls to the node's API since janusd started, refused ones included |

A metric that doesn't apply is absent rather than 0: no NTP
synchronization yet, no configuration applied yet, no extension.

## Alerts

```yaml title="examples/prometheus/janus-rules.yml"
groups:
  - name: janus
    rules:
      - alert: JanusCertificateExpiresSoon
        expr: janus_certificate_expiry_timestamp_seconds - time() < 14 * 86400
        labels: {severity: warning}
        annotations:
          summary: '{{ $labels.certificate }} ({{ $labels.cn }}) on {{ $labels.instance }} expires in {{ $value | humanizeDuration }}'
      - alert: JanusHAProxyDown
        expr: janus_haproxy_up == 0
        for: 1m
        labels: {severity: critical}
      - alert: JanusHAProxyCrashed
        expr: increase(janus_haproxy_unexpected_exits_total[15m]) > 0
        labels: {severity: warning}
      - alert: JanusExtensionServiceDown
        expr: janus_service_state{state="restarting"} == 1
        for: 5m
        labels: {severity: warning}
      - alert: JanusSELinuxDenials
        expr: increase(janus_selinux_denials_total[1h]) > 0
        labels: {severity: warning}
      - alert: JanusOOMKill
        expr: increase(janus_kernel_oom_kills_total[1h]) > 0
        labels: {severity: warning}
      - alert: JanusStateErrors
        expr: janus_state_filesystem_errors > 0
        labels: {severity: critical}
      - alert: JanusClockNotSynchronized
        expr: janus_time_synchronized == 0 or time() - janus_time_last_sync_timestamp_seconds > 3600
        for: 15m
        labels: {severity: warning}
      - alert: JanusUpgradeUnconfirmed
        expr: janus_upgrade_pending_confirmation == 1
        for: 10m
        labels: {severity: warning}
      - alert: JanusAPIRefusals
        expr: sum by (instance) (increase(janus_api_requests_total{code=~"PermissionDenied|Unauthenticated"}[15m])) > 10
        labels: {severity: warning}
      - alert: JanusVRRPFault
        expr: janus_vrrp_instance_state{state="FAULT"} == 1
        for: 1m
        labels: {severity: critical}
      - alert: JanusBGPSessionDown
        expr: janus_bgp_session_established == 0
        for: 2m
        labels: {severity: critical}
      - alert: JanusACMERenewalFailing
        expr: janus_acme_certificate_failures >= 3
        labels: {severity: warning}
      - alert: JanusACMECertificateNotObtained
        expr: janus_acme_certificate_state{state=~"pending|expired"} == 1
        for: 1h
        labels: {severity: critical}
```

## HAProxy's own metrics

Janus's HAProxy is built with its Prometheus exporter. Serve it from any
frontend of your configuration:

```haproxy
frontend prometheus
  bind :8405
  http-request use-service prometheus-exporter if { path /metrics }
  no log
```
