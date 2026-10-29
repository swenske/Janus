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
| KVM hosts | the first: Xeon E3-1220 v3 (4 cores), 16 GB, a RAID controller with its write cache; since v2026.10.24-6 a second: the same CPU, 32 GB, two plain SATA drives without a write cache (a synchronous write about 30 times slower). Both Debian 13, QEMU q35, Debian's OVMF 2025.02. Since v2026.10.25 the machines are spread over both: node 01, a backend and the Controller's own machines on the first; the Controller, node 02, the other backend and the screen-only node on the second |
| Router | RouterOS 7 (7.25 since 2026-10-26): the lab's VLANs, BGP with BFD (200 ms × 3) to the nodes; until v2026.10.26 no BGP ECMP (one node carried the anycast route at a time), since then BGP multipath - both nodes carry it, flows spread by addresses and ports (`l4`), the anycast address out of FastTrack ([BGP](../bgp.md#activeactive-behind-routeros-bgp-multipath)) |
| Node 01 and node 02 | 2 vCPU (host CPU), 1 GiB, OVMF without Secure Boot; a schematic with the five extensions (BGP, VRRP, nftables, node_exporter, the QEMU agent); management, front and backend networks |
| High availability | a VIP by VRRP (VRRPv3, 0.2 s adverts, HAProxy's health weighs -100) and an anycast /32 by BGP |
| The auto-scaled node | since v2026.10.14: a machine the Controller creates on the same host, 2 vCPU and 1 GiB from the release's default image, room for 4 vCPU and 2 GiB |
| Backends | two nginx VMs, 1 vCPU each |
| Load client | a 2-vCPU VM on the same host, with `wrk` - up to v2026.10.14, removed since: it took the host's CPUs from the nodes |
| The clients | since v2026.10.14-2, outside the lab's host, reaching the front network through the routers like internet users: the three CI runners (16, 12 and 8 threads), used only while none runs a CI job, and since v2026.10.14-3 a fourth machine that isn't a runner (16 threads, always free: the probes run there; on a 2.5 GbE link since v2026.10.16-4, its path changed - the release measured again; left out of the campaigns from v2026.10.17-2 to v2026.10.18-3 for a memory fault, the probes then ran from a runner; back with its memory replaced for v2026.10.19, where it ran the browser while the load and probes stayed on the runners, to compare). A temporary `wrk` on each, removed afterwards. The host's CPUs go to the nodes |
| Controller | Docker Compose with its updater, an admin with TOTP |
| Proxy | since v2026.10.23-2: Squid 6.13 on the Controller's machine, asking for a username and password, open to the lab's networks only |

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
  none in 4,680, for v2026.10.18-2: none in 4,678, for v2026.10.19-4:
  none in 5,062). A single isolated
  timeout
  outside the moment a scenario changes something is reported apart
  from the scenario's losses.
- **Load**: `wrk` (4.1.0). Keep-alive connections unless a scenario says
  otherwise.
- **The nodes**: the pre-release's own `janusctl` (from its `.deb`),
  the Janus exporter (`janus_boot_stage_seconds`), a node's
  `/proc/stat` read through its file API for the CPU split with steal
  time, the event log (`system events`).
- **The Controller's pages**: headless Chromium (the docs' pinned one),
  console errors and failed requests collected; since v2026.10.19 every
  palette, light and dark, over the node list (cards and rows) and a
  node's Overview, Metrics, Traffic flow and Storage, with axe (WCAG 2
  A and AA, serious and critical) - on the lab's Controller, whose
  nodes are real and include machines it made, which the demo has not.

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
  v2026.10.17-2 and v2026.10.18-2);
- since v2026.10.14-2, from the clients at once: 256 keep-alive
  connections each, 30 s, two runs (the three runners, and the fourth
  machine since v2026.10.14-3 - except v2026.10.17-2 and
  v2026.10.18-2).

Measured: requests per second and the 99th percentile, with the nodes'
boot stages. When the release's runs couldn't be taken before the
update, the nodes roll back to it for a few minutes (the other slot),
then forward again - the report says so.

### Failovers

On the node holding the VIP and the anycast route, probes for 60 s:

- **node loss**: the VM destroyed (`virsh destroy`), started again 20 s
  later;
- **HAProxy stopped** for 10 s through the API, then started;
- since v2026.10.24-4, under `wrk` on the VIP: **janusd restarted**
  (`janusctl system restart` on the VRRP master, the node page's
  Restart janusd on the backup) - the extension daemons listed before
  and after, one of each expected; and the node **shut down by its
  hypervisor through the guest agent** (`virsh shutdown --mode agent`,
  Proxmox's Shutdown), started again once off; since v2026.10.24-5 the
  restart from the master's node page too, and a machine alone (its own
  VRRP instance) restarted once its janusd has run over a minute -
  init doubles its restart delay for a process that ran less.

Measured: each address's outage, when the node goes and when it comes
back.

### Reboots

On node 02:

- through the firmware, twice;
- through kexec, chained, three times.

Each is timed from the Reboot call to the API answering again. The
kernel log is searched for an oops afterwards. Since v2026.10.25 the
firmware reboots run on node 02 four times and on node 01 twice, each
node's serial console timestamped on its host: the kernel's "Restarting
system", the firmware's boot manager, the next kernel and the API, and
any virtio-net failure in between - for the intermittent slow reboot.
Since v2026.10.25-3, a Controller machine without a variable store and
one with a store, on the same host at the same time: 45 firmware
reboots each through their node pages, each timed to HAProxy answering
again, their consoles timestamped through the Controller's console
stream ("Restarting system" to the firmware's boot manager); and the
new machines' libvirt definitions read on both hosts.

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
  every start;
- v2026.10.18-2: the diagnostic tools, on a configuration with a stick
  table, a map and fake passwords applied to both nodes for the
  campaign, then the lab's own put back - the configuration a reader's
  certificate reads (masked) against an admin's; the stick table and
  the sessions under `wrk`, an entry cleared; an invalid request
  carrying credentials, as HAProxy captured it; map lookups; the
  connectivity tests from the nodes to the backends, the Controller and
  the internet, from their own addresses, the VIP and the anycast
  address; a diagnostic bundle made by janusctl and one downloaded
  through the Controller, read for secrets, and the Controller's audit;
- v2026.10.18-3: a short check, its only change being one SELinux rule -
  the Controller, the image factory, the rolling update under load, the
  longterm machine, the nodes' health, and the connectivity tests given
  a source address that resolve a name, from each node's address, the
  VIP and the anycast address, with each node's AVC denials before and
  after;
- v2026.10.19: the Controller's new pages, every palette in both modes
  over the lab's seven nodes with axe (above), a stylesheet laid over
  them to try a fix on the same data before it's in an image; the
  fleet's curves from `GET /api/nodes/metrics`; and what an open page
  costs a node - its janusd's CPU time from `/proc` over a minute, with
  no page, Metrics at a sample a second and at five, and the node list;
- v2026.10.19-2: a short check of its Controller fixes - the same walk,
  nothing laid over the pages - with the update steps and the nodes'
  health;
- v2026.10.19-4:
  - the neighbour table on both nodes, through janusctl and the
    Controller, then the VIP's MAC seen from the backup before, during
    and after a drain of the node holding it;
  - haproxy.cfg's history through the Controller: a change applied, then
    back to the first version under the runners' probes, a reader's
    masked view and refused revert, the history read again after
    firmware and kexec reboots and a rollback to the release and back;
  - the pages showing them, in two palettes (Janus and Everforest).
- v2026.10.20: the Controller's serving of its pages - the Controller's
  own page and a node's page through it, with the session: each file's
  `Cache-Control`, `ETag`, gzip, a 304 on `If-None-Match`, a directory
  not listed, and the main bundle's bytes on the wire, plain and gzipped
  (`s-static`); the rest of the campaign against the release measured
  the same morning, the nodes' janusd being the one with the faster
  paths;
- v2026.10.20-3: the console's share, under load - a node's HAProxy
  logging every request to its stdout under `wrk`, init's and janusd's
  CPU with the share at its default, at 0 (set with `janusctl`, read
  back through the Controller's relay and the exporter), then at the
  default again (`x-budget`); and a node without a serial console - a
  machine made by hand on the lab's host from the screen-only
  schematic's image, provisioned by NoCloud, approved, read through
  the Controller's relay since nothing prints on its serial port: its
  serial log's size, its version and health, a screenshot of its
  screen, then the same load with init's CPU (`screen-machine`,
  `x-screen-load`); from v2026.10.21 the image is the factory's - no
  node runs that schematic, so the script asks for the build first;
- v2026.10.22: the node pages' shared assets - two nodes' pages through
  the Controller naming the same `/node-app/` script, its caching
  headers, a 304, public while a node's API isn't (`s-static-node-app`);
- v2026.10.22-2: single sign-on on the lab's Controller - a Keycloak (the
  image CI's test pins) started on the Controller's own host with a
  certificate of a CA made for the test, the provider set up through the
  API and checked with **Test**, then headless Chromium through
  Keycloak's pages: a reader and an admin made by the groups, one
  refused, signing out of Keycloak too, Keycloak's second factor counted
  with an `acr`; the settings file's owner, mode and secret on the
  Controller's volume; everything removed afterwards (`sso-keycloak`,
  `sso-check`). And the minute-long update reboot looked for: the
  rolling update to the other slot and back under load, both nodes'
  serial consoles timestamped on the lab's host as they're written
  (`x-slow-reboot`).
- v2026.10.23: where the exporters listen after the update - every
  address kept, the `listen-default` marker - and on the main interface
  once asked (`exporters`, then `-address auto` on node 02 and back);
  each connection's byte counters under `wrk`, Netstat read twice
  through the node's page (`netstat-load`); janusd's reboot lines in the
  lab's log server (`reboot-steps`); and an A/B throughput run, node 01
  on each slot minutes apart (`ab-bench`).
- v2026.10.24: clusters of the two nodes made by the Controller, below;
  a node's state carried to a new machine - the longterm machine given a
  configuration, a map and a kernel parameter for it, exported into a
  machine the Controller made, compared, the machine destroyed; the
  accept queues' memory suggestion's listeners; and HAProxy's reload
  under load on a node on its own, four reloads twice, on the
  pre-release and on the release (node 01 on each slot minutes apart,
  `reloads`).
- v2026.10.24-2: the clusters' in-sync under load and a member removed
  with the ticket keys checked by TLS resumption (below); a member of a
  cluster of Controller machines recycled under load (Recycling).
- v2026.10.24-3: a **full campaign**, every scenario and every feature
  at the user's request: the factory's every-extension, HAProxy 3.0 +
  longterm and HAProxy 3.2 images on demand; on a machine with every
  extension, network and firewall trials left to revert, VRRP, the BGP
  gate, Consul's validation, kernel parameters (trial, cancel, timeout,
  refusals, a reboot), LOGS and the audit, the exporters' addresses, the
  HAProxy history, runtime maps/ACLs and SNI certificates, the console's
  share, its state carried into a machine of another image; Let's
  Encrypt's four paths (as v2026.10.23-2); auto-scaling and two OOM kills
  on a plain machine; the Controller's 9 tabs and a full node's 36
  pages in two palettes and both modes with axe on WCAG 2 A/AA,
  accounts and roles, single sign-on (as v2026.10.22-2), scoped tokens,
  enrollment tokens, a machine resized, re-addressed, moved to another
  image and locked, the release's Terraform provider with OpenTofu,
  backups to a bucket of their own, `janusctl`'s sign-ins; the clusters
  from creation to deletion, a member removed and joining again, and
  the recycle under load.
- v2026.10.24-4: janusd restarted on a machine with every extension
  (its own VRRP instance, no peer), on the VRRP master and on the
  backup from its node page, and the master shut down by its
  hypervisor through the guest agent (Failovers); the clusters' in-sync
  under the three runners at once; the six node pages v2026.10.24-3's
  axe run had findings on; a cancelled kernel parameter trial; a quick
  clusters pass with the recycle under load.
- v2026.10.24-5: the restart and the hypervisor's shutdown again, with
  the drain first (Failovers); `-h` on the power commands; clusters
  made on the hypervisor and scaled, and auto-scaling, below.
- v2026.10.24-6: the hosts' identities after the update (their SSH host
  keys) and the machines' views; the clusters' anti-affinity on the
  lab's two KVM hosts, a VRRP version under auto-scaling, below; the
  slow reboot chased with the serial console timestamped on the host.
- v2026.10.28-2: the campaign after two pre-releases without one, against
  the release; HAProxy's throughput with node 01 alternating between
  both versions and its CPU time by mode per request (`cpuab`); Consul
  joining a Consul server of the same version run next to the
  Controller for the test; the libvirt preparation's UEFI firmware step
  on the lab's hosts; the longterm and Let's Encrypt machines updated
  through the Controller.
- v2026.10.29: the cluster's update in place under load in place of the
  rolling update, and a drained member rebooted (Clusters, below); two
  machines on networks without DHCP, one per version, their boots read
  from the console through the Controller; a machine with the guest
  agent and its management network in DHCP mode; the new VRRP and BGP
  counters read from both nodes' exporters; headless Claude Code with
  the plugin's hook against a node, a reader certificate; HAProxy's
  throughput again with node 01 alternating both versions (`abrun`).

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
| A cluster's records | since v2026.10.26: a cluster's members (given a gateway on the management network to reach the targets) and the nodes in none; a Debian rsyslog (`hack/rsyslog`) beside the Controller added to the profile for the test | the cluster's fields in Elasticsearch, by host; the RFC 5424 structured data as rsyslog parsed it; the profile put back |

### Let's Encrypt without the internet

Since v2026.10.23-2. A machine the Controller creates from the
pre-release with the Let's Encrypt extension, worked through its page
only (it has no credentials of its own). Its internet is cut at the
lab's router (its new connections to anything outside the lab
rejected). The CA is a private Pebble on a machine outside the lab,
reachable from inside only on the Controller's own host through a tunnel
- the node can never reach it directly, the proxy and the Controller
can; its DNS test server answers DNS-01 behind an `httpreq` provider.
Two certificates: two names over HTTP-01, a wildcard over DNS-01.

| Step | How | Measured |
|---|---|---|
| The checks | the page's **Test**, each way, against Let's Encrypt's own directory and the private CA: directly, the proxy without and with its password, the Controller's relay | what each says: reached in how long, or why not |
| Through a proxy | the configuration with the proxy's URL and credentials | time to both certificates, served and checked against the CA's root; the proxy's log (tunnels, the DNS API's calls, the user) |
| Through the relay | the relay turned on (its address, the CA allowed), the path chosen on the page, both renewed | a new serial served; the relay's journal; what fails and why |
| Ordered by the Controller | its account and DNS provider on its tab, the path chosen on the page, a renewal asked for | time to both installed, new serials served; the node's and the Controller's logs; no tunnel through the relay |
| The node and the pages | its health afterwards; both Let's Encrypt pages with the real data | AVC denials, the CIS audit; both themes, axe, console |

### Clusters

Since v2026.10.24. The lab's Controller makes a cluster of node 01 (the
source, holding the VIP and the anycast route) and node 02 through its
API, as its Clusters page does; under load, `wrk` on the VIP from one
runner and the probes from another. Each member's stick table is read
through `janusctl` along the way, its counts compared with the requests
`wrk` and the probes made.

| Step | How | Measured |
|---|---|---|
| Created | both nodes, their front addresses | time to made; the locks (a change straight to a member refused) |
| A stick table | a version adding a table replicated over the peers (`peers janus`, `track-sc0 src`) | the backup's table against the master's |
| Rolling apply under load | a version changing a timeout | its time; requests lost; `wrk`; the tables' counts carried on |
| A member destroyed under load | the master destroyed, started again | outages; the backup counting on; when the master has the table back, against when it takes the VIP back; the final counts against `wrk`'s |
| A member rebooted | the backup, through the firmware | HAProxy's stop (janusd's line), API back, the table learnt back |
| Drained through the Controller | the master drained, then undrained, under load | time to drained, requests lost, the cluster's problems |
| VRRP from a template | each member's priority as a variable, the master's `keepalived.conf` templated, applied managing VRRP | each member's rendered configuration against the one written by hand; the VIP; the lock |
| Applies with and without VRRP | rolling applies of a `haproxy.cfg` change under load, VRRP managed then not | requests lost, `wrk`'s worst latency and errors |
| In sync under load | `wrk` through the VIP, each request a stick-table update; each member's `cluster status`, its exporter's `janus_cluster_peer_table_synced` and the Controller's check, sampled during the load and after (since v2026.10.24-2) | in sync or "catching up", member by member, under load and idle |
| A member removed under load | the backup removed through the API, with a TLS frontend naming the ticket key ring on both and a session saved from the master before (since v2026.10.24-2) | time, requests lost; the rings' SHA-256 after - the departed node's own, the master's renewed; the session resumed nowhere; the departed node's drain, as it reports it |
| Updated in place under load | `POST /api/clusters/{id}/upgrade` from the previous release to the pre-release, `wrk` and the probes running (since v2026.10.29, in place of the rolling update) | requests lost, each member's drained-to-back time, the journal's steps, the tables and slots after |
| A drained member rebooted | the backup drained, rebooted through the firmware, the router's routes read every 0.75 s until undrained (since v2026.10.29) | the member's VRRP state and BGP protocols at boot, the router's routes, which node the probes reached |
| Deleted | | time; the nodes on their own - each with a ring of its own -, the lab's configuration put back |
| The page | the Clusters list and the cluster's tabs with these data | both themes, axe, console errors, failed requests |

A `janusctl` fleet's cluster isn't run in the lab: its nodes trust the
Controller's fleet (the report says why).

### Recycling

Since v2026.10.24-2. The lab's Controller makes two machines from the
pre-release, each with one interface by DHCP (a recycle clones a
machine's interfaces; a static address can't be cloned), and a cluster
of them: a stick table, a TLS frontend naming the ticket key ring, VRRP
from a template with a VIP of their own on the management network.
`wrk` on that VIP from a runner, the probes from another, then the
master recycled through the API as the page does.

| Measured | How |
|---|---|
| The recycle's time | from the call to the cluster idle |
| Requests lost on the VIP | the probes; `wrk`'s errors |
| The VIP moving once | who served each probe; each member's keepalived log, its MASTER transitions before and after |
| The stick table on the newcomer | its count against the requests sent (`wrk` + the probes) |
| The old machine | gone from the Controller and the hypervisor |
| The ticket keys | the remaining members' rings shared and renewed; a session from before no longer resuming |
| The journal, the page | each step journaled; the Members tab and its Recycle form in headless Chromium, both themes, axe |

### Clusters on the hypervisor, and scaling

Since v2026.10.24-5; on two KVM hosts since v2026.10.24-6 (the second
host added between the two campaigns):

| Step | How | Measured |
|---|---|---|
| The hosts' identities | each record's `host_id` after the Controller's update, against each host's SSH host key (`ssh-keyscan`); the machines' views (`GET /api/machines`) and their events | the form, distinct per host; no move journaled |
| Two records, one host | a second record to the first host's SSH server (another address), its key added to the Controller's account there and removed after | both records' host identity |
| Anti-affinity required | three new machines asked on the records of both hosts (one host per machine: one too many); then two | three refused before anything is made, the hosts named; two placed one per host, the libvirt domains where the journal says |
| A recycle, a member more | with required: a member recycled; three members asked for | where the new machine went; the refusal and its reason |
| Anti-affinity preferred | the cluster made on both records | its time; the placement journaled; the page's same-host problem, each member's host |
| A recycle | a member recycled | where its machine went, the journal |
| Scaling | a version answering on port 80 with a stick table; the members set to 3 then 2 while two runners load one member each (`wrk`, as a balancer in front would), probes of both | the times; `wrk`'s errors, lost probes; the new member made like the newest; the removed one's machine destroyed |
| Auto-scaling | the policy at its shortest windows (1 min out, 5 min in, 1 min cooldowns); `observe`, then `on`, under the runners' load on the members (one vCPU each); then the load stopped | the load as the engine reads it; its journal (would add, adding, taking out) and the times; the member added's share of the traffic |
| A VRRP cluster | a version with a keepalived template applied with auto-scaling observing, then on (409 since v2026.10.24-6), then off; then auto-scaling asked | the refusals; the VIP answering once applied |
| The pages | the cluster's page and the New machines form, two palettes, both modes, axe | page and console errors, findings |
| Placement by memory | since v2026.10.25: clusters of new machines on both hosts' records, the smaller host's listed first - anti-affinity off (three machines), preferred (three), a machine only the bigger host has room for, requests no host can take (required and off); a member recycled in each mode | where each machine went against each host's memory available; the refusals' words, nothing made; the recycle's host against the members' |
| Terraform's `janus_cluster` | since v2026.10.25: OpenTofu at CI's pinned version, the provider from the pre-release's archive, a token made for it: two members required; three (refused on two hosts); the anti-affinity changed (a replacement); members 2 → 3 → 2; `autoscaling` set and removed; destroy | each apply's time and outcome, where the machines went, a plan with nothing to change after each step, the machines gone after destroy |
| The policy's durations | since v2026.10.25: `PUT .../autoscale` with the `*_seconds` names, then with the nanosecond names older Controllers used | what `GET` gives back |
| A balancer in front | since v2026.10.25-2: an HAProxy in a container beside the lab's Controller, its servers set only by the balancer hook through a receiver that checks each call's signature and applies it through HAProxy's runtime API; `wrk` (keep-alive, 25,000 requests a second) and a probe through it, from the machine that isn't a runner; a member drained and undrained, the members set to 3 then 2, a version applied (every member reloading); the receiver answering 503; the balancer with and without retries on empty responses | each call against the journal's drain and removal (told before, the drain delay); each member's share in the balancer's counters; `wrk`'s errors and the probes; the refusal, the cluster's problem, the next check calling again |
| Terraform's balancer block | since v2026.10.25-2: `janus_cluster` with `balancer` (the receiver above, its secret a sensitive variable); the drain delay changed; the block removed | the receiver's calls; a plan with nothing to change after each step (the secret never read back) |
| The Controller's metrics | since v2026.10.25-2: `GET /api/metrics` with a reader API token, under the load above with auto-scaling observing | the families; the load figures against the cluster's view and each member's own HAProxy idle on its page |
| A cluster deleted with its machines | since v2026.10.25-4: a cluster of new machines on both hosts deleted from its page with "Delete the members' virtual machines too" ticked; another without | the dialog, the call; the cluster and machines gone, and when; both hosts' domains, the pool's volumes and the variable stores against before (nothing else gone); the audit; the machines kept without the box |

### Replacing a node, node to node

Since v2026.10.25-2, on three fresh Controller machines: one given a
configuration of its own (a marker in `haproxy.cfg`, a file, a kernel
parameter confirmed), its state pulled by another with `janusctl -n NEW
system state import -from OLD`, then by a third with `-identity`.

| Measured | How |
|---|---|
| What arrived | the new node's configuration, its answer, the file's SHA-256, the kernel parameter |
| The identity | the CA the new node's API certificate is issued by, before and after - its own without `-identity`, the old node's with it |
| The way it went | both nodes' logs: the grant (until when, the identity or not, by whom), the export by the transfer certificate, the import |
| The refusals | a grant asked with a reader's certificate; a second import onto a node already in use |

### A cluster from a node

Since v2026.10.25-3, as the migration of a load balancer already
serving: node 01 given a map, an error page and an uploaded certificate,
then **New cluster → New machines → Start from** node 01 on the
Controller's page, in a browser (two machines, anti-affinity required,
both hosts).

| Measured | How |
|---|---|
| The time | the call to the cluster made, from its journal |
| The node untouched | its `haproxy.cfg`'s SHA-256, files and certificates before and after; in no cluster |
| The way it went | the node's log and audit: one grant (by whom, until when, the identity or not), one export by the machine's transfer certificate, from the machine's own address |
| What the members got | their `haproxy.cfg` against the node's, the files' SHA-256, the certificate loaded; each one's CA, hostname and address its own |
| A `bind` on the node's own address | the node given one, a one-machine cluster seeded from it: refused or made, what the member listens on; the node's configuration put back |

### Let's Encrypt in a cluster

Since v2026.10.25-3, on the cluster above: a private ACME CA (Pebble)
behind the lab's proxy, its DNS answering every name with the member
that **doesn't** order, so each HTTP-01 challenge is answered by it. Two
certificates (HTTP-01 and DNS-01) configured on the secrets' source
through its page; versions adding the HTTP-01 rule, then a TLS frontend.
Since v2026.10.25-5, the CA is pointed at another member through the
DNS server's management API, without restarting it: the count of
certificates it issued then covers the whole test.

| Measured | How |
|---|---|
| Who orders | each member's Let's Encrypt status (ordered by, the account it answers challenges with), the journal |
| A renewal asked of the member that doesn't order | refused, and how |
| Pulling | a renewal forced on the orderer, **Check now**: the journal, the serial each member serves |
| The handover | the secrets' source changed on the page, **Check now**: who orders, the CA's issued certificates, each member's challenge answer; the CA pointed at the old orderer and a renewal asked of the new one |
| The stick tables across it | since v2026.10.25-5: a version with a stick table replicated over the cluster's peers (a string key from the query, `http_req_cnt`), a few hundred keys sent; each member's count before and after the handover, and each HAProxy's restart time |

### A RouterOS router in front

Since v2026.10.25-3, on the lab's router, for the campaign only (taken
back off after): the REST service with a self-signed certificate open to
the Controller's address, a group and a user as the guide says, an input
rule for the Controller. The members given a test /32 and a frontend
answering their name.

| Step | How | Measured |
|---|---|---|
| The routes | the balancer set (`routeros`, the fingerprint, `check_gateway`) | the routes and their flags on the router, the journal |
| The spread | new connections from the four clients, the router's hash `l3`, then `l4`, then `l4` with the /32 out of FastTrack | each member's share per client; connections reset |
| A drain | `wrk` (keep-alive) from a runner and new connections every 50 ms from another; a member drained then undrained; then each alone | the journal's order (told, then drained); lost probes; `wrk`'s errors against a run without a drain |
| Drift | a route deleted by hand, another changed, **Check now**; a route of the router's own to the same prefix | the journal; the routes after |
| A member lost | its machine forced off, not drained, probes running | how long its share failed; its route's flags; back once started |
| Removed | the balancer removed | the routes left |

### A cluster's members on the pages

Since v2026.10.26: a cluster of new machines on both hosts with
keepalived, a version with a virtual IP (the members' priorities as
template variables) and a stick table, managing HAProxy, its files and
VRRP. In a browser, as an admin:

| Step | Measured |
|---|---|
| A member's top bar | the badge's words, its link followed, the VRRP role |
| The pages of what the cluster manages | the lock notice, the buttons offered |
| A write through a member's page API | the node's refusal, the Controller's answer |
| An admin's release from the page, a change, **Check now** | the cluster's problems and journal; `janus_cluster_member_drift` and `_released` |
| The version applied again | the drift gone, the lock and the notice back |
| The master's HAProxy stopped, then started | the address's move, each badge's role after |
| The Nodes page | the cluster's frame in cards and in the list |

### The Controller's metrics and the alerts

Since v2026.10.26: a Prometheus on the build box (the image the
repository's `promtool` checks use) scraping the cluster's members'
Janus exporters and the Controller's `/api/metrics` with a reader API
token, `examples/prometheus/janus-rules.yml` loaded, every 5 s.

| State | Measured |
|---|---|
| A member released and drifted | each `janus-clusters` alert's state |
| A member's HAProxy stopped | when JanusClusterPeerDown goes pending and fires; the others; the Controller's peers figures after a check |
| A member's HAProxy stopped, a check | `janus_cluster_member_error` and the figures it can't read |
| Back in step | all inactive |

### Settings

Since v2026.10.26, in a browser, each choice made on the Settings page:
the Nodes page's requests counted over 30 to 65 s (the refresh, an
admin's floor, the figures off and on), its units (bits), a node page's
times (UTC); reset to the defaults. The Hypervisors tab's lists with the
lab's two hosts.

### The Traffic flow's rates

Since v2026.10.26, on a cluster's member: a TCP frontend relaying a
100 MB file served beside the Controller, downloaded at 2 MiB/s from
outside, the frontend without then with `option contstats`; the page
read every 2 s in a browser - the frontend's, its backend's and the
interface's rates, the hourglass and its tooltip; the dots' animations
tagged, then counted again after 12 s of refreshes; reduced motion.

### A network change on the VRRP master

Since v2026.10.26-2: under probes of the VIP and the anycast address
(50 ms, from the fourth client), a network configuration applied on the
node holding the VIP - an address no one announces added to its front
interface, taken away 12 s later; keepalived's "reports IP … removed"
lines counted on that node before and after. Measured: the lost probes,
VRRP's transitions at the applies. The newest release first.

### The same IPv6 anycast address on both nodes

Since v2026.10.26-2, for the campaign only (taken off afterwards, the
router's export compared with the one from before): a ULA `/64` on the
front network and one on the backends', on the router; BGP over IPv6
with both nodes (`afi=ipv6`, BFD, the lab's multipath, IPv6 hashed by
ports), only `/128`s from the anycast prefix accepted, IPv6 BGP/BFD let
in and the backends let through to the anycast address; on each node an
address on the front `/64`, the same `/128`, an IPv6 gateway, an IPv6
session exporting a `haproxy_*` static route, HAProxy on IPv6.

| Step | Measured |
|---|---|
| The configuration in place | each node's `/128` flags (`/proc/net/if_inet6`: nodad, dadfailed, tentative); the router's routes to it, the sessions, BFD |
| A backend VM asking the anycast address through the router every 50 ms for 30 s, node 01's HAProxy stopped 8 s in the middle | failed requests, outages, which node answered |
| The rolling update with it in place | it survives the update (both nodes' configuration kept) |
| Since v2026.10.26-3: each node's own TCP connections to a backend address the source rule ties on (as few leading bits shared with the nodes' `/64` as with the anycast prefix), routed by the router | the connecting socket's local address; failed connections |

Set on the newest release first, then measured again once the rolling
update took both nodes to the pre-release.

### A server drained, HAProxy reloaded

Since v2026.10.27-2: under probes of the VIP and the anycast address
(50 ms, from a runner) and `wrk` on the VIP (8 keep-alive connections,
another runner), on the node holding the VIP, `janusctl haproxy
server-state` on one backend server - drain 20 s, ready 10 s, maint
20 s, ready -; then that server drained and `janusctl haproxy reload` on
that node, then on the other. Measured: the lost probes, which backend
server answered each VIP probe in each phase (the probe logs it), `wrk`'s
errors, the state janusctl prints, whether the drained server is back
after the reload, the configuration's history (a reload adds no
version).

### Claude Code with the janusctl skill

Since v2026.10.27-2: the plugin installed from this repository on
GitHub in an empty Claude Code configuration (`claude plugin marketplace
add` + `install`, and the one-step `install --marketplace`), then
headless Claude Code (`claude -p`, the skill's page's ask rules,
`janusctl *` allowed, no janusctl context) given client certificates the
node holding the VIP issued for two hours (`pki generate-client-config`,
a reader's and an operator's, deleted after):

| Asked | With | Expected |
|---|---|---|
| Is the lab's load balancer healthy? | the reader's | reads only; what the role keeps from it said, not worked around |
| Drain a server | the reader's | the node refuses; Claude reports it, tries no other credential |
| Take a server out, check, put it back | the operator's, under probes and `wrk` | `server-state` drain and ready, checked with `haproxy backends`/`stats`; no lost probe |

Measured: each run's tool calls (Claude Code's stream), the node's audit
(who called what, the refusals), the probes.

### Hypervisors' labels

Since v2026.10.29-2: the Controller's two hypervisors labelled (`dc`,
`room`) through `PATCH /api/hypervisors/{id}` with their whole record;
then:

| Step | Measured |
|---|---|
| `/api/nodes` | `inherited_labels` and `inherited_from` on the machines' nodes, none on the nodes made by hand |
| A machine's own label on a key it inherits | its own kept apart; the card's inherited badge struck through |
| A reader API token scoped to `dc=lab` | the nodes `/api/nodes` lists, each node page's answer (200 or 403), its `janusctl login` context and what a write gets |
| The hypervisor relabelled, a node given the label itself, back | the same, after each change |
| The Hypervisors and Nodes pages | in headless Chromium, light and dark: console errors, failed requests, axe |

### Consul through the Controller

Since v2026.10.29-2, for the campaign only: a Consul server (the
extension's version, pinned by digest) in a container on the
Controller's machine, set up as the guide's "Before you start" asks -
ACLs denying by default, gossip encryption, RPC TLS verified both ways,
`auto_encrypt`, the HTTP API on HTTPS only with its own CA - with the
Controller's token (the guide's policy) and a read-only token. Three
machines the Controller makes: two of them a cluster (a configuration
routing by a hosts map and a Host ACL, HAProxy's Prometheus exporter on
a frontend) given `consul` and node_exporter by `PATCH /api/machines`
- through the cluster's update -, the third made with them.

| Step | Measured |
|---|---|
| **Test** with the right token, a read-only one, no CA | what it says |
| The settings saved, a selector on an inherited label | when each node gets its token and layer; the Nodes table |
| Each node, in Consul and with `janusctl network consul managed\|services\|status` | its token (node identity, `janus-nodes`), its agent, `node_meta`, each service's address, port, tags, meta and checks |
| A map entry set and deleted at runtime; an exporter off, on, moved | the seconds until Consul follows (the catalog read every 0.2 s) |
| Drain, undrain; HAProxy stopped (with and without a map); a firmware reboot | maintenance on and off, the seconds; the services after (no duplicate) |
| A hypervisor's label changed | the node's `node_meta` |
| A node token deleted in Consul, **Run now** | the new token, the catalog |
| A node out of the selector and back; a machine destroyed; the integration off | the layer, the token, the catalog entry, the agent |
| The Consul tab, a node's **Apps › Consul** | in headless Chromium, light and dark |
| The cost | janusd's and the agent's CPU and memory, idle, with and without the extension; the boot stages; the images' sizes |
| Since v2026.10.29-3: the Controller's Consul pass with nodes on an earlier release, picked or not | its `last_pass` read every 5 s: how often it comes back |
| Since v2026.10.29-4: the cluster's configuration in production's shape - a TCP `:443` splitting TLS by server name (one host passed through, the rest over `abns@` to an HTTP frontend reading a hosts map through a variable, a Host ACL), a TCP frontend refusing other sources -, on the release then the pre-release | the URLs registered, their tags and meta (`via`, `scheme=`, `proto=`); each frontend's check over minutes (the agent's output read through the node's page); a frontend disabled by a version; HAProxy stopped; janusd restarted |
| Since v2026.10.29-4: the cluster updated on another image (one more extension), a machine updated by Terraform | when the image factory's build happens against the member's drain and its machine's phase; `apply` and the next `plan` |

### Prometheus finding them

Since v2026.10.29-2, with the Consul scenario's nodes: a Prometheus and
a blackbox exporter in containers on the fourth machine, with
`examples/prometheus/consul.yml` adapted to the lab's Consul (its
address, its CA, the read-only token). Measured: the targets found and
`up`, their labels, the URL targets (one per URL, a cluster's members
merged) and their probes, which targets a drained member leaves -
both members drained - and a node out of the selector.
Since v2026.10.29-4, the example's `tags: [url, proto=http]`
against `tags: [url, proto=tcp]` and the `filter` the notes suggest.

## The scripts

The scripts and the raw logs stay in the lab's own repository: they
carry its addressing. One directory per campaign, with a README of what
ran:

- `under-load`, `runder-load`: a command under probes, with `wrk` on the
  VIP - the load client or the runners;
- `bench`, `rbench`: the throughput runs;
- `failover`, `rfailover`: node loss and HAProxy stopped;
- `reboot-time`: the reboots; `reboot-steps`: their timing lines in the
  log server (v2026.10.23);
- `new-machine` (`EXTENSIONS=`, `KERNEL=`), `update-machine`: a
  Controller machine made or updated through the API; `le-ca`: the
  private CA and its tunnel; `le`: the Let's Encrypt steps through the
  machine's page (v2026.10.23-2);
- `d-diag`: the diagnostic tools on both nodes (v2026.10.18-2);
- `u-check`: the Controller's pages in every palette, with axe (`CSS`:
  a stylesheet laid over each page); `u-cost`: a node's janusd CPU time
  with a page open (v2026.10.19);
- `haproxy-address`: where HAProxy, keepalived and BIRD are mapped on
  each node;
- `x-budget`: the console's share set to 0 and back under load, each
  way it shows; `screen-machine`, `x-screen-load`: a screen-only node
  made by hand, read and loaded through the Controller's relay
  (v2026.10.20-3); `s-static-node-app`: the node pages' shared assets
  (v2026.10.22);
- `n-neighbors`: both nodes' neighbour tables, and the VIP's MAC from
  the backup around a drain (v2026.10.19-4);
- `h-history`: haproxy.cfg's history through the Controller - a change,
  back to the first version, optionally under probes -, and a reader's
  view (v2026.10.19-4);
- `rolling`: the drained rolling update through the Controller (since
  v2026.10.19-2 it trusts its own wait and names the check that fails);
- `ctl-update`: the Controller's update with its probe;
- `sso-keycloak`, `sso-check`: a Keycloak next to the lab's Controller,
  and single sign-on through it in the browser (v2026.10.22-2);
- `x-slow-reboot`: the rolling update there and back with the nodes'
  consoles timestamped (v2026.10.22-2);
- `exporters`: where each node's exporters listen and the marker;
  `netstat-load`: per-connection counters under load; `reboot-steps`:
  janusd's reboot lines from Elasticsearch; `ab-bench`: the runners'
  throughput run with node 01 on each slot (v2026.10.23);
- `cluster-lab`: a cluster of the two nodes through the Controller's API
  - created, versions applied (`MANAGED=` what it manages from then on),
  VRRP from a template, drained, its view, journal and the members'
  tables, deleted; `cluster-failover`: a member destroyed and started
  again under `runder-load`, the tables read along the way; `reloads`:
  four HAProxy reloads on a node under `runder-load`; `ucheck.mjs`: the
  Clusters pages in headless Chromium; `bin/peers-raw`: a node's
  `ClusterStatus` with HAProxy's raw counters (v2026.10.24);
- `insync`: the members' in-sync from every view (status, exporter, the
  Controller's); `peers-lag`: node 01's raw peer counters a second
  apart; `tls-resume`: a TLS 1.2 session saved from a node, then whether
  it resumes on others; `rings`: the nodes' ticket key rings;
  `remove-member`: a member removed through the API, timed;
  `new-dhcp-machine`: a Controller machine with one interface by DHCP;
  `rec-cluster`: the recycle cluster - created, its VRRP version, a
  recycle, its view and journal, deleted; `runder-load-vip`:
  `runder-load` on another VIP (`VIP=`, `URLS=`) (v2026.10.24-2);
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
- `plc.sh`: helpers for a cluster made on the hypervisor (its view,
  journal, `scale`); `s-scale`: the members set to 3 and 2 under load on
  each member; `s-auto`: auto-scaling observe, on and quiet under the
  runners' load; `s-vrrp`: a VRRP version and the refusals;
  `u-check40` (+ `.mjs`): the cluster's page and the New machines form
  in headless Chromium (v2026.10.24-5).
- `hb-up`, `hb-down`, `hb/receiver.py`: the balancer in front on the
  Controller's machine and its hook receiver; `hload`: a command under
  `wrk` and a probe through it; `cl`: a cluster's drain, scale, check,
  view, journal and the receiver's log; `hbstat`: the balancer's servers;
  `cv2`: a version with one comment more, applied; `m-sample`,
  `m-members`: the Controller's metrics against the view and the
  members' pages; `node-ca`: the CA a node's API certificate is issued
  by; `tf/`: `janus_cluster` with its balancer block (v2026.10.25-2).
- `nm-master`: a network change on the VRRP master under probes,
  keepalived's lines counted; `v6-router`, `v6-nodes`, `v6-check`: the
  IPv6 anycast test's router and nodes (`apply`, `revert`) and its
  measure; `rolling` with `BGP=` a directory of `bird.conf` files, its
  drain withdrawing every `haproxy_anycast*` route (v2026.10.26-2).
- `v6-check` also probes the nodes' own IPv6 connections and reads
  their local address (v2026.10.26-3).
- `s1-steps`: `server-state` drain, ready, maint, ready on the node
  holding the VIP, each step stamped; `s2-steps`: a server drained, then
  `haproxy reload` on each node (both under `runder-load`);
  `claude-runs/`: the headless Claude Code runs' streams and their tool
  calls (v2026.10.27-2).
- `cpuab`: node 01's CPU time by mode (its exporter) around a `wrk` run
  of new connections and one of 256 keep-alive connections, per request;
  `consul54`, a Consul server in a container next to the Controller
  (v2026.10.28-2).
- `cluster-lab upgrade VERSION`: the cluster updated in place, each
  member's stage printed as it changes; `drain-reboot MEMBER
  firmware|kexec`: a member drained, rebooted and watched (VRRP, BGP,
  the router's routes) until undrained; `boot-console NAME SECONDS OUT`:
  a Controller machine reset with its console recorded; `new-machine`
  with `NOMGMT=1` (front and backend only) or `MGMT_DHCP=1`; `snap DIR`:
  both nodes' configurations, to compare with the campaign before;
  `abrun N`: `cpuab` on node 01, alternating versions by rollbacks
  (v2026.10.29).
- `k1`: hypervisors' labels, a node's own, what `/api/nodes` says and
  what a scoped token reaches; `consul56`: the throwaway Consul server
  (`up`, `bootstrap`, `down`); `consul-settings`: the Controller's
  Consul settings tested, saved, turned off; `kc`: the scenario's
  cluster (its versions: the map, then the configuration); `mpatch`: a
  machine changed through the API, its phase and its cluster's members
  followed; `k2`: Consul's view - members, nodes, a node's services and
  checks, the URLs, the tokens, the Controller's table, and `watch` (a
  service's presence and health every 0.2 s); `cost`: a node's
  processes' CPU and memory over two minutes; `k3`: the Prometheus and
  the blackbox exporter; `u-check56` (+ `.mjs`): pages in headless
  Chromium, light and dark (v2026.10.29-2).
- `hstop NODE SECONDS`: HAProxy stopped and started through janusctl,
  each of the node's `janus:*` services watched in Consul;
  `bbtargets`: the Prometheus's URL targets, the node each one was
  taken from and its health (v2026.10.29-3).
- `kc-upgrade VERSION EXTENSIONS`: a cluster updated on another image,
  its members' stage and drain, their machines' phase and the journal
  printed as they change; `agentcheck NODE [ID]`: the node's agent's own
  checks and their output, through its page; `tf/`: a machine made and
  updated by OpenTofu (v2026.10.29-4).
