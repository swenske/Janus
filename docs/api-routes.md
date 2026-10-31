# Janus — gRPC API catalog

Full contract lives in `api/proto/janus/v1alpha1/*.proto` (generated
Go code in `gen/janus/v1alpha1`, implementations in `internal/api`).
This is the human-readable index, adapted from Talos's own
`MachineService`/`LifecycleService` (verified directly against
`siderolabs/talos`'s `.proto` files on GitHub - `api/machine/machine.proto`,
`storage.proto`, `lifecycle.proto`) with everything Kubernetes/etcd-
specific dropped, and `HAProxyService`/`NetworkService` added as
Janus's own differentiating surface.

Status column: ✅ implemented · ⬜ contract defined, returns
`codes.Unimplemented` (see `internal/api`).

**mTLS is mandatory on every connection** (`internal/pki`, wired up in
`cmd/janusd`) - there is no plaintext or unauthenticated mode. A
node generates its own CA + server certificate + an initial admin client
certificate on first boot; `GenerateClientConfiguration` issues
additional client certificates once you already have one. Once it trusts
a fleet (`AccessService` below), the client certificates of the fleet's
issuing CAs let in too.

**Every RPC is also role-checked** against the caller's certificate
(`internal/api/authz.go` against `internal/rbac`'s table, fail-closed -
an RPC with no explicit entry defaults to admin-only; the Controller
reads the same table to show a node's page only what its account may
do). Three roles:

- `os:reader`: status and observability - `Version`/`Hostname`/`Events`/
  `GetConfig`/`ValidateConfig`/`*Status`/`*List`/`*Get` - not file/log/
  packet-capture access or credential issuance, even though some of those
  are also technically non-mutating;
- `os:operator`: a reader that also runs what's set up - HAProxy's
  configuration, reload, servers' state, maps, ACLs, certificates, files,
  ACME; services and their logs; reboot, shutdown, janusd restart - not
  how the node is set up (network, firewall, VRRP/BGP/Consul, upgrades,
  reset, its files, packet capture, credentials, its fleet);
- `os:admin`: everything.

A Controller certificate (`janus:controller`) has no right of its own: it
names the user it acts for in each call's metadata (`janus-as-user`,
`janus-as-roles`) and the call gets that user's roles - narrowed, when
`janus-as-domains` names some, to those domains (`internal/rbac.Domain`:
observe, haproxy, services, network, system; observing always allowed). Every call that
isn't read-only is logged by janusd with who made it (`api:
HAProxyService/ApplyConfig: alice (os:operator) via janus-controller`).

## AccessService

The fleet a node trusts besides its own CA (`internal/pki/fleet.go`): it
pins the fleet's root once, then only takes a bundle that root signed,
newer than its own, listing the issuing CAs whose certificates let in - a
certificate the root signed itself lets in too. Its own CA always does.

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `TrustGet` | | ✅ | The fleet's root, the bundle (its version, issuing CAs, and the signed bundle itself - `janusctl fleet sync` takes a newer one), and the node's own CA (`local_ca_cert`: what a client of the fleet pins, checked on the fingerprint the console shows) - read-only |
| `TrustSet` | | ✅ | Pin the root (the first time) and apply a bundle it signed, newer than the node's - admin |
| `TrustReset` | | ✅ | Forget the fleet - admin, and only with a certificate of the node's own CA |
| `LocalCARotate` | | ✅ | Replace the node's own CA (admin): new CA, server and admin certificates; every certificate the old CA issued stops working, the fleet's don't. The new admin certificate is issued for the caller's public key (`janusctl access rotate-ca` makes the key locally), or the node makes the key and prints it on its console like at first boot. The new CA comes cross-signed by the old one, served after the server certificate: whoever pinned the old CA still verifies the node, and the Controller pins the new one by itself. Written in two steps on STATE - a power cut leaves the old CA or the new one, whole |

## SystemService

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `Version` | | ✅ | Daemon version, Go version, kernel version, active A/B slot, architecture, image schematic (ID and canonical JSON) and extensions, and the HAProxy branch and kernel track the image is built with, pinned or default ([image-factory.md](image-factory.md)); the other slot's image (`inactive_slot`: release, schematic, HAProxy, kernel - its `image.json`, read off its data partition with `internal/slotimage`, kept until an update writes there; `error` for an image before v2026.10.06-3); `secure_boot`: what the firmware does with Secure Boot (`ON`, `OFF`, `NO_UEFI`, read from its UEFI variables - [secure-boot.md](guide/secure-boot.md)) - connectivity check |
| `Hostname` | | ✅ | `janusctl system hostname` |
| `Reboot` | | ✅ | Soft-stops HAProxy (in-flight connections get 5s), stops the extensions' daemons (BIRD closes its BGP sessions, keepalived sends its priority 0 - keepalived and BIRD last), syncs, reboots - through the firmware (DEFAULT and POWERCYCLE, the same full reboot), or into the active kernel without it (KEXEC: refused, nothing rebooted, when Secure Boot is on and the image isn't signed by Janus) |
| `Shutdown` | | ✅ | Same graceful stop, then powers off |
| `Restart` | | ✅ | Restarts `janusd` only: rootfs/init starts it again and it takes the running HAProxy over with a seamless reload - HAProxy keeps serving throughout. Refused when `janusd` is PID 1 (e.g. `local-dev`), where nothing would start it again |
| `Reset` | | ✅ | `wipe_state` empties the persistent STATE partition (PKI, applied config, Controller registration, pending boot confirmation) and reboots: a new CA/admin certificate is printed on the console, current certificates stop working. A/B slots and ESP untouched. `wipe_ephemeral` alone just reboots (everything ephemeral is tmpfs) |
| `ApplyConfiguration` | server | ⬜ | Apply declarative config (`internal/config`), auto/no-reboot/reboot/try modes - no declarative machine config model exists yet |
| `Events` | server | ✅ | Event log (`internal/events`, last 1000): janusd start (its version, slot and boot), HAProxy started/exited (with why: reload, stop, or on its own), config applied/rejected, reloads, server state, maps/ACLs/certificates, stick table entries cleared and sessions ended (`haproxy.table.cleared`, `haproxy.session.shutdown`), services, upgrades (with the release written)/rollbacks/installs, boot confirmation/revert, self-registration, packet captures, connectivity tests (`probe.run`, who ran it), reboot/shutdown/restart/reset, vCPUs and memory plugged in or taken out (`system.cpus_changed` - with whether HAProxy was reloaded -, `system.memory_changed`), OOM kills (`kernel.oom_kill`, the victim), log forwarding configured, a log target failing or working again (`logship.configured`, `logship.failing`, `logship.recovered`). Kept on STATE (`events/events.jsonl`, synced every 5 s and before any reboot): it comes through janusd restarts, reboots and updates, IDs going on; a Reset wipes it. Backlog after `since_id`, then follows |
| `Dmesg` | server | ✅ | `/dev/kmsg`, formatted like `dmesg`; `follow` keeps streaming. `boot` -1, -2...: a previous boot's, from the LOGS partition (`internal/logvol`) |
| `Logs` | server | ✅ | `janusd` (its log, captured after the one-time PKI print so no private key is kept) or `haproxy` (stdout/stderr) - last 5000 lines in memory, `tail_lines`, `follow`. `boot` -1, -2...: a previous boot's, from the LOGS partition (HAProxy's own messages, not its access logs) |
| `LogBoots` | | ✅ | The LOGS partition: device, size, records written/dropped, and the boots it holds, newest first, with their offset for `Logs`/`Dmesg` |
| `Audit` | server | ✅ | The node's audit (admins): every call that changed something once done - the method as the type, the caller (and the Controller acting for them), the client's address, the code, the error, the duration as the payload - and every call refused. Last 2000 kept on STATE (`events/audit.jsonl`) across reboots and updates; `since_id`, then follows |
| `LogForwardingGet` | | ✅ | Where the logs go - the targets, without their secrets - and how each is doing ([log forwarding](guide/log-forwarding.md)) |
| `LogForwardingSet` | | ✅ | Replace the log targets: syslog over UDP/TCP/TLS, Elasticsearch (`_bulk`, data streams, ECS); applied at once, kept on STATE, secrets kept by target name |
| `LogForwardingTest` | | ✅ | Send one record to a target now, and say what happened |
| `Stats` | | ✅ | CPU (lifetime average, like `ps`) and RSS of `janusd` and `haproxy`, summed over processes |
| `SystemStat` | | ✅ | `/proc/stat`: boot time, context switches, processes created |
| `Memory` | | ✅ | `/proc/meminfo`, and the OOM kills since boot (`/proc/vmstat` `oom_kill`) |
| `CPUInfo` | | ✅ | `/proc/cpuinfo` (arm64: the core named from its MIDR), sockets and cores from sysfs topology |
| `LoadAvg` | | ✅ | `/proc/loadavg` |
| `DiskStats` | | ✅ | `/proc/diskstats` |
| `DiskUsage` | server | ✅ | Apparent size of each path; `recursive` adds one entry per directory (deepest first). Doesn't descend into `/proc`, `/sys`, `/dev` |
| `NetworkDeviceStats` | | ✅ | `/proc/net/dev`: bytes and errors per interface |
| `Netstat` | | ✅ | TCP through sock_diag, each connection with its bytes received/sent (`tcp_info`; `/proc/net/tcp{,6}` without them if the kernel refuses), UDP from `/proc/net/udp{,6}` - IPv4-mapped addresses shown as IPv4 |
| `ClientList` | | ✅ | The connections to the API open now: each one's certificate, the interface it comes by, its calls and bytes (TLS included); a Controller's, the users its calls were for in the last 2 minutes. Names and addresses of the others, and a Controller's users, for an admin only |
| `Mounts` | | ✅ | `/proc/self/mounts` + `statfs` sizes |
| `Processes` | | ✅ | Every process: pid, command line, CPU (lifetime average, and cumulative seconds for a rate between two samples), RSS |
| `ServiceList` | | ✅ | `janusd`, `haproxy` and the services of the image's extensions (`prometheus-node-exporter`, `qemu-guest-agent`...), with state and health (HAProxy healthy = answers on its stats socket; an extension service waiting for a device, like the QEMU guest agent's virtio port, is `waiting`) |
| `ServiceStart` / `Stop` / `Restart` | | ✅ | `haproxy`: start; soft stop (finishes in-flight connections, 10s, then SIGTERM); restart = seamless reload. `janusd`: restart = `Restart`, stop refused (node would be unreachable). Extension services: stop (SIGTERM, then SIGKILL after 10s) keeps them stopped until started again |
| `List` | server | ✅ | Directory listing (optionally recursive), symlinks not followed, per-entry errors inline |
| `Read` | server | ✅ | One file's content; devices refused, and the node's secrets (PKI keys, registration token, ACME account and DNS credentials, any PEM private key) - `internal/api/secretfiles.go` |
| `Copy` | server | ✅ | Tar stream of a file or tree (regular files, directories, symlinks); `/proc` and `/sys` refused (use `Read`); the node's secrets left out, refused when asked for by name |
| `PacketCapture` | server | ✅ | tcpdump-equivalent over gRPC: pcap stream, kernel-side filter - see [packet-capture.md](packet-capture.md) |
| `Probe` | server | ✅ | A network test from the node - DNS, TCP, TLS, HTTP, ping, traceroute - each step streamed and timed; bounded (60 s, 20 echo requests, one bodiless HTTP request, 4 at once), operators - see [connectivity-probe.md](guide/connectivity-probe.md) |
| `MetaWrite` / `MetaDelete` | | ⬜ | META partition key/value entries - Janus has no META partition |
| `GenerateClientConfiguration` | | ✅ | Issue an mTLS client cert (`internal/pki`) named after who it's for (`name`), with `os:admin`, `os:operator` or `os:reader`, valid `ttl_seconds` - one year at most; `LocalCARotate` revokes them all |
| `CertificateRequest` | | ✅ | A new key made on the node for a purpose - a cluster member's certificate (`ClusterService.ClusterSet` brings it back), a state transfer (kept in memory, used once by `StateImportFrom`) - and its signing request: the key never leaves the node (admin) |
| `StateExport` | server | ✅ | What a replacement needs to serve as this node (`internal/statexfer`, [replacing a node](guide/replacing-a-node.md)): a tar, `MANIFEST.json` first (each file's size, mode, SHA-256), then the files under their STATE paths - HAProxy's configuration and history, files, uploaded and ACME certificates, the TLS ticket key ring, the modules' configurations, the network configuration, the Controller registration, the fleet trust; `include_identity`: the CA and keys too. Not the event log, the logs, a pending boot confirmation, a one-time token, a cluster membership. Holds private keys: admin, refused when the Controller relays the call |
| `StateImport` | client | ✅ | Takes a `StateExport` stream onto an unused node (no configuration applied, no certificate or file uploaded - refused otherwise): first message the options (`reuse_identity`, `apply_network`, `no_reboot`), then the stream. Every file checked against the manifest, nothing outside what an export carries; staged on STATE (`import/`, a `ready` marker last) and put in place by janusd as it next starts, before anything reads STATE - the node reboots for that. The network configuration, unless applied, comes back in the answer for the takeover. Admin, refused when the Controller relays the call |
| `StateExportGrant` | | ✅ | Signs a new node's state transfer request (`CertificateRequest` `STATE_TRANSFER`) with this node's own CA: a certificate of ten minutes that opens one `StateExport` - the identity only when granted - and no other RPC, its grant in janusd's memory (a restart ends it). Admin, refused when the Controller relays the call |
| `StateImportFrom` | | ✅ | The new node pulls the state from the old one itself and stages it as `StateImport` does: its own key, the granted certificate, the old node checked by its CA (pinned, whatever address it's reached at). The export never sits on the operator's machine; janusctl carries a request and a certificate. Admin, refused when the Controller relays the call |
| `MetricsConfigGet` | | ✅ | The node's Prometheus exporter settings, whether it's listening and on which addresses - `auto`, the default, is the main interface's ([metrics.md](metrics.md#where-it-listens)) |
| `MetricsConfigSet` | | ✅ | Turn the exporter on/off, move it to another port or address (`auto`, every address, one IP) - applied at once, persisted; a port that can't be bound is refused |
| `ConsoleBudgetGet` | | ✅ | The services' share of the node's console - bytes a second and burst -, the default, the lines left out since janusd started ([log-forwarding.md](guide/log-forwarding.md#the-console)) |
| `ConsoleBudgetSet` | | ✅ | Change it at once and for good (STATE `config/console.json`), down to 0 - nothing of the services' output on the console -, or back to the default |
| `NodeExporterConfigGet` | | ✅ | prometheus-node-exporter's settings - running or not, listen address (`auto` by default: the main interface) and port, collectors - the addresses it listens on, and the collectors it offers ([metrics.md](metrics.md#the-node-exporter)); `FailedPrecondition` without the extension |
| `NodeExporterConfigSet` | | ✅ | Change them: node_exporter restarts with them (or stops), persisted |
| `SysctlList` | | ✅ | The kernel parameters the node shows ([kernel-tuning.md](guide/kernel-tuning.md)): the whitelist HAProxy depends on - each value, Janus's default, the saved one, bounds, effect on HAProxy, risk -, the read-only ones, the CIS benchmark's controls and their compliance, what's on trial - and a value suggested for the node, with its rule and measurements, and the signals it observed by hour ([suggestions](guide/kernel-tuning.md#suggestions)) |
| `SysctlApply` | | ✅ | Change whitelisted parameters (or back to their defaults) on trial: checked against the whitelist, bounds and the node's state, the CIS benchmark audited after the write; reverted by itself unless confirmed; `validate_only` checks only; `FailedPrecondition` off a Janus node |
| `SysctlConfirm` | | ✅ | Save the values on trial - over a connection opened after the apply; every boot applies them, after the baseline and before the CIS values are written again |
| `SysctlCancel` | | ✅ | Put the values from before the trial back at once |
| `SysctlHistory` | | ✅ | Who changed which parameter, when, each value before and after - kept on STATE |

## LifecycleService

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `Install` | server | ✅ | Partition a blank target disk from scratch (GPT + ESP/FAT32 + STATE/ext4, pure Go via go-diskfs) and write a release bundle's rootfs identically to both A/B slots - doesn't reboot anything; optional `controller_address`/`controller_ca_cert` (and `controller_fleet_root_cert`, the Controller's fleet root) write a `controller/` directory onto STATE for the installed node to self-register with on first boot; optional `registration_token` (with `controller_address`: a Controller's enrollment token admits the node without approval, labelled) writes `controller/token`; optional `fleet_root_cert` + `fleet_bundle` (checked first) write `pki/fleet/` - the node trusts that fleet from its first boot, without a Controller ([fleet-without-controller.md](fleet-without-controller.md)) |
| `Upgrade` | server | ✅ | Write a release bundle's rootfs to the inactive A/B slot, switch + reboot - `wait_for_health` auto-reverts if the new slot's HAProxy (real stats-socket check) never comes up healthy in time, or if janusd itself never stays running long enough to check |
| `Rollback` | | ✅ | Switch back to the other A/B slot, reboot |

## HAProxyService

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `GetConfig` | | ✅ | Current `haproxy.cfg` - its secrets masked for a caller who may not apply one, see [haproxy-config.md](haproxy-config.md#who-reads-it) |
| `ApplyConfig` | server | ✅ | Validate (`haproxy -c`) then seamless-reload (`-sf <pid>`); with `version`, a version of the history applied again - going back. Each accepted configuration is kept in the history |
| `ValidateConfig` | | ✅ | Dry-run validation only |
| `ConfigHistory` | | ✅ | The configurations applied: who, when, SHA-256, lines added and removed - figures of the masked texts for a caller who may not apply one, see [haproxy-config.md](haproxy-config.md#the-history-who-applied-what-and-going-back) |
| `ConfigVersion` | | ✅ | One version's text, masked like `GetConfig` |
| `Reload` | | ✅ | Seamless reload of the current config |
| `Stats` | | ✅ | Proxy of the HAProxy stats socket `show stat` (raw CSV) |
| `ShowInfo` | | ✅ | Proxy of `show info` (version/uptime/connections) |
| `BackendList` | | ✅ | Every backend with its servers, addresses and states (from `show stat`, parsed by column name) |
| `ServerSetState` | | ✅ | Runtime enable/drain/maint a backend server |
| `MapList` / `MapGet` / `MapUpdate` | | ✅ | Runtime maps - file-backed only (`map(<path>)` in the running config); upsert is delete-then-add since `set map` doesn't create missing keys |
| `ACLUpdate` | | ✅ | Runtime ACL pattern values - file-backed only (`acl ... -f <path>`), same delete-then-add upsert reasoning |
| `MapLookup` / `ACLLookup` | | ✅ | `get map` / `get acl`: the entry or pattern a value matches, with the configuration's matching method - see [haproxy-runtime.md](guide/haproxy-runtime.md) |
| `TableList` / `TableShow` / `TableClear` | | ✅ | Stick tables (`show table`, `clear table`): a key or up to 4 `data.<type> <op> <value>` filters, 500 entries unless asked (5000 at most); clearing every entry takes `all` |
| `SessionList` / `SessionShutdown` | | ✅ | `show sess` (the runtime API's own session left out, filtered by frontend/backend/client) and `shutdown session` |
| `ErrorsShow` | | ✅ | `show errors`: the last invalid request and response per proxy, the refused byte located, credential headers' values masked - operators |
| `CertificateList` / `Upload` / `Delete` | | ✅ | HAProxy's cert store (`new`/`set`/`commit`/`del ssl cert`), plus optional binding into a `crt-list` already referenced by a `bind ... ssl crt-list <path>` in the running config (`add`/`del ssl crt-list`, with SNI filters) - `CertificateList` reports each cert's `Used`/`Unused` status. Uploaded certificates are kept on STATE and put back into HAProxy after every reload, restart and reboot |
| `FileList` / `FileGet` / `FilePut` / `FileDelete` | | ✅ | HAProxy's own files, `/etc/haproxy/files/<name>`: error pages, maps, certificates `haproxy.cfg` references ([haproxy-files.md](haproxy-files.md)). A change is refused when `haproxy.cfg` wouldn't load with it; a private key is never read back |
| `ACMEStatus` | | ✅ | The letsencrypt extension: the account (thumbprint, registration), the HTTP-01 rule, and each certificate's state, expiry, renewal time and last error ([letsencrypt.md](letsencrypt.md)) |
| `ACMEGetConfig` | | ✅ | The configuration - secrets (DNS provider settings, EAB key) returned empty |
| `ACMEApplyConfig` | | ✅ | Check (`validate_only`) or save it, optionally with an account key to import; certificates are obtained in the background. An empty secret keeps the saved one |
| `ACMERenew` | | ✅ | Obtain the named certificates (or all) now, due or not |
| `ACMECheckPath` | | ✅ | Fetch the CA's directory the way the node reaches it - directly, through a proxy or the Controller's relay, the saved way or the given one - and say how it went ([letsencrypt.md](letsencrypt.md#how-the-node-reaches-lets-encrypt)) |
| `ACMECertificateRequest` | | ✅ | A new key, kept on the node, and its certificate signing request - for the Controller or an orchestrator to order the certificate ([letsencrypt.md](letsencrypt.md#ordered-by-the-controller)) |
| `ACMEInstallCertificate` | | ✅ | Install the certificate issued for the request waiting, swapped into HAProxy without a reload - or keep why it couldn't be obtained |

## NetworkService

The node's own network configuration - see
[network-configuration.md](network-configuration.md):

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `NetworkConfigGet` | | ✅ | The configuration in effect (hostname, interfaces and VLANs, DNS, NTP), and whether it's the default |
| `NetworkConfigApply` | server | ✅ | Applies a configuration **on trial**: reverts by itself unless confirmed in time (30 s default); streams `validating`, `applying`, `awaiting-confirmation` |
| `NetworkConfigConfirm` | | ✅ | Keeps and saves the trial - only over a connection to an address the new configuration keeps |
| `NetworkStatus` | | ✅ | Links, addresses, DHCP lease, routes, resolvers, hostname, clock synchronization |
| `NeighborList` | | ✅ | The kernel's neighbour table - ARP and NDP: each neighbour's MAC, state (reachable, stale, failed...), router flag and ages; one interface's or all |

Optional modules:

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `BGPStatus` | | ✅ | The bird extension: every BIRD protocol's state, BGP sessions (neighbor, AS, last error), routes per channel, and the `haproxy_*` protocols janusd holds down while HAProxy doesn't answer ([bgp.md](bgp.md)) |
| `BGPGetConfig` | | ✅ | The saved bird.conf |
| `BGPApplyConfig` | | ✅ | Check (`validate_only`, by BIRD itself) or apply bird.conf - saved, BIRD reconfigures; empty stops BIRD |
| `VRRPStatus` | | ✅ | The keepalived extension: each VRRP instance's state, interface, priority, virtual IPs, and HAProxy's health as tracked ([vrrp.md](vrrp.md)) |
| `VRRPGetConfig` | | ✅ | The saved keepalived.conf |
| `VRRPApplyConfig` | | ✅ | Check (`validate_only`, by keepalived itself) or apply keepalived.conf - saved, reloaded; empty stops keepalived |
| `FirewallList` | | ✅ | The nftables extension: the live ruleset, whether one is saved, a trial in progress ([firewall.md](firewall.md)) |
| `FirewallGetRuleset` | | ✅ | The saved ruleset |
| `FirewallApplyRuleset` | | ✅ | Check (`validate_only`) or apply a ruleset on trial - reverts unless confirmed |
| `FirewallConfirm` | | ✅ | Keep and save the ruleset on trial - refused over a connection opened before it was applied |
| `FirewallSets` / `FirewallSetUpdate` | | ✅ | Named sets of the live ruleset; add/delete elements live, kept across applies and reboots unless they have a timeout |
| `ConsulStatus` | | ✅ | The consul extension: the agent's service state and, from its HTTP API (with the managed layer's token, if any), its node, role, datacenter, leader, members and services ([consul.md](consul.md)) |
| `ConsulGetConfig` | | ✅ | The saved configuration and its files' names (admin only: it may hold the gossip key and ACL tokens) |
| `ConsulApplyConfig` | | ✅ | Check (`validate_only`, by `consul validate`) or apply a configuration with its files - saved, the agent restarts; empty stops the agent |
| `ConsulManagedGet` | | ✅ | The layer a Controller manages ([consul.md](consul.md#from-the-controller)) - its gossip key and ACL token left out (`has_*`) - and how the registration of the node's own services stands |
| `ConsulManagedSet` | | ✅ | Replace the managed layer (admin): checked by `consul validate` with the operator's configuration, the agent restarts when what it reads changed, the node registers its services with the layer's token; empty removes it and them |

## ClusterService

The node as a member of a cluster ([cluster.md](cluster.md),
`internal/cluster`): the composition an orchestrator sets, the member
certificate (for a key the node made: `SystemService.CertificateRequest`)
and the cluster's CA on STATE under `haproxy/cluster/`, and the `peers
janus` section janusd generates for HAProxy from it - a node in no
cluster included, itself alone, so a reload keeps the stick tables either
way. HAProxy runs with it as a second `-f` and `-L <member name>`.

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `ClusterGet` | | ✅ | What the node knows: the cluster's id and name, its member name, the members and their addresses, the managed domains, the generated peers section, the TLS ticket key ring's state - readers |
| `ClusterSet` | | ✅ | Install or update the membership (admin): the cluster's CA, the member certificate for the key `CertificateRequest` made (or the current one's, to renew it), the members. The section is regenerated and HAProxy reloaded; a certificate alone is swapped at runtime. A HAProxy that refuses the section gets the previous membership back. The member's own address must be one of the node's. `advisory`: an admin's certificate sending the cluster's id writes through the locks too (`janusctl cluster`, no Controller) |
| `ClusterLeave` | | ✅ | Forget the membership (admin): the section names the node alone again, the member certificate and CA are deleted |
| `ClusterRelease` | | ✅ | Let this member's own users change domains the cluster manages again - `haproxy`, `files`, `vrrp`, `bgp`, `firewall`, none named: all - for an emergency on one node; the cluster reports it as drifted until its next apply (admin). While a domain is managed, its writes (`ApplyConfig`, `FilePut`/`FileDelete`, `CertificateUpload`/`CertificateDelete`, `VRRPApplyConfig`, `BGPApplyConfig`, `FirewallApplyRuleset` - validate-only calls aside) are refused `FailedPrecondition` unless the Controller's certificate makes them for that cluster (`janus-cluster-apply`) |
| `ClusterStatus` | | ✅ | HAProxy's `show peers`: each peer connected or not, its last handshake, and per stick table whether the peer acknowledged every local update; `tables_learnt` once HAProxy has its tables back after a start or reload (an orchestrator waits for it before reloading the next member) - readers |
| `ClusterDrain` | server | ✅ | Out of service without stopping (admin): HAProxy reads as unhealthy to keepalived and BIRD - the virtual IPs move, the `haproxy_*` routes are withdrawn - while it keeps serving; streams until VRRP shows BACKUP and BGP the routes gone, or the timeout. Holds until `ClusterUndrain` or `ClusterLeave`, across reboots: kept on STATE, put back before keepalived or BIRD start |
| `ClusterUndrain` | | ✅ | Back in service (admin) |
| `ClusterPull` | server | ✅ | Fetch another member's secrets over mTLS - the member certificates on both ends, the cluster's CA vouching for each: HAProxy's uploaded certificates, its files, the letsencrypt extension's certificates, the TLS ticket key ring - and install them (admin). Names in the progress, never content |
| `ClusterExport` | server | ✅ | Serves `ClusterPull`: a tar of the items. Only a member certificate of this node's own cluster opens it - no user role does |
| `TLSTicketKeysRotate` | | ✅ | Add a key to the TLS ticket key ring (`haproxy/cluster/tls-ticket-keys`, three keys, made at first boot) and hand it to HAProxy without a reload (`set ssl tls-key`); the other members pull the ring afterwards, so a session resumes on any of them (admin) |

## Deliberately not present

- `EtcdService` - Janus nodes don't form an etcd cluster.
- `ImageService`/container runtime RPCs - no container runtime on the
  target OS; HAProxy and the optional daemons are native processes
  supervised by the custom PID 1 (`rootfs/init`, Phase 1).
