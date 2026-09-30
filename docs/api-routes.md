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
additional client certificates once you already have one.

**Every RPC is also role-checked** against the caller's certificate
(`internal/api/authz.go`, fail-closed - an RPC with no explicit entry
defaults to admin-only). Two roles: `os:admin` (everything) and
`os:reader` (the ✅ methods marked "read-only" in the tables below, plus
`Version`/`Hostname`/`Events`/`GetConfig`/`ValidateConfig`/`*Status`/
`*List`/`*Get` - status and observability RPCs only, not file/log/
packet-capture access or credential issuance, even though some of those
are also technically non-mutating).

## SystemService

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `Version` | | ✅ | Daemon version, Go version, kernel version, active A/B slot, architecture, image schematic ID and extensions ([image-factory.md](image-factory.md)) - connectivity check |
| `Hostname` | | ✅ | `janusctl system hostname` |
| `Reboot` | | ✅ | Soft-stops HAProxy (in-flight connections get 5s), syncs, reboots - both modes are a full firmware reboot (no kexec) |
| `Shutdown` | | ✅ | Same graceful stop, then powers off |
| `Restart` | | ✅ | Restarts `janusd` only: rootfs/init starts it again and it takes the running HAProxy over with a seamless reload - HAProxy keeps serving throughout. Refused when `janusd` is PID 1 (e.g. `local-dev`), where nothing would start it again |
| `Reset` | | ✅ | `wipe_state` empties the persistent STATE partition (PKI, applied config, Controller registration, pending boot confirmation) and reboots: a new CA/admin certificate is printed on the console, current certificates stop working. A/B slots and ESP untouched. `wipe_ephemeral` alone just reboots (everything ephemeral is tmpfs) |
| `ApplyConfiguration` | server | ⬜ | Apply declarative config (`internal/config`), auto/no-reboot/reboot/try modes - no declarative machine config model exists yet |
| `Events` | server | ✅ | In-memory event log (`internal/events`, last 1000): janusd start, HAProxy started/exited (with why: reload, stop, or on its own), config applied/rejected, reloads, server state, maps/ACLs/certificates, services, upgrades/rollbacks/installs, boot confirmation/revert, self-registration, packet captures, reboot/shutdown/restart/reset. Backlog after `since_id`, then follows. Restarts empty with janusd |
| `Dmesg` | server | ✅ | `/dev/kmsg`, formatted like `dmesg`; `follow` keeps streaming |
| `Logs` | server | ✅ | `janusd` (its log, captured after the one-time PKI print so no private key is kept) or `haproxy` (stdout/stderr) - last 5000 lines in memory, `tail_lines`, `follow` |
| `Stats` | | ✅ | CPU (lifetime average, like `ps`) and RSS of `janusd` and `haproxy`, summed over processes |
| `SystemStat` | | ✅ | `/proc/stat`: boot time, context switches, processes created |
| `Memory` | | ✅ | `/proc/meminfo` |
| `CPUInfo` | | ✅ | `/proc/cpuinfo` |
| `LoadAvg` | | ✅ | `/proc/loadavg` |
| `DiskStats` | | ✅ | `/proc/diskstats` |
| `DiskUsage` | server | ✅ | Apparent size of each path; `recursive` adds one entry per directory (deepest first). Doesn't descend into `/proc`, `/sys`, `/dev` |
| `NetworkDeviceStats` | | ✅ | `/proc/net/dev`: bytes and errors per interface |
| `Netstat` | | ✅ | `/proc/net/{tcp,tcp6,udp,udp6}` - IPv4-mapped addresses shown as IPv4 |
| `Mounts` | | ✅ | `/proc/self/mounts` + `statfs` sizes |
| `Processes` | | ✅ | Every process: pid, command line, CPU, RSS |
| `ServiceList` | | ✅ | `janusd`, `haproxy` and the services of the image's extensions (`node-exporter`, `qemu-guest-agent`...), with state and health (HAProxy healthy = answers on its stats socket; an extension service waiting for a device, like the QEMU guest agent's virtio port, is `waiting`) |
| `ServiceStart` / `Stop` / `Restart` | | ✅ | `haproxy`: start; soft stop (finishes in-flight connections, 10s, then SIGTERM); restart = seamless reload. `janusd`: restart = `Restart`, stop refused (node would be unreachable). Extension services: stop (SIGTERM, then SIGKILL after 10s) keeps them stopped until started again |
| `List` | server | ✅ | Directory listing (optionally recursive), symlinks not followed, per-entry errors inline |
| `Read` | server | ✅ | One file's content; devices refused |
| `Copy` | server | ✅ | Tar stream of a file or tree (regular files, directories, symlinks); `/proc` and `/sys` refused (use `Read`) |
| `PacketCapture` | server | ✅ | tcpdump-equivalent over gRPC: pcap stream, kernel-side filter - see [packet-capture.md](packet-capture.md) |
| `MetaWrite` / `MetaDelete` | | ⬜ | META partition key/value entries - Janus has no META partition |
| `GenerateClientConfiguration` | | ✅ | Issue an mTLS client cert (`internal/pki`) - 1 year validity, no rotation flow yet |

## LifecycleService

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `Install` | server | ✅ | Partition a blank target disk from scratch (GPT + ESP/FAT32 + STATE/ext4, pure Go via go-diskfs) and write a release bundle's rootfs identically to both A/B slots - doesn't reboot anything; optional `controller_address`/`controller_ca_cert` write a `controller/` directory onto STATE for the installed node to self-register with on first boot (Point 2 suite tranche 4 - janusd reading it back isn't built yet, tranche 5) |
| `Upgrade` | server | ✅ | Write a release bundle's rootfs to the inactive A/B slot, switch + reboot - `wait_for_health` auto-reverts if the new slot's HAProxy (real stats-socket check) never comes up healthy in time, or if janusd itself never stays running long enough to check |
| `Rollback` | | ✅ | Switch back to the other A/B slot, reboot |

## HAProxyService

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `GetConfig` | | ✅ | Current `haproxy.cfg` |
| `ApplyConfig` | server | ✅ | Validate (`haproxy -c`) then seamless-reload (`-sf <pid>`) |
| `ValidateConfig` | | ✅ | Dry-run validation only |
| `Reload` | | ✅ | Seamless reload of the current config |
| `Stats` | | ✅ | Proxy of the HAProxy stats socket `show stat` (raw CSV) |
| `ShowInfo` | | ✅ | Proxy of `show info` (version/uptime/connections) |
| `BackendList` | | ✅ | Every backend with its servers, addresses and states (from `show stat`, parsed by column name) |
| `ServerSetState` | | ✅ | Runtime enable/drain/maint a backend server |
| `MapList` / `MapGet` / `MapUpdate` | | ✅ | Runtime maps - file-backed only (`map(<path>)` in the running config); upsert is delete-then-add since `set map` doesn't create missing keys |
| `ACLUpdate` | | ✅ | Runtime ACL pattern values - file-backed only (`acl ... -f <path>`), same delete-then-add upsert reasoning |
| `CertificateList` / `Upload` / `Delete` | | ✅ | HAProxy's cert store (`new`/`set`/`commit`/`del ssl cert`), plus optional binding into a `crt-list` already referenced by a `bind ... ssl crt-list <path>` in the running config (`add`/`del ssl crt-list`, with SNI filters) - `CertificateList` reports each cert's `Used`/`Unused` status |

## NetworkService

The node's own network configuration - see
[network-configuration.md](network-configuration.md):

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `NetworkConfigGet` | | ✅ | The configuration in effect (hostname, interfaces and VLANs, DNS, NTP), and whether it's the default |
| `NetworkConfigApply` | server | ✅ | Applies a configuration **on trial**: reverts by itself unless confirmed in time (30 s default); streams `validating`, `applying`, `awaiting-confirmation` |
| `NetworkConfigConfirm` | | ✅ | Keeps and saves the trial - only over a connection to an address the new configuration keeps |
| `NetworkStatus` | | ✅ | Links, addresses, boot DHCP lease, routes, resolvers, hostname, clock synchronization |

Optional modules:

| Method | Streaming | Status | Purpose |
|---|---|---|---|
| `BGPStatus` / `BGPApplyConfig` | | ✅ (not-enabled) | bird - `MODULE_STATE_NOT_ENABLED` and apply refused (`FailedPrecondition`) when bird isn't in the image, which is every image today; answers `Unimplemented` if the binary is present, since its management isn't built yet |
| `VRRPStatus` / `VRRPApplyConfig` | | ✅ (not-enabled) | keepalived, same convention |
| `FirewallList` / `FirewallApplyRuleset` | | ✅ (not-enabled) | nftables, same convention |

## Deliberately not present

- `EtcdService` - Janus nodes don't form an etcd cluster.
- `ImageService`/container runtime RPCs - no container runtime on the
  target OS; HAProxy and the optional daemons are native processes
  supervised by the custom PID 1 (`rootfs/init`, Phase 1).
