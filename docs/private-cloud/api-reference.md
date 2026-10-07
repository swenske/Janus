<!-- Generated from api/proto and internal/rbac: go test ./internal/rbac -run TestAPIReferenceDoc -update -->

# Node API reference

Every call of a Janus node's gRPC API, the role and the domain it needs,
and what it does - generated from the .proto files and the roles the
node enforces. The messages' fields are in the .proto files themselves
(`api/proto/janus/v1alpha1`, at the release your nodes run).

- **Role**: the least role that may make the call - `os:operator` may do
  what `os:reader` may, `os:admin` everything
  ([certificates and roles](orchestrator-certificates.md#roles)).
- **Domain**: what the call is about, for a permission narrowed to some
  domains (`janus-as-domains`); `observe` comes with any.
- A call the role or the domain doesn't allow answers `PermissionDenied`;
  a call to an extension the image doesn't have, `FailedPrecondition`.
- Every call is `janus.v1alpha1.<Service>/<Call>` - `janus.v1alpha1.HAProxyService/ApplyConfig`.

## HAProxyService

HAProxyService configures and drives the node's HAProxy - what editing haproxy.cfg over SSH would be elsewhere: its configuration, its runtime state (servers, maps, ACLs, certificates) over its stats socket, its files, and the letsencrypt extension's certificates.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `GetConfig` | `Empty` → `GetConfigResponse` | `os:reader` | haproxy | GetConfig returns the applied haproxy.cfg and its SHA-256. |
| `ApplyConfig` | `ApplyConfigRequest` → `stream ApplyConfigResponse` | `os:operator` | haproxy | ApplyConfig validates the given config via `haproxy -c` before reloading - a rejected config never reaches the running process. The stages stream: "validating", then "reloading" and "done" (accepted), or "rejected" with HAProxy's errors. The configuration is kept across reboots and updates. |
| `ValidateConfig` | `ValidateConfigRequest` → `ValidateConfigResponse` | `os:reader` | haproxy | ValidateConfig runs the same validation as ApplyConfig without touching the running process (dry-run). |
| `Reload` | `Empty` → `ReloadResponse` | `os:operator` | haproxy | Reload triggers a seamless (zero-downtime) reload of the currently applied config. |
| `Stats` | `Empty` → `HAProxyStatsResponse` | `os:reader` | observe | Stats proxies HAProxy's own stats socket ("show stat"). |
| `ShowInfo` | `Empty` → `ShowInfoResponse` | `os:reader` | observe | ShowInfo proxies the stats socket's "show info". |
| `BackendList` | `Empty` → `BackendListResponse` | `os:reader` | observe | BackendList lists every backend with each server's address and state ("up", "down", "maint", "drain"...). |
| `ServerSetState` | `ServerSetStateRequest` → `Empty` | `os:operator` | haproxy | ServerSetState enables/disables/drains a single backend server at runtime (stats socket "set server ... state ..."). |

Runtime maps and ACL lists (stats socket "show/add/del map", "add/del acl"): only those haproxy.cfg loads from a file. A change is made in the running HAProxy's memory - the file, and so the next reload, keep the old content (FilePut changes the file).

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `MapList` | `Empty` → `MapListResponse` | `os:reader` | haproxy | MapList lists the maps the running HAProxy loaded. |
| `MapGet` | `MapGetRequest` → `MapGetResponse` | `os:reader` | haproxy | MapGet returns a map's entries. |
| `MapUpdate` | `MapUpdateRequest` → `Empty` | `os:operator` | haproxy | MapUpdate adds, replaces or deletes one entry of a map. |
| `ACLUpdate` | `ACLUpdateRequest` → `Empty` | `os:operator` | haproxy | ACLUpdate adds or deletes one value of an ACL list. |
| `CertificateList` | `Empty` → `CertificateListResponse` | `os:reader` | haproxy | CertificateList lists the certificates the running HAProxy holds, with their expiry. |
| `CertificateUpload` | `CertificateUploadRequest` → `Empty` | `os:operator` | haproxy | CertificateUpload loads a certificate into the running HAProxy and keeps it on the node (STATE): janusd puts it back - crt-list binding included - into every new HAProxy process, after a reload, a restart or a reboot. |
| `CertificateDelete` | `CertificateDeleteRequest` → `Empty` | `os:operator` | haproxy | CertificateDelete removes an uploaded certificate from the running HAProxy and from the node, unbinding it from crt_list first. |

HAProxy's own files ([docs/haproxy-files.md](../haproxy-files.md)): what haproxy.cfg references besides the letsencrypt extension's certificates - error pages, maps, ACL lists, Lua, other certificates - as /etc/haproxy/files/&lt;name&gt;, kept on the node. A file is only written or removed if haproxy.cfg still loads with the change; a file holding a private key is never read back.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `FileList` | `Empty` → `FileListResponse` | `os:reader` | haproxy | FileList lists the files, with their size and SHA-256. |
| `FileGet` | `FileGetRequest` → `FileGetResponse` | `os:reader` | haproxy | FileGet returns a file's content - refused for one holding a private key. |
| `FilePut` | `FilePutRequest` → `FilePutResponse` | `os:operator` | haproxy | FilePut writes a file, and reloads HAProxy with it if asked. |
| `FileDelete` | `FileDeleteRequest` → `FileDeleteResponse` | `os:operator` | haproxy | FileDelete removes a file - refused while haproxy.cfg needs it. |

ACME: the letsencrypt extension ([docs/letsencrypt.md](../letsencrypt.md)). The node obtains and renews its certificates itself, from Let's Encrypt or any ACME CA, writes each to /etc/haproxy/acme/&lt;name&gt;.pem and swaps a renewed one into the running HAProxy without a reload. Without the extension in the image, ACMEStatus says so and the other calls answer FailedPrecondition.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `ACMEStatus` | `Empty` → `ACMEStatusResponse` | `os:reader` | haproxy | ACMEStatus reports each certificate's state: obtained, its expiry, its next renewal, the last error. |
| `ACMEGetConfig` | `Empty` → `ACMEGetConfigResponse` | `os:reader` | haproxy | ACMEGetConfig returns the configuration - never a secret (a DNS provider's settings, the EAB key): their values come back empty. |
| `ACMEApplyConfig` | `ACMEApplyConfigRequest` → `ACMEApplyConfigResponse` | `os:operator` | haproxy | ACMEApplyConfig replaces the configuration; an empty secret keeps the saved one. |
| `ACMERenew` | `ACMERenewRequest` → `ACMERenewResponse` | `os:operator` | haproxy | ACMERenew obtains the named certificates (every one if none is named) now, whether or not they're due, in the background: ACMEStatus says how it went. |

## NetworkService

NetworkService configures the node's network - always available - and its optional network extensions: BGP (bird), VRRP (keepalived), the firewall (nftables), Consul. An extension is chosen when the image is built ([docs/image-factory.md](../image-factory.md)): without it, its binary isn't in the image at all, and its calls say so (MODULE_STATE_NOT_ENABLED, FailedPrecondition).

BGP: the bird extension ([docs/bgp.md](../bgp.md)). The protocols named haproxy_* are kept down while this node's HAProxy doesn't answer. Without the extension in the image, BGPStatus says so and the other calls answer FailedPrecondition - as for VRRP, the firewall and Consul below.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `BGPStatus` | `Empty` → `BGPStatusResponse` | `os:reader` | observe | BGPStatus reads every protocol's state over BIRD's control socket. |
| `BGPGetConfig` | `Empty` → `BGPGetConfigResponse` | `os:operator` | network | BGPGetConfig returns the saved bird.conf. |
| `BGPApplyConfig` | `BGPApplyConfigRequest` → `BGPApplyConfigResponse` | `os:admin` | network | BGPApplyConfig has BIRD check a bird.conf, saves it, and BIRD reconfigures. |

VRRP: the keepalived extension ([docs/vrrp.md](../vrrp.md)).

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `VRRPStatus` | `Empty` → `VRRPStatusResponse` | `os:reader` | observe | VRRPStatus reads keepalived's own state of each instance. |
| `VRRPGetConfig` | `Empty` → `VRRPGetConfigResponse` | `os:operator` | network | VRRPGetConfig returns the saved keepalived.conf. |
| `VRRPApplyConfig` | `VRRPApplyConfigRequest` → `VRRPApplyConfigResponse` | `os:admin` | network | VRRPApplyConfig has keepalived check a keepalived.conf, saves it, and keepalived reloads. |

Firewall: the nftables extension ([docs/firewall.md](../firewall.md)). The ruleset is the node's whole nftables ruleset, in nft's own syntax.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `FirewallList` | `Empty` → `FirewallListResponse` | `os:reader` | observe | FirewallList reports the module and the live ruleset. |
| `FirewallGetRuleset` | `Empty` → `FirewallGetRulesetResponse` | `os:reader` | network | FirewallGetRuleset returns the saved ruleset. |
| `FirewallApplyRuleset` | `FirewallApplyRulesetRequest` → `FirewallApplyRulesetResponse` | `os:admin` | network | FirewallApplyRuleset validates and applies a ruleset on trial: unless FirewallConfirm comes within the timeout, the previous one is put back. Confirmed, it's saved and applied at every boot. |
| `FirewallConfirm` | `Empty` → `FirewallConfirmResponse` | `os:admin` | network | FirewallConfirm keeps the ruleset on trial. It must come over a connection opened after the apply - established connections are kept whatever the ruleset. |
| `FirewallSets` | `Empty` → `FirewallSetsResponse` | `os:reader` | network | FirewallSets lists the live ruleset's named sets and their elements. |
| `FirewallSetUpdate` | `FirewallSetUpdateRequest` → `FirewallSetUpdateResponse` | `os:admin` | network | FirewallSetUpdate adds and deletes elements of a named set without reloading the ruleset - elements added without a timeout are kept across applies and reboots. |

Consul: the consul extension ([docs/consul.md](../consul.md)). The agent runs with the operator's own configuration and the files it names (TLS certificates, keys...) under /run/janus/consul/files.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `ConsulStatus` | `Empty` → `ConsulStatusResponse` | `os:reader` | observe | ConsulStatus reports the agent's service and what the agent says of itself. |
| `ConsulGetConfig` | `Empty` → `ConsulGetConfigResponse` | `os:admin` | network | ConsulGetConfig returns the configuration and the files' names, never their content. |
| `ConsulApplyConfig` | `ConsulApplyConfigRequest` → `ConsulApplyConfigResponse` | `os:admin` | network | ConsulApplyConfig has `consul validate` check a configuration, saves it with its files, and restarts the agent. |

The node's own network configuration: hostname, interfaces (physical and 802.1Q VLANs, DHCP or static), DNS and NTP. Unlike the optional modules above, always available.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `NetworkConfigGet` | `Empty` → `NetworkConfigGetResponse` | `os:reader` | network | NetworkConfigGet returns the saved network configuration. |
| `NetworkConfigApply` | `NetworkConfigApplyRequest` → `stream NetworkConfigApplyResponse` | `os:admin` | network | NetworkConfigApply applies a configuration on trial: the node switches to it at once, but keeps it only if NetworkConfigConfirm arrives within the confirm window - otherwise it reverts to the previous configuration by itself. A configuration that cuts the caller off therefore undoes itself. Only a confirmed configuration is written to persistent storage, so a reboot during the trial also comes back on the previous one. The stream ends once the configuration is applied and awaiting confirmation. |
| `NetworkConfigConfirm` | `Empty` → `NetworkConfigConfirmResponse` | `os:admin` | network | NetworkConfigConfirm confirms the configuration on trial. Accepted only over a connection that reaches the node on an address the new configuration keeps - proof that it's still reachable - and refused over one whose local address the trial removed. |
| `NetworkStatus` | `Empty` → `NetworkStatusResponse` | `os:reader` | observe | NetworkStatus reports what's actually in effect: links, addresses, DHCP leases, routes, resolvers, hostname and clock synchronization. |

## SystemService

SystemService is the reduced, non-Kubernetes equivalent of Talos's MachineService: the machine's power, observability, managed-service control, and the handful of read-only file/network RPCs that replace an interactive shell. See [docs/api-routes.md](../api-routes.md) for the full design rationale. Every method is implemented but ApplyConfiguration, MetaWrite and MetaDelete, which return codes.Unimplemented.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `Version` | `Empty` → `VersionResponse` | `os:reader` | observe | Version reports what the node runs: Janus's version, its kernel, the A/B slot it booted from and its image schematic. |
| `Hostname` | `Empty` → `HostnameResponse` | `os:reader` | observe | Hostname reports the node's hostname. |
| `Reboot` | `RebootRequest` → `RebootResponse` | `os:operator` | services | Reboot power-cycles the whole machine, after a soft stop of HAProxy: connections in flight get a chance to finish. |
| `Shutdown` | `Empty` → `ShutdownResponse` | `os:operator` | services | Shutdown powers the machine off, after the same soft stop of HAProxy. |
| `Restart` | `Empty` → `RestartResponse` | `os:operator` | services | Restart restarts the janusd control-plane process in place, without rebooting the machine or interrupting HAProxy itself. |
| `Reset` | `ResetRequest` → `ResetResponse` | `os:admin` | system | Reset wipes the requested partitions (state/ephemeral) and reboots - the equivalent of returning the node to its just-installed state. |
| `ApplyConfiguration` | `ApplyConfigurationRequest` → `stream ApplyConfigurationResponse` | `os:admin` | system | ApplyConfiguration is not implemented - it answers Unimplemented: a node has no declarative machine configuration, each area has its own calls (HAProxyService, NetworkService...). |
| `Events` | `EventsRequest` → `stream Event` | `os:reader` | observe | Events streams the machine's internal event log (config applied, service state changes, upgrade progress, etc.). |
| `Dmesg` | `DmesgRequest` → `stream Data` | `os:admin` | system | Dmesg streams the kernel ring buffer. |
| `Logs` | `LogsRequest` → `stream Data` | `os:operator` | services | Logs streams a managed service's log output. |
| `Stats` | `Empty` → `StatsResponse` | `os:reader` | observe | Stats sums the CPU time and memory of each managed service's processes - janusd, haproxy (a reload briefly leaves an old haproxy process finishing its connections next to the new one). |
| `SystemStat` | `Empty` → `SystemStatResponse` | `os:reader` | observe | SystemStat reports the machine's counters since boot: boot time, context switches, processes created, CPU ticks (/proc/stat). |
| `Memory` | `Empty` → `MemoryResponse` | `os:reader` | observe | Memory reports the machine's total, available and cached memory. |
| `CPUInfo` | `Empty` → `CPUInfoResponse` | `os:reader` | observe | CPUInfo describes the machine's CPUs: model, frequency, sockets and cores. |
| `LoadAvg` | `Empty` → `LoadAvgResponse` | `os:reader` | observe | LoadAvg reports the load averages over 1, 5 and 15 minutes. |
| `DiskStats` | `Empty` → `DiskStatsResponse` | `os:reader` | observe | DiskStats reports each disk's I/O counters. |
| `DiskUsage` | `DiskUsageRequest` → `stream DiskUsageInfo` | `os:admin` | system | DiskUsage reports each requested path's total size (apparent size of every regular file under it, not crossing into /proc, /sys or /dev); with recursive, also one entry per directory beneath it. |
| `NetworkDeviceStats` | `Empty` → `NetworkDeviceStatsResponse` | `os:reader` | observe | NetworkDeviceStats reports each network interface's traffic and error counters. |
| `Netstat` | `Empty` → `NetstatResponse` | `os:reader` | observe | Netstat lists the machine's TCP and UDP sockets: listening ones and connections. |
| `Mounts` | `Empty` → `MountsResponse` | `os:reader` | observe | Mounts lists the mounted filesystems with their size and free space. |
| `Processes` | `Empty` → `ProcessesResponse` | `os:reader` | observe | Processes lists the machine's processes. |
| `ServiceList` | `Empty` → `ServiceListResponse` | `os:reader` | observe | ServiceList reports the managed services: janusd itself, haproxy, and the services of the image's optional extensions. |
| `ServiceStart` | `ServiceRequest` → `ServiceResponse` | `os:operator` | services | ServiceStart starts a managed service. NotFound for a service the image doesn't have; FailedPrecondition for an extension's service its settings disable. |
| `ServiceStop` | `ServiceRequest` → `ServiceResponse` | `os:operator` | services | ServiceStop stops a managed service - haproxy after a soft stop; never janusd (FailedPrecondition: Restart restarts it). |
| `ServiceRestart` | `ServiceRequest` → `ServiceResponse` | `os:operator` | services | ServiceRestart restarts a managed service. |

List, Read, Copy and PacketCapture are the deliberate, narrow replacements for an interactive shell: read-only, scoped, never arbitrary command execution. None of them serves the node's secrets.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `List` | `ListRequest` → `stream FileInfo` | `os:admin` | system | List walks a directory, never into /proc, /sys or /dev. |
| `Read` | `ReadRequest` → `stream Data` | `os:admin` | system | Read streams a file's content - never a device's. |
| `Copy` | `CopyRequest` → `stream Data` | `os:admin` | system | Copy streams a file or a directory as a tar archive - never from /proc or /sys. |
| `PacketCapture` | `PacketCaptureRequest` → `stream Data` | `os:admin` | system | PacketCapture streams a live capture as a pcap file (classic libpcap format, microsecond timestamps), split across Data messages - concatenate them to get a file tcpdump/Wireshark read directly. Runs for duration_seconds, or until the client cancels. |
| `MetaWrite` | `MetaWriteRequest` → `Empty` | `os:admin` | system | MetaWrite is not implemented - it answers Unimplemented: a node has no META partition for small key/value entries. |
| `MetaDelete` | `MetaDeleteRequest` → `Empty` | `os:admin` | system | MetaDelete is not implemented - it answers Unimplemented, like MetaWrite. |
| `GenerateClientConfiguration` | `GenerateClientConfigurationRequest` → `GenerateClientConfigurationResponse` | `os:admin` | system | GenerateClientConfiguration issues a client certificate signed by this node's CA (see internal/pki), for the given roles, named after who it's for and valid one year or less. |

The node's Prometheus exporter ([docs/metrics.md](../metrics.md)): Janus's own metrics - certificate expiry, boot slot, HAProxy as janusd runs it, extension services, time sync, SELinux - over plain HTTP, on by default on port 10056, on every address unless one is set.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `MetricsConfigGet` | `Empty` → `MetricsConfigResponse` | `os:reader` | observe | MetricsConfigGet reports the exporter's settings. |
| `MetricsConfigSet` | `MetricsConfig` → `MetricsConfigResponse` | `os:admin` | system | MetricsConfigSet changes the exporter's settings, at once and for good; a port that can't be bound is refused and the exporter stays as it was. |

The prometheus-node-exporter extension's settings ([docs/metrics.md](../metrics.md)): whether node_exporter runs, the address and port it listens on, and its collectors, from a fixed list. FailedPrecondition when the image doesn't have the extension.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `NodeExporterConfigGet` | `Empty` → `NodeExporterConfigResponse` | `os:reader` | observe | NodeExporterConfigGet reports node_exporter's settings. |
| `NodeExporterConfigSet` | `NodeExporterConfig` → `NodeExporterConfigResponse` | `os:admin` | system | NodeExporterConfigSet changes node_exporter's settings: it restarts with them, and they're kept. |

The node's kernel parameters ([docs/guide/kernel-tuning.md](../guide/kernel-tuning.md)): a whitelist of the ones HAProxy depends on, which an operator may change - on trial, kept once confirmed, back to Janus's defaults on request - and the CIS benchmark's, which no call changes. Changes answer FailedPrecondition unless janusd runs a Janus node.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `SysctlList` | `Empty` → `SysctlListResponse` | `os:reader` | observe | SysctlList reports every parameter the node shows - its value, Janus's default, the saved one, its bounds, what it does to HAProxy, its risks and a value suggested for this node -, the CIS benchmark's controls, what's on trial, and what the node observed for its suggestions. |
| `SysctlApply` | `SysctlApplyRequest` → `SysctlApplyResponse` | `os:admin` | system | SysctlApply checks changes against the whitelist, each parameter's bounds and the node's state, and applies them on trial: unless SysctlConfirm comes within the timeout, the values from before the trial come back by themselves. A trial already running grows by these changes. With validate_only, it only checks them. |
| `SysctlConfirm` | `Empty` → `SysctlTrialResponse` | `os:admin` | system | SysctlConfirm saves the values on trial, which every boot then applies. It must come over a connection opened after the latest SysctlApply - proof that the node still takes new connections. |
| `SysctlCancel` | `Empty` → `SysctlTrialResponse` | `os:admin` | system | SysctlCancel puts the values from before the trial back at once. |
| `SysctlHistory` | `SysctlHistoryRequest` → `SysctlHistoryResponse` | `os:reader` | observe | SysctlHistory returns the changes made to the parameters, newest first: who, when, and each value before and after. |

## LifecycleService

LifecycleService handles installation, upgrade and rollback: writing a new immutable image to the inactive A/B slot, switching the bootloader to it, and rolling back automatically if the new slot doesn't become healthy within its grace period. See [docs/architecture.md](../architecture.md) for the A/B partition layout this is built on.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `Install` | `InstallRequest` → `stream InstallResponse` | `os:admin` | system | Install writes an image to a blank disk for the first time (bare metal / fresh VM) - partitioning it from scratch and writing identical content to both A/B slots, since there's no "other slot" yet to leave untouched. Streams progress; never reboots anything, since the disk it just wrote isn't necessarily the one this node itself is running from. |
| `Upgrade` | `UpgradeRequest` → `stream UpgradeResponse` | `os:admin` | system | Upgrade writes a new image to the currently-inactive A/B slot, switches the bootloader default, and reboots. If wait_for_health is true, the *next* boot has to confirm itself healthy within health_timeout_seconds or the node reverts and reboots back to the slot that was active before this call - autonomously, driven by the node itself, not by this RPC: the stream (and the connection it rides on) ends once the first reboot happens, well before any confirmation or possible revert, so neither is ever visible as a stream message here. |
| `Rollback` | `Empty` → `RollbackResponse` | `os:admin` | system | Rollback switches the bootloader default back to the other A/B slot and reboots, without needing a new image. |
| `UploadReleaseFile` | `stream UploadReleaseFileRequest` → `UploadReleaseFileResponse` | `os:admin` | system | UploadReleaseFile streams one release-bundle file (one of "rootfs.squashfs", "rootfs.verity", "uki-a.efi", "uki-b.efi" - the fixed set image/release/assemble.sh produces) to a local staging area on this node, for network topologies where the node can't dial out to fetch a bundle itself (see UpgradeRequest.source's own doc comment for the alternative, node-initiated http(s):// fetch mode). Call once per file the upcoming Upgrade call will need, then call Upgrade itself with source.reference set to the returned staging_dir. Never triggers an upgrade by itself - purely a file transfer. |

## AccessService

AccessService: whom a node trusts besides its own certificate authority - its fleet (internal/pki/fleet.go). A node pins its fleet's root once; afterwards it only takes a bundle that root signed, newer than its own, listing the issuing CAs whose client certificates it accepts. Its own CA - the first-boot admin certificate - always lets in.

| Call | Request → response | Role | Domain | What it does |
|---|---|---|---|---|
| `TrustGet` | `Empty` → `TrustState` | `os:reader` | observe | The fleet the node trusts: its root, its bundle. Empty when none. |
| `TrustSet` | `TrustSetRequest` → `TrustState` | `os:admin` | system | Pins root_cert (the first time; afterwards it must be the same, or left empty) and applies bundle - signed by that root, newer than the node's. The same bundle again changes nothing. |
| `TrustReset` | `Empty` → `TrustState` | `os:admin` | system | Forgets the fleet: its certificates no longer let anyone in. Only a certificate of the node's own CA may do it - the way back when a fleet is lost, never for the fleet itself. |
| `LocalCARotate` | `LocalCARotateRequest` → `LocalCARotateResponse` | `os:admin` | system | Replaces the node's own CA: a new CA, server certificate and admin certificate. Every certificate the old CA issued stops working - the first-boot admin one, a Controller's service credential, those GenerateClientConfiguration issued; the fleet's let in as before. With admin_public_key (PKIX, PEM: ECDSA P-256/P-384, Ed25519 or RSA 2048+), the new admin certificate is issued for it, its key never seen by the node; without, the node makes the key and prints both on its console, like at first boot. Clients must verify the node with the new CA (ca_cert) from now on. |
