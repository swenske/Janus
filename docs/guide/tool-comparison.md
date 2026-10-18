# Comparing the tools

A Janus node does everything through one API: gRPC over mutual TLS, on
port 9505. `janusctl`, its live dashboard `janusctl tui`, the Controller
and the Terraform provider each cover part of that API, and add what a
single node can't do: many nodes at once, accounts, virtual machines.
This page shows, call by call, which tool does what.

## At a glance

| | janusctl | janusctl tui | Controller | Terraform |
|---|---|---|---|---|
| What it is | A command line: one command per call, plain text to script | A live dashboard in the terminal, for one node or a whole fleet | The web UI of a fleet: accounts and roles, the virtual machines it creates | The `janus` provider for Terraform and OpenTofu: nodes as code |
| How it reaches a node | Directly, port 9505, with a certificate from the Controller, the fleet, or the node's own CA | Like janusctl: the same contexts and certificates | Directly, with its fleet certificate (or the credential a node was added with), acting for the account signed in | Through the Controller's API, with an API token - never the node itself |
| Without a Controller | Yes: a node's own certificate, or [a fleet without a Controller](../fleet-without-controller.md) | Yes | - | No: it drives a Controller |
| Covers | Nearly every call | Watching, and a few actions | Nearly every call | The node's VM, network, release and haproxy.cfg |
| More | [Using janusctl](../janusctl.md), [reference](janusctl-reference.md) | [The dashboard](../janusctl.md#the-dashboard-janusctl-tui) | [The Controller](../../dashboard/README.md) | [Terraform / OpenTofu](../terraform.md) |

Whatever the tool, the node checks the caller's role (`os:reader`,
`os:operator`, `os:admin`) on every call and logs who changed what: a
tool offering an action doesn't mean you may take it. The Controller and
the TUI offer only the actions your role allows. Calling the API
yourself, from your own code or `grpcurl`: [driving
nodes](../private-cloud/driving-nodes.md); every call and its role: the
[node API reference](../private-cloud/api-reference.md).

In the tables below, the tool does it where a cell names janusctl's
command (shown without `janusctl`) or the Controller's page (in a
node's sidebar), or shows ✅ - in the TUI, a panel shows it or its
action menu makes the call. ⚙️ means the tool makes the call by itself,
for a feature of its own; - that it doesn't. Terraform's `janus_node` drives a node's
network and its release, `janus_haproxy_config` its haproxy.cfg; its ⚙️
are calls the Controller makes for a `janus_node`.

## The node's API, call by call

### SystemService: the node itself

| Call | janusctl | TUI | Controller | Terraform |
|---|---|---|---|---|
| `Version` - release, kernel, slots, image | `version`, `system info` | ✅ | Overview, Update | ⚙️ |
| `Hostname` - the node's hostname | `system hostname` | ✅ | Overview | - |
| `Reboot` - soft-stop HAProxy, reboot: firmware, power cycle or kexec | `system reboot` | ✅ | Power | - |
| `Shutdown` - soft-stop HAProxy, power off | `system shutdown` | - | Power; ⚙️ resizing or deleting a VM | ⚙️ |
| `Restart` - restart janusd, HAProxy keeps serving | `system restart` | ✅ | Power | - |
| `Reset` - wipe STATE, reboot with a new CA | `system reset` | - | Power | - |
| `Events` - the node's event log, live | `system events` | ✅ | Events | - |
| `Dmesg` - the kernel's messages | `system dmesg` | - | Kernel | - |
| `Logs` - a service's output | `system logs` | ✅ | Service logs | - |
| `Stats` - each service's CPU and memory | `system stats` | ✅ | Metrics | - |
| `SystemStat`, `Memory`, `CPUInfo`, `LoadAvg` - CPU, memory, load | `system info`, `systemstat` | ✅ | Overview, Metrics | - |
| `DiskStats` - each disk's counters | `system info` | - | Storage | - |
| `DiskUsage` - a path's disk usage | `system du` | - | Storage | - |
| `NetworkDeviceStats` - each interface's counters | `system netdev` | ✅ | Network | - |
| `Netstat` - TCP and UDP sockets | `system netstat` | - | Network | - |
| `Mounts` - mounted filesystems | `system mounts` | - | Storage | - |
| `Processes` - every process | `system ps` | ✅ | Processes | - |
| `ServiceList` - the services and their health | `system services` | ✅ | Services | - |
| `ServiceStart`, `ServiceStop`, `ServiceRestart` | `system service` | - | Services | - |
| `List`, `Read`, `Copy` - the node's files, never its secrets | `system ls`, `cat`, `cp` | - | Files | - |
| `PacketCapture` - a live capture, as pcap | `system pcap` | - | Packet capture | - |
| `GenerateClientConfiguration` - a certificate of the node's own CA | `pki generate-client-config` | - | Access; ⚙️ adding a node | - |
| `MetricsConfigGet`, `MetricsConfigSet` - the Janus exporter | `system metrics` | - | Janus exporter | - |
| `NodeExporterConfigGet`, `NodeExporterConfigSet` - prometheus-node-exporter | `system node-exporter` | - | Node exporter | - |
| `SysctlList` - kernel parameters, suggestions, the CIS benchmark | `system sysctl list`, `get`, `observed` | ✅ | Sysctl; ⚙️ auto-scaling | - |
| `SysctlApply` - parameters on trial | `system sysctl set`, `reset` | - | Sysctl; ⚙️ auto-scaling | ⚙️ |
| `SysctlConfirm` - keep the values on trial | `system sysctl confirm` | ✅ | Sysctl; ⚙️ auto-scaling | ⚙️ |
| `SysctlCancel` - the previous values back, now | `system sysctl cancel` | - | Sysctl | - |
| `SysctlHistory` - who changed what, when | `system sysctl history` | - | Sysctl; ⚙️ auto-scaling | - |
| `ApplyConfiguration`, `MetaWrite`, `MetaDelete` - not implemented | - | - | - | - |

Terraform's ⚙️: a `janus_node`'s version read back, a clean shutdown
before a resize or a destroy, its kernel parameters following its size
with `autoscaling.sysctl`. `ApplyConfiguration` and the `Meta*` calls
answer "unimplemented": Janus has no declarative machine configuration
and no META partition.

### HAProxyService

| Call | janusctl | TUI | Controller | Terraform |
|---|---|---|---|---|
| `GetConfig` - the running haproxy.cfg | `haproxy get-config` | ⚙️ | HAProxy › Configuration | ✅ |
| `ApplyConfig` - check, write, reload without dropping connections | `haproxy apply-config` | - | HAProxy › Configuration | ✅ |
| `ValidateConfig` - check only | - | - | HAProxy › Configuration | - |
| `Reload` - a seamless reload | - | ✅ | HAProxy | - |
| `Stats` - `show stat` | `haproxy stats` | ✅ | HAProxy › Status | - |
| `ShowInfo` - version, uptime, connections | `haproxy show-info` | ✅ | HAProxy › Status | - |
| `BackendList` - backends, servers, states | `haproxy backends` | - | HAProxy › Backends | - |
| `ServerSetState` - ready, drain, maintenance | - | ✅ | HAProxy › Backends | - |
| `MapList`, `MapGet` - the maps and their entries | `haproxy map-list`, `map-get` | ⚙️ | HAProxy › Maps & ACLs | - |
| `MapUpdate` - set or delete an entry, live | `haproxy map-set`, `map-delete` | - | HAProxy › Maps & ACLs | - |
| `ACLUpdate` - add or delete a pattern, live | `haproxy acl-add`, `acl-delete` | - | HAProxy › Maps & ACLs | - |
| `MapLookup`, `ACLLookup` - the entry or pattern a value matches | `haproxy map-lookup`, `acl-lookup` | - | HAProxy › Maps & ACLs | - |
| `TableList`, `TableShow`, `TableClear` - stick tables | `haproxy tables`, `table`, `table-clear` | - | HAProxy › Stick tables | - |
| `SessionList`, `SessionShutdown` - the sessions in progress | `haproxy sessions`, `session-kill` | - | HAProxy › Sessions | - |
| `ErrorsShow` - the last refused requests and responses | `haproxy errors` | - | HAProxy › Errors | - |
| `CertificateList`, `CertificateUpload`, `CertificateDelete` | `haproxy cert-list`, `cert-upload`, `cert-delete` | - | HAProxy › Certificates | - |
| `FileList`, `FileGet`, `FilePut`, `FileDelete` - error pages, CAs... | `haproxy files`, `file-get`, `file-put`, `file-delete` | - | HAProxy › Files | - |
| `ACMEStatus`, `ACMEGetConfig`, `ACMEApplyConfig`, `ACMERenew` - Let's Encrypt | `haproxy acme` | - | Let's Encrypt | - |

The TUI's ⚙️: its traffic flow (`f`) reads the configuration and its
maps - the Controller's Traffic flow page too. `apply-config` and
`janus_haproxy_config` have the node check the configuration before
applying it; checking alone is the Controller's.

### NetworkService

| Call | janusctl | TUI | Controller | Terraform |
|---|---|---|---|---|
| `NetworkStatus` - interfaces, addresses, routes, DNS, clock | `network status` | ✅ | Network | ⚙️ |
| `NetworkConfigGet` - the saved configuration | `network get` | - | System › Network | ✅ |
| `NetworkConfigApply` - a configuration on trial | `network apply` | - | System › Network | ✅ |
| `NetworkConfigConfirm` - keep it | `network confirm` | ✅ | System › Network | ✅ |
| `FirewallList` - saved, on trial, live | `network firewall status` | ✅ | Firewall | - |
| `FirewallGetRuleset`, `FirewallApplyRuleset` | `network firewall get`, `check`, `apply` | - | Firewall | - |
| `FirewallConfirm` - keep the ruleset on trial | `network firewall confirm` | ✅ | Firewall | - |
| `FirewallSets`, `FirewallSetUpdate` - the live named sets | `network firewall sets`, `set-add`, `set-del` | - | Firewall | - |
| `VRRPStatus` - instances, virtual IPs | `network vrrp status` | ✅ | VRRP | - |
| `VRRPGetConfig`, `VRRPApplyConfig` - keepalived.conf | `network vrrp get`, `check`, `apply` | - | VRRP | - |
| `BGPStatus` - protocols, sessions, routes | `network bgp status` | ✅ | BGP | - |
| `BGPGetConfig`, `BGPApplyConfig` - bird.conf | `network bgp get`, `check`, `apply` | - | BGP | - |
| `ConsulStatus` - the agent, its cluster | `network consul status` | ✅ | Consul | - |
| `ConsulGetConfig`, `ConsulApplyConfig` - its configuration | `network consul get`, `check`, `apply` | - | Consul | - |

`janusctl network apply` and `janus_node` confirm the trial by
themselves (`interfaces`, `dns`, `ntp`). `janusctl network modules`,
the Controller's sidebar and the TUI's panels tell which optional
modules the image has, from the `*Status` calls.

### LifecycleService

| Call | janusctl | TUI | Controller | Terraform |
|---|---|---|---|---|
| `Install` - partition a blank disk, write a release | `lifecycle install` | - | - | - |
| `Upgrade` - the other slot, reverted unless healthy | `lifecycle upgrade` | - | Update; ⚙️ a VM's release | ✅ |
| `UploadReleaseFile` - a bundle streamed to the node | `lifecycle upload-release` | - | Update | - |
| `Rollback` - boot the other slot | `lifecycle rollback` | - | - | - |

`lifecycle upgrade` takes a directory on the node or a URL; the
Controller's Update page a version, a URL or a bundle it relays;
`janus_node` its `version`, `extensions`, `haproxy` and `kernel`
(`update_reboot_mode`: firmware or kexec). The Controller installs
nothing: it creates VMs from images.

### AccessService: the fleet a node trusts

| Call | janusctl | TUI | Controller | Terraform |
|---|---|---|---|---|
| `TrustGet` - the fleet's root and bundle, the node's CA | `access trust`; ⚙️ `fleet` | - | Access; ⚙️ its fleet | - |
| `TrustSet` - pin the root, apply a newer bundle | `access trust-set`; ⚙️ `fleet` | - | ⚙️ its fleet | - |
| `TrustReset` - forget the fleet | `access trust-reset` | - | - | - |
| `LocalCARotate` - a new CA for the node | `access rotate-ca` | - | Access | - |

`janusctl fleet adopt` and `fleet sync` set the trust by themselves; the
Controller sets its fleet's on each node every 10 minutes. `TrustReset`
takes a certificate of the node's own CA.

## Beyond the node's API

What a tool adds on top of the calls above - the fleet, accounts,
virtual machines:

| | janusctl | TUI | Controller | Terraform |
|---|---|---|---|---|
| Several nodes at once | `-n a,b`, `-all` | ✅ | ✅ | ✅ |
| Signing in | SSH key, browser, token | janusctl's | password, passkey, TOTP | API token |
| Accounts, roles, label scopes, API tokens | - | - | ✅ | - |
| The fleet's trust: root, bundle, issuing CAs | `fleet` | - | ✅ | - |
| Nodes registering themselves, enrollment tokens | - | - | ✅ | - |
| A disk image provisioned offline | `image` | - | ⚙️ its VMs | - |
| VMs on [libvirt/KVM or Proxmox VE](../hypervisors.md): create, resize, delete | - | - | ✅ | ✅ |
| [Auto-scaling](auto-scaling.md): vCPUs and memory hot-plugged | - | - | ✅ | ✅ |
| A VM's serial console | - | - | ✅ | - |
| Node labels | - | - | ✅ | ✅ |
| Images with other extensions, HAProxy branch or kernel | - | - | ✅ | ✅ |
| Update and security notices | - | - | ✅ | - |
| Live graphs | - | ✅ | ✅ | - |
| Traffic flow: frontends to servers | - | ✅ | ✅ | - |
| Encrypted backups, to S3 too | - | - | ✅ | - |
| Audit log | - | - | ✅ | - |
| Drift: changes made elsewhere | - | - | ✅ | ✅ |
| Locking a node's page | - | - | - | ✅ |
| Shell completion, pickers, plain output | ✅ | - | - | - |

Without a Controller, janusctl keeps [a fleet](../fleet-without-controller.md)
itself and upgrades a node to any image given its URL. The Controller
says when an update is available, 🔒 when it fixes a vulnerability. Each node logs who changed what; the
Controller's audit log has every change made through it, its backups
each node's configuration. Terraform declares hypervisors
(`janus_hypervisor`, `janus_proxmox_hypervisor`) and VMs (`janus_node`:
size, `autoscaling`, `labels`, `lock_ui`, release, network); `terraform
plan` shows what changed elsewhere, and the Controller's record of a VM
follows the machine.

## Which one to use

- **One node, a terminal, a script**: `janusctl` - nearly the whole API,
  plain output, with a Controller or without.
- **Watching a fleet, acting fast**: `janusctl tui` - drain a server,
  reload HAProxy, reboot, confirm a trial, without leaving the terminal.
- **A team, a fleet, its VMs**: the Controller - accounts and roles, the
  nodes' pages, updates, backups, the VMs it creates and grows.
- **Nodes as code**: Terraform - the VM, its network, its release and
  its haproxy.cfg in a plan; a token narrowed to HAProxy is enough for
  `janus_haproxy_config`.
- **Something none of them does**: the node's API itself, from your
  own code ([your own orchestrator](../private-cloud/own-orchestrator.md)).

Only `janusctl` installs onto a blank disk (`lifecycle install`), rolls
back (`lifecycle rollback`) and forgets a fleet (`access trust-reset`).
It has no command to drain a server or to reload HAProxy alone: the TUI
and the Controller do. Only the Controller checks a haproxy.cfg without
applying it.
