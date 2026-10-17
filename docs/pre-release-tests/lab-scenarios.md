# Lab test scenarios

The scenarios a pre-release goes through in the lab, how each one is run
and what it measures - the protocol behind every
[pre-release report](README.md). The newest release runs the same
scenario first, measured the same way, so that each figure has something
to compare with. A report says when a scenario was skipped, changed or
added.

## The lab

| Role | What |
|---|---|
| KVM host | Xeon E3-1220 v3 (4 cores), 16 GB, Debian 13, QEMU q35, Debian's OVMF 2025.02 |
| Router | RouterOS 7: the lab's VLANs, BGP with BFD (200 ms × 3) to the nodes, no BGP ECMP (one node carries the anycast route at a time) |
| Node 01 and node 02 | 2 vCPU (host CPU), 1 GiB, OVMF without Secure Boot; a schematic with the five extensions (BGP, VRRP, nftables, node_exporter, the QEMU agent); management, front and backend networks |
| High availability | a VIP by VRRP (VRRPv3, 0.2 s adverts, HAProxy's health weighs -100) and an anycast /32 by BGP |
| The auto-scaled node | since v2026.10.14: a machine the Controller creates on the same host, 2 vCPU and 1 GiB from the release's default image, room for 4 vCPU and 2 GiB |
| Backends | two nginx VMs, 1 vCPU each |
| Load client | a 2-vCPU VM on the same host, with `wrk` - up to v2026.10.14, removed since: it took the host's CPUs from the nodes |
| The clients | since v2026.10.14-2, outside the lab's host, reaching the front network through the routers like internet users: the three CI runners (16, 12 and 8 threads), used only while none runs a CI job, and since v2026.10.14-3 a fourth machine that isn't a runner (16 threads, always free: the probes run there; on a 2.5 GbE link since v2026.10.16-4, its path changed - the release measured again; left out of v2026.10.17-2's campaign for a memory fault, the probes then ran from a runner). A temporary `wrk` on each, removed afterwards. The host's CPUs go to the nodes |
| Controller | Docker Compose with its updater, an admin with TOTP |

The two nodes and the backends saturate the host's 4 cores under a
full load: HAProxy's throughput figures compare releases with each
other, they aren't a capacity measure.

## How it is measured

- **Probes**: a request every 50 ms on the VIP and the anycast address
  (or the node under test), each with a 1 s timeout and stamped with its
  start, the node that answered read from a header. An outage runs from
  the first failed request to the next one that succeeds, so it is
  measured to 50 ms.
- **Path noise**: the network path from a runner sometimes loses one
  request, about one every 1 to 2 minutes, when nothing changes in the
  lab: a request that gets no answer within its second. Before a
  campaign, each prober is run for 2 minutes against an untouched VIP
  (the fourth machine: none lost in 4,385; a runner, for v2026.10.17-2:
  none in 4,680). A single isolated timeout
  outside the moment a scenario changes something is reported apart
  from the scenario's losses.
- **Load**: `wrk` (4.1.0). Keep-alive connections unless a scenario says
  otherwise.
- **The nodes**: the pre-release's own `janusctl` (from its `.deb`),
  the Janus exporter (`janus_boot_stage_seconds`), a node's
  `/proc/stat` read through its file API for the CPU split with steal
  time, the event log (`system events`).
- **The Controller's pages**: headless Chromium (the docs' pinned one),
  both themes, console errors and failed requests collected.

## The scenarios

### The Controller's update

The Controller updates itself through its updater, from the newest
release to the pre-release. Measured:

- what it offers: the newest release, and the pre-release apart;
- that installing the pre-release without confirming it's one is
  refused (409);
- the update's length (the updater's job);
- the seconds without an answer, from a probe of `/api/version` every
  0.1 s run on the lab's host (since v2026.10.14-2; from the workstation
  before, whose path adds its own lost requests).

### The image factory

A node's Update page offers the pre-release apart; asked to, the factory
builds it for the lab's schematic. Measured: the time until the bundle
is ready, and the `schematic-build` run.

### Rolling update under load

Both nodes are updated one after the other through the Controller, each
with `wait_for_health`. Each node is drained first: its VRRP priority goes
to 50 and its anycast route is withdrawn. Both come back once the node is
in service again: healthy on the new version, VRRP not in FAULT, BGP
established. `wrk -t1 -c8` loads the VIP throughout; probes on the VIP and
the anycast address.

Measured: requests lost on each address, each node's time from its
Upgrade to back in service, `wrk`'s rate, 99th percentile and errors.

### HAProxy's throughput

`wrk` on the VIP:

- from one client: keep-alive with 8, 64 and 256 connections, then a new
  connection for every request (32 connections) - 30 s each, two runs;
  since v2026.10.14-3 that client is the fourth machine (a runner
  before, the lab's own client up to v2026.10.14; a runner again for
  v2026.10.17-2);
- since v2026.10.14-2, from the clients at once: 256 keep-alive
  connections each, 30 s, two runs (the three runners, and the fourth
  machine since v2026.10.14-3 - except v2026.10.17-2).

Measured: requests per second and the 99th percentile, with the nodes'
boot stages. When the release's runs couldn't be taken before the
update, the nodes roll back to it for a few minutes (the other slot),
then forward again - the report says so.

### Failovers

On the node holding the VIP and the anycast route, probes for 60 s:

- **node loss**: the VM destroyed (`virsh destroy`), started again 20 s
  later;
- **HAProxy stopped** for 10 s through the API, then started.

Measured: each address's outage, when the node goes and when it comes
back.

### Reboots

On node 02:

- through the firmware, twice;
- through kexec, chained, three times.

Each is timed from the Reboot call to the API answering again. The
kernel log is searched for an oops afterwards.

### Rollback

Node 02 rolls back to the release, then back to the pre-release. Each
roll back is timed from the call to the API answering again. Since
v2026.10.14, `version` also shows the other slot after each one.

### The nodes' health

On both nodes:

- versions (the release, HAProxy, the kernel track, the extensions);
- every service running and healthy;
- SELinux enforcing, with no AVC denial;
- OOM kills;
- the network modules;
- the CIS Debian Linux 13 Benchmark (Level 2 - Server) at 33/33.

### What the pre-release brings

Each campaign checks the pre-release's own features on real nodes. For
example:

- v2026.10.12: `janusctl tui`'s splash screen, and kexec reboots under
  the SMM-locked Secure Boot OVMF;
- v2026.10.14: the other slot and the kept event log - event IDs going on
  without a gap through a power cut, reboots and a rollback;
- v2026.10.14 and -2: auto-scaling, below;
- v2026.10.14-4 and -5: what auto-scaling does when its mode changes or
  its policy goes - the kernel parameters put back, an OOM kill counted
  once - below;
- v2026.10.15: the image with every extension - built by the factory, a
  machine created from it through the Controller, its services and
  denials; the nodes of the previous release naming the new image;
- v2026.10.15-2: the OOM order - every process's `oom_score_adj` on the
  nodes, and the three-cache configuration (below) without janusd ever
  killed;
- v2026.10.16 and -2: log forwarding and the LOGS partition, below - for
  -2, each boot's kernel messages counted by host on the updated nodes;
- v2026.10.16-3: an *unsupported* machine updated through the Controller,
  timed until its profile is applied; a target never saved tested
  through the Controller's API;
- v2026.10.16-4: both kernel tracks updated - a machine on the other
  track (longterm) created through the Controller, the image factory
  building its image; its health, and its kexec and firmware reboots
  timed from its own event log. The lab keeps it for the next kernel
  updates;
- v2026.10.17: dependency updates only - every step against the release
  measured the same morning, the longterm machine updated through the
  Controller, and the Controller serving its rebuilt pages;
- v2026.10.17-2: HAProxy, keepalived and BIRD as static PIEs - where
  each binary's first mapping starts (`/proc/<pid>/maps`), on the
  release and then after the update, a reload, HAProxy stopped and
  started, firmware and kexec reboots and a rollback: a new address at
  every start.

### HAProxy killed by the kernel

Since v2026.10.14-3, on the auto-scaled node (1 GiB, auto-scaling
paused), with probes on it:

- **The configuration**: three 400 MB caches, which HAProxy writes
  through when it starts - more than the node's memory together.
- **Applied several times**: the kernel's OOM killer kills the new
  process, and sometimes the serving one after it.
- **Measured for each try**:
  - what the kernel killed (`kernel.oom_kill`);
  - whether the configuration was refused and the previous one put back;
  - whether janusd started HAProxy again (`haproxy.restarted`) when the
    serving process died;
  - the time until the node serves again, and the requests lost.
- **Once with auto-scaling on**: the memory step that follows an OOM
  kill at once, then paused - back to its size.

### Auto-scaling

Since v2026.10.14, on the auto-scaled node, with probes on it
throughout. The return delay is 10 minutes instead of 30, to fit the
campaign; every other threshold keeps its default.

| Step | How | Measured |
|---|---|---|
| Created | the Controller creates the machine from the release's image | time to ready |
| Updated | since v2026.10.14-2: the machine's version changed through the Controller | time, its record afterwards |
| A policy set | maxima 4 vCPU and 2 GiB, mode observe: the one restart it takes | time, HTTP unanswered |
| Observing under load | `wrk` for 7 minutes | the journal and the status say what it would do, nothing resized |
| On, the host saturated | load through the backends: the node, both backends and the load share the host's 4 cores | the steps taken or refused, the node's steal time, HAProxy's idle time, lost requests |
| On, the node alone | since v2026.10.14-2: HAProxy answering by itself, the three runners loading it - the host's CPUs left to the node | the same |
| Back to its size | the load gone | each step down after the return delay, lost requests |
| Memory under pressure | a 700 MB HAProxy cache on 1 GiB (7 % of the memory available), removed after 6 minutes | the step up, the kernel parameters following (`tcp_mem`, `fs.file-max`), the step down, the parameters back, lost requests |
| Paused | the policy's mode set to paused | back to its size, how fast |
| Modes and removal | since v2026.10.14-4: on, an OOM kill (the three-cache configuration below), the usual configuration back, paused, on again for 2 minutes, the policy removed | the kernel parameters back with its size and before the removal's restart; no step on again for a kill already answered (since -5); the node's events and probes throughout |
| Put back with HAProxy down | since v2026.10.14-6, on a machine whose kernel parameters nobody changed by hand: the three-cache configuration applied until it runs at 2 GiB, then paused (back to 1 GiB, where it can't run), on again, the policy removed at 2 GiB | the parameters confirmed back at 1 GiB and still there past the trial's 2 minutes; put back before the removal's restart; HAProxy's processes listed every 3 s |
| Its pages | the Hypervisors tab: the machine's card, its status, its journal | both themes, no console error |

### Log forwarding

Since v2026.10.16. The log servers: a single-node Elasticsearch (2 CPUs)
and a Logstash (1 CPU, syslog over UDP, TCP and TLS with its own CA) on
a Docker host of the home network, behind its reverse proxy.

| Step | How | Measured |
|---|---|---|
| From the Controller | its Log forwarding page through the API: its own log and audit to Elasticsearch; a profile for every node - Elasticsearch and Logstash over TLS | each node's state (applied, *unsupported* for the older ones), `managed by` on the node, what each data stream and Logstash received, by host |
| A new machine | created by the Controller from the release's image, then rebooted | time to ready, its LOGS partition and boots (its node page), the profile applied at admission, its boot's kernel messages in Elasticsearch |
| The updated nodes' reboots | the firmware and kexec reboots below, forwarding on | each boot's kernel messages in Elasticsearch and the host they're under |
| Throughput | the benchmark below with forwarding on (a profile's default sources) | against the release, measured the same morning |
| The cost | on a node of the branch with LOGS, HAProxy answering every request itself, four clients for a minute: no log, `log stdout` sent nowhere, to syslog, to Elasticsearch, with the access logs on LOGS, HAProxy's own `log` over UDP and through a TLS ring | requests/s, janusd's and HAProxy's CPU, the lines HAProxy dropped (`DroppedLogs`), what the target sent and dropped, what the server kept, LOGS's write rate; then the queue drained |

## The scripts

The scripts and the raw logs stay in the lab's own repository: they
carry its addressing. One directory per campaign, with a README of what
ran:

- `under-load`, `runder-load`: a command under probes, with `wrk` on the
  VIP - the load client or the runners;
- `bench`, `rbench`: the throughput runs;
- `failover`, `rfailover`: node loss and HAProxy stopped;
- `reboot-time`: the reboots;
- `haproxy-address`: where HAProxy, keepalived and BIRD are mapped on
  each node;
- `rolling`: the drained rolling update through the Controller;
- `ctl-update`: the Controller's update with its probe;
- `factory-ready`: a node's page asks for the pre-release's bundle until
  the image factory has it;
- `new-machine`: a machine created by the Controller, its journal;
- `rollback-twice`: rollbacks timed from the call to the API back (gone
  first);
- `logfwd-setup`: the Controller's log forwarding and the lab's profile;
  `bench.sh` (scenario 16): log forwarding's cost, one configuration a
  run;
- `measure`, `rmeasure`, `probe`, `probe-summary`: the probes;
- `health`, `machine-health`: the nodes' health - a node with its own
  credentials, a Controller machine through its node page;
  `machine-reboot`: a Controller machine's reboots through its node page;
  `kernel-count`: each node's kernel messages in Elasticsearch, by host;
- for auto-scaling, the policy, journal and load helpers and the HAProxy
  configurations.
