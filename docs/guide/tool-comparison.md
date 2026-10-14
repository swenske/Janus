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

In the tables below: ✅ a command, a page or an attribute does it ·
⚙️ the tool makes the call by itself, for a feature of its own · - not
offered. janusctl's commands are shown without `janusctl`; the
Controller's pages are those of a node's page (its sidebar).

## The node's API, call by call

### SystemService: the node itself

| Call | What it does | janusctl | janusctl tui | Controller | Terraform |
|---|---|---|---|---|---|
| `Version` | Janus release, kernel, slots, image | ✅ `version`, `system info` | ✅ header | ✅ Overview, Update | ⚙️ `janus_node` reads it back |
| `Hostname` | The node's hostname | ✅ `system hostname` | ✅ header | ✅ Overview | - |
| `Reboot` | Soft-stop HAProxy, then reboot (firmware, power cycle or kexec) | ✅ `system reboot` | ✅ action | ✅ Power | - |
| `Shutdown` | Soft-stop HAProxy, then power off | ✅ `system shutdown` | - | ✅ Power; ⚙️ before resizing or deleting a VM it created | ⚙️ `janus_node`: before a resize or a destroy |
| `Restart` | Restart janusd, HAProxy keeps serving | ✅ `system restart` | ✅ action | ✅ Power | - |
| `Reset` | Wipe STATE and reboot: a new CA | ✅ `system reset` | - | ✅ Power | - |
| `Events` | The node's event log, live | ✅ `system events` | ✅ events panel | ✅ Events, Overview | - |
| `Dmesg` | The kernel's messages | ✅ `system dmesg` | - | ✅ Kernel (dmesg) | - |
| `Logs` | A service's output (janusd, HAProxy, extensions) | ✅ `system logs` | ✅ log panel | ✅ Service logs | - |
| `Stats` | CPU and memory of each service | ✅ `system stats` | ✅ services panel | ✅ Metrics | - |
| `SystemStat`, `Memory`, `CPUInfo`, `LoadAvg` | CPU, memory, load | ✅ `system info`, `system systemstat` | ✅ graphs | ✅ Overview, Metrics | - |
| `DiskStats` | Each disk's counters | ✅ `system info` | - | ✅ Storage | - |
| `DiskUsage` | Disk usage of a path | ✅ `system du` | - | ✅ Storage | - |
| `NetworkDeviceStats` | Each interface's counters | ✅ `system netdev` | ✅ graphs | ✅ Network, Packet capture | - |
| `Netstat` | TCP and UDP sockets | ✅ `system netstat` | - | ✅ Network | - |
| `Mounts` | Mounted filesystems | ✅ `system mounts` | - | ✅ Storage | - |
| `Processes` | Every process | ✅ `system ps` | ✅ processes panel | ✅ Processes | - |
| `ServiceList` | Managed services and their health | ✅ `system services` | ✅ services panel | ✅ Services | - |
| `ServiceStart`, `ServiceStop`, `ServiceRestart` | Control a managed service | ✅ `system service` | - | ✅ Services | - |
| `List`, `Read`, `Copy` | List, print or archive the node's files (never its secrets) | ✅ `system ls`, `system cat`, `system cp` | - | ✅ Files | - |
| `PacketCapture` | A live capture, as a pcap stream | ✅ `system pcap` | - | ✅ Packet capture | - |
| `GenerateClientConfiguration` | A client certificate of the node's own CA | ✅ `pki generate-client-config` | - | ✅ Access; ⚙️ once, when adding a node | - |
| `MetricsConfigGet`, `MetricsConfigSet` | The Janus exporter's settings | ✅ `system metrics` | - | ✅ Janus exporter | - |
| `NodeExporterConfigGet`, `NodeExporterConfigSet` | prometheus-node-exporter's settings | ✅ `system node-exporter` | - | ✅ Node exporter | - |
| `SysctlList` | Kernel parameters, their suggestions, the CIS benchmark | ✅ `system sysctl list`, `get`, `observed` | ✅ trial pending | ✅ Sysctl; ⚙️ auto-scaling | - |
| `SysctlApply` | Change parameters on trial | ✅ `system sysctl set`, `reset` | - | ✅ Sysctl; ⚙️ auto-scaling | ⚙️ `janus_node`'s `autoscaling.sysctl` |
| `SysctlConfirm` | Keep the values on trial | ✅ `system sysctl confirm` | ✅ action | ✅ Sysctl; ⚙️ auto-scaling | ⚙️ `janus_node`'s `autoscaling.sysctl` |
| `SysctlCancel` | Put the previous values back now | ✅ `system sysctl cancel` | - | ✅ Sysctl | - |
| `SysctlHistory` | Who changed what, when | ✅ `system sysctl history` | - | ✅ Sysctl; ⚙️ auto-scaling | - |
| `ApplyConfiguration` | Not implemented: Janus has no declarative machine configuration | - | - | - | - |
| `MetaWrite`, `MetaDelete` | Not implemented: Janus has no META partition | - | - | - | - |

### HAProxyService

| Call | What it does | janusctl | janusctl tui | Controller | Terraform |
|---|---|---|---|---|---|
| `GetConfig` | The running haproxy.cfg | ✅ `haproxy get-config` | ⚙️ traffic flow (`f`) | ✅ HAProxy › Configuration, Traffic flow | ✅ `janus_haproxy_config` |
| `ApplyConfig` | Validate, write, reload without dropping connections | ✅ `haproxy apply-config` | - | ✅ HAProxy › Configuration | ✅ `janus_haproxy_config` |
| `ValidateConfig` | Have HAProxy check a configuration, apply nothing | - (`apply-config` checks first) | - | ✅ HAProxy › Configuration | - (an apply checks first) |
| `Reload` | Reload HAProxy seamlessly | - | ✅ action | ✅ HAProxy | - |
| `Stats` | `show stat` | ✅ `haproxy stats` | ✅ HAProxy panels | ✅ HAProxy › Status, Traffic flow | - |
| `ShowInfo` | Version, uptime, connections | ✅ `haproxy show-info` | ✅ HAProxy panel | ✅ HAProxy › Status | - |
| `BackendList` | Backends, their servers and states | ✅ `haproxy backends` | - (from `Stats`) | ✅ HAProxy › Backends | - |
| `ServerSetState` | A server ready, drained or in maintenance | - | ✅ action | ✅ HAProxy › Backends | - |
| `MapList`, `MapGet` | The file-backed maps and their entries | ✅ `haproxy map-list`, `map-get` | ⚙️ traffic flow (`f`) | ✅ HAProxy › Maps & ACLs, Traffic flow | - |
| `MapUpdate` | Set or delete a map entry, live | ✅ `haproxy map-set`, `map-delete` | - | ✅ HAProxy › Maps & ACLs | - |
| `ACLUpdate` | Add or delete an ACL pattern, live | ✅ `haproxy acl-add`, `acl-delete` | - | ✅ HAProxy › Maps & ACLs | - |
| `CertificateList`, `CertificateUpload`, `CertificateDelete` | Certificates in HAProxy's store | ✅ `haproxy cert-list`, `cert-upload`, `cert-delete` | - | ✅ HAProxy › Certificates | - |
| `FileList`, `FileGet`, `FilePut`, `FileDelete` | HAProxy's own files: error pages, CAs, map files... | ✅ `haproxy files`, `file-get`, `file-put`, `file-delete` | - | ✅ HAProxy › Files | - |
| `ACMEStatus`, `ACMEGetConfig`, `ACMEApplyConfig`, `ACMERenew` | Let's Encrypt (letsencrypt extension) | ✅ `haproxy acme` | - | ✅ Let's Encrypt | - |

### NetworkService

| Call | What it does | janusctl | janusctl tui | Controller | Terraform |
|---|---|---|---|---|---|
| `NetworkStatus` | Interfaces, addresses, routes, DNS, clock | ✅ `network status` | ✅ interfaces, trial pending | ✅ Network | ⚙️ `janus_node` |
| `NetworkConfigGet` | The saved network configuration | ✅ `network get` | - | ✅ System › Network | ✅ `janus_node` (`interfaces`, `dns`, `ntp`) |
| `NetworkConfigApply` | A configuration on trial, reverted unless confirmed | ✅ `network apply` | - | ✅ System › Network | ✅ `janus_node` (`interfaces`, `dns`, `ntp`) |
| `NetworkConfigConfirm` | Keep the configuration on trial | ✅ `network confirm` (`apply` confirms by itself) | ✅ action | ✅ System › Network | ✅ `janus_node` (confirms by itself) |
| `FirewallList` | Saved, on trial, live (nftables extension) | ✅ `network firewall status` | ✅ firewall panel | ✅ Firewall | - |
| `FirewallGetRuleset`, `FirewallApplyRuleset` | The ruleset, checked or applied on trial | ✅ `network firewall get`, `check`, `apply` | - | ✅ Firewall | - |
| `FirewallConfirm` | Keep the ruleset on trial | ✅ `network firewall confirm` | ✅ action | ✅ Firewall | - |
| `FirewallSets`, `FirewallSetUpdate` | The live named sets: list, add, delete | ✅ `network firewall sets`, `set-add`, `set-del` | - | ✅ Firewall | - |
| `VRRPStatus` | keepalived's instances and virtual IPs | ✅ `network vrrp status` | ✅ VRRP panel | ✅ VRRP | - |
| `VRRPGetConfig`, `VRRPApplyConfig` | keepalived.conf: read, check, apply | ✅ `network vrrp get`, `check`, `apply` | - | ✅ VRRP | - |
| `BGPStatus` | BIRD's protocols, sessions, routes | ✅ `network bgp status` | ✅ BGP panel | ✅ BGP | - |
| `BGPGetConfig`, `BGPApplyConfig` | bird.conf: read, check, apply | ✅ `network bgp get`, `check`, `apply` | - | ✅ BGP | - |
| `ConsulStatus` | The Consul agent, its node, its cluster | ✅ `network consul status` | ✅ Consul panel | ✅ Consul | - |
| `ConsulGetConfig`, `ConsulApplyConfig` | Consul's configuration: read, check, apply | ✅ `network consul get`, `check`, `apply` | - | ✅ Consul | - |

`janusctl network modules`, the Controller's sidebar and the TUI's
module panels tell which optional modules the image has, from these
`*Status` calls.

### LifecycleService

| Call | What it does | janusctl | janusctl tui | Controller | Terraform |
|---|---|---|---|---|---|
| `Install` | Partition a blank disk and write a release | ✅ `lifecycle install` | - | - (it creates VMs from images instead) | - |
| `Upgrade` | Write a release to the other slot, reboot into it, revert unless healthy | ✅ `lifecycle upgrade` (a node-side directory or a URL) | - | ✅ Update (by version, a URL, or a bundle it relays); ⚙️ a VM's version or apps | ✅ `janus_node` (`version`, `extensions`, `haproxy`, `kernel`, `update_reboot_mode`) |
| `UploadReleaseFile` | Stream a release bundle to the node | ✅ `lifecycle upload-release` | - | ✅ Update (relay, upload) | - |
| `Rollback` | Boot the other slot | ✅ `lifecycle rollback` | - | - | - |

### AccessService: the fleet a node trusts

| Call | What it does | janusctl | janusctl tui | Controller | Terraform |
|---|---|---|---|---|---|
| `TrustGet` | The fleet's root and bundle, the node's own CA | ✅ `access trust`; ⚙️ `fleet adopt`, `fleet sync` | - | ✅ Access; ⚙️ its fleet | - |
| `TrustSet` | Pin the fleet's root, apply a newer bundle | ✅ `access trust-set`; ⚙️ `fleet adopt`, `fleet sync` | - | ⚙️ its fleet, every 10 minutes | - |
| `TrustReset` | Forget the fleet (the node's own CA only) | ✅ `access trust-reset` | - | - | - |
| `LocalCARotate` | Replace the node's own CA | ✅ `access rotate-ca` | - | ✅ Access | - |

## Beyond the node's API

What a tool adds on top of the calls above - the fleet, accounts,
virtual machines:

| | janusctl | janusctl tui | Controller | Terraform |
|---|---|---|---|---|
| Several nodes at once | ✅ `-n a,b`, `-all` | ✅ the fleet screen | ✅ the nodes list | ✅ one resource per node |
| Signing in | ✅ `login`: an SSH key, the browser, a device code, an API token | ✅ janusctl's contexts | ✅ accounts, passkeys or TOTP, trusted browsers | ✅ an API token |
| Accounts, roles, label scopes, API tokens | - (it uses them) | - | ✅ | - (it uses them) |
| The fleet's trust: root, bundle, issuing CAs | ✅ `fleet`: [without a Controller](../fleet-without-controller.md) | - | ✅ fleet setup, recovery kit | - |
| Nodes registering themselves, approval, enrollment tokens | - | - | ✅ | - |
| Provisioning a disk image offline | ✅ `image seed-controller`, `seed-fleet`, `seed-network` | - | ⚙️ a NoCloud disk for each VM it creates | - |
| Creating, resizing, deleting VMs on libvirt/KVM or Proxmox VE | - | - | ✅ [hypervisors](../hypervisors.md) | ✅ `janus_hypervisor`, `janus_proxmox_hypervisor`, `janus_node` |
| [Auto-scaling](auto-scaling.md): vCPUs and memory hot-plugged | - | - | ✅ | ✅ `janus_node`'s `autoscaling` |
| A VM's serial console | - | - | ✅ | - |
| Node labels | - | - | ✅ | ✅ `janus_node`'s `labels` |
| Images with other extensions, HAProxy branch or kernel | - (give `lifecycle upgrade` the image's URL) | - | ✅ Update › add or remove apps | ✅ `janus_node`'s `extensions`, `haproxy`, `kernel` |
| Update and security notices | - | ✅ pre-releases marked | ✅ update available, 🔒 security badge | - |
| Live graphs, refreshed every few seconds | - | ✅ | ✅ | - |
| Traffic flow: frontends to servers | - | ✅ `f` | ✅ Traffic flow | - |
| Backups of the fleet (encrypted, S3) | - | - | ✅ with each node's configuration | - |
| Audit log | - (each node logs who changed what) | - | ✅ every change made through it | - |
| Drift: changes made elsewhere | - | - | ✅ a VM's record follows the machine | ✅ `terraform plan` shows them |
| Locking a node's page against changes | - | - | - | ✅ `janus_node`'s `lock_ui` |
| Shell completion, pickers, plain output to script | ✅ | - | - | - |

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
