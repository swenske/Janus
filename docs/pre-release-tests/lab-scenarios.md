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
