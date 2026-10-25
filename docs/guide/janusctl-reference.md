<!-- Generated from cmd/janusctl/commands.go: go test ./cmd/janusctl -run TestReferenceDoc -update -->

# janusctl reference

Every janusctl command, its arguments and its flags - generated from the
command tree janusctl itself runs, completes and prints its help from.
How to install it, sign in and reach nodes: [janusctl](../janusctl.md).

## Global flags

They come before the command:

```text
janusctl [-context NAME] [-n NODE[,NODE] | -all] COMMAND ...
janusctl -endpoint HOST:PORT -ca FILE -cert FILE -key FILE COMMAND ...
```

| Flag | |
|---|---|
| `-context NAME` | the context to use (default: the current one) |
| `-n NODE[,NODE]` | the node(s) to run the command on - '?' to pick |
| `-all` | run the command on every node of the fleet |
| `-endpoint HOST:PORT` | a node's API, with its own certificate |
| `-ca FILE` | the node's CA certificate |
| `-cert FILE` | the client certificate |
| `-key FILE` | the client private key |
| `-as-user NAME` | with a Controller certificate: the user the calls are made for |
| `-as-roles ROLES` | with -as-user: that user's roles, comma-separated |

## Sign in

### janusctl login

Sign in to a Controller: a certificate of its fleet, and its nodes.

```text
janusctl login [-context NAME] [-controller HOST[:PORT]] [-controller-ca FILE] [-controller-fingerprint SHA256] [-user NAME] [-ssh-key FILE] [-browser] [-no-open] [-device]
```

Runs on this machine: no node is contacted.

| Flag | |
|---|---|
| `-context NAME` | the context to sign in |
| `-controller HOST[:PORT]` | the Controller's address (the first time) |
| `-controller-ca FILE` | check the Controller against this CA (or certificate) |
| `-controller-fingerprint SHA256` | trust the Controller's certificate with this SHA-256 |
| `-user NAME` | your account (with an SSH key) |
| `-ssh-key FILE` | the SSH key: its private key file, or its .pub for ssh-agent |
| `-browser` | sign in on the Controller's page, in the browser |
| `-no-open` | with -browser: only print the page's address |
| `-device` | sign in on the Controller's page from another machine |

### janusctl context

The Controllers and fleets signed in to.

```text
janusctl context [list | use NAME | delete NAME]
```

Runs on this machine: no node is contacted.

### janusctl nodes

The context's nodes (refreshed with JANUS_TOKEN).

```text
janusctl nodes
```

Runs on this machine: no node is contacted.

## Node

### janusctl version

Janusctl's version, and the node's.

```text
janusctl version
```

### janusctl tui

A live dashboard in the terminal: the node, or every node of the context.

```text
janusctl tui [-interval DURATION] [-once] [-theme NAME]
```

| Flag | |
|---|---|
| `-interval DURATION` | how often the nodes are asked (default 2s, 1s to 30s) |
| `-once` | print one frame as text and exit |
| `-theme NAME` | the colour theme for this run (list: every theme; M in the dashboard saves one) |

### janusctl system

The node: state, logs, files, power.

#### janusctl system info

Version, kernel, slot, memory, CPU, load, disks.

```text
janusctl system info
```

#### janusctl system hostname

The node's hostname.

```text
janusctl system hostname
```

#### janusctl system services

Managed services and their health.

```text
janusctl system services
```

#### janusctl system service

Control a managed service (janusd: restart only).

```text
janusctl system service start|stop|restart ID
```

#### janusctl system logs

Janusd or haproxy output.

```text
janusctl system logs [-f] [-n LINES] [-boot N] SERVICE
```

| Flag | |
|---|---|
| `-f` | follow |
| `-n LINES` | only the last LINES lines |
| `-boot N` | a previous boot's: -1 the one before this one (system boots) |

#### janusctl system events

The node's event log, live.

```text
janusctl system events [-since ID]
```

| Flag | |
|---|---|
| `-since ID` | only events after this ID |

#### janusctl system audit

Who changed what, from where, how it ended - and every call refused, live.

```text
janusctl system audit [-since ID]
```

| Flag | |
|---|---|
| `-since ID` | only entries after this ID |

#### janusctl system dmesg

The kernel's messages.

```text
janusctl system dmesg [-f] [-boot N]
```

| Flag | |
|---|---|
| `-f` | follow |
| `-boot N` | a previous boot's: -1 the one before this one (system boots) |

#### janusctl system boots

The boots whose logs the LOGS partition keeps.

```text
janusctl system boots
```

#### janusctl system log-forwarding

Where the node's logs go: syslog, Elasticsearch (docs/guide/log-forwarding.md).

##### janusctl system log-forwarding status

Each target's state: sent, dropped, waiting, the last error.

```text
janusctl system log-forwarding status
```

##### janusctl system log-forwarding get

The configuration as JSON, without its secrets.

```text
janusctl system log-forwarding get
```

##### janusctl system log-forwarding set

Replace the configuration (JSON, as get prints it; a secret left out keeps its saved value).

```text
janusctl system log-forwarding set FILE
```

##### janusctl system log-forwarding test

Send one record to a target now.

```text
janusctl system log-forwarding test [-file FILE] TARGET
```

| Flag | |
|---|---|
| `-file FILE` | take the target from this configuration rather than the saved one |

#### janusctl system stats

CPU and memory of janusd and haproxy.

```text
janusctl system stats
```

#### janusctl system systemstat

Boot time, context switches, processes created, the CPUs' ticks (idle, steal).

```text
janusctl system systemstat
```

#### janusctl system ps

Every process.

```text
janusctl system ps
```

#### janusctl system netdev

Network interface counters.

```text
janusctl system netdev
```

#### janusctl system netstat

TCP and UDP sockets.

```text
janusctl system netstat
```

#### janusctl system clients

The connections to the API: a Controller and the users it acts for, janusctl....

```text
janusctl system clients
```

#### janusctl system mounts

Mounted filesystems.

```text
janusctl system mounts
```

#### janusctl system du

Disk usage.

```text
janusctl system du [-r] PATH...
```

| Flag | |
|---|---|
| `-r` | one line per directory |

#### janusctl system ls

List a directory.

```text
janusctl system ls [-r] PATH
```

| Flag | |
|---|---|
| `-r` | recursive |

#### janusctl system cat

Print a file.

```text
janusctl system cat PATH
```

#### janusctl system cp

A tar archive of PATH (stdout by default).

```text
janusctl system cp [-o FILE] PATH
```

| Flag | |
|---|---|
| `-o FILE` | the archive's file, - for stdout |

#### janusctl system support

A diagnostic bundle of the node, without its secrets (docs/guide/diagnostic-bundle.md).

```text
janusctl system support [-o FILE]
```

| Flag | |
|---|---|
| `-o FILE` | the bundle's file (janus-support-<hostname>-<time>.tar.gz), - for stdout |

#### janusctl system pcap

Live packet capture, as a pcap file (docs/packet-capture.md).

```text
janusctl system pcap [-i IFACE] [-f FILTER] [-promisc] [-include-own-stream] [-snaplen BYTES] [-duration DURATION] [-o FILE]
```

| Flag | |
|---|---|
| `-i IFACE` | the interface to capture on |
| `-f FILTER` | a tcpdump-style filter |
| `-promisc` | promiscuous mode |
| `-include-own-stream` | capture this capture's own connection too |
| `-snaplen BYTES` | bytes kept per packet |
| `-duration DURATION` | stop after this long |
| `-o FILE` | the pcap file, - for stdout |

#### janusctl system probe

Test the network from the node: dns, tcp, tls, http, ping, traceroute (docs/guide/connectivity-probe.md).

```text
janusctl system probe [-source IP] [-type TYPE] [-server HOST[:PORT]] [-connect-to HOST[:PORT]] [-sni NAME] [-alpn P,P] [-ca-file FILE] [-method METHOD] [-H 'NAME: VALUE'] [-count N] [-interval DURATION] [-size BYTES] [-df] [-max-hops N] [-timeout DURATION] [-v] KIND TARGET
```

| Flag | |
|---|---|
| `-source IP` | send from this address of the node (a VIP, an anycast address) |
| `-type TYPE` | dns: A, AAAA, CNAME, SRV, TXT, MX, NS or PTR |
| `-server HOST[:PORT]` | dns: ask this server |
| `-connect-to HOST[:PORT]` | tls, http: connect here, the target giving SNI and Host |
| `-sni NAME` | tls, http: the server name sent |
| `-alpn P,P` | tls, http: protocols offered |
| `-ca-file FILE` | tls, http: verify against this CA (PEM) |
| `-method METHOD` | http: GET, HEAD or OPTIONS |
| `-H 'NAME: VALUE'` | http: a header (repeatable) |
| `-count N` | ping: echo requests (4) |
| `-interval DURATION` | ping: between them (1s) |
| `-size BYTES` | ping, traceroute: payload (56) |
| `-df` | ping, traceroute: the DF bit - an MTU test |
| `-max-hops N` | traceroute: 30 by default and at most |
| `-timeout DURATION` | each step's |
| `-v` | the HTTP headers and body, every certificate |

#### janusctl system metrics

The node's Prometheus exporter: show or change.

```text
janusctl system metrics [-enable] [-disable] [-address IP|auto|*] [-port PORT]
```

| Flag | |
|---|---|
| `-enable` | turn it on |
| `-disable` | turn it off |
| `-address IP\|auto\|*` | serve on this address only (auto: the main interface's, *: all) |
| `-port PORT` | serve on this port |

#### janusctl system console-budget

The services' share of the node's console (docs/guide/log-forwarding.md): show or change.

```text
janusctl system console-budget [-rate BYTES] [-burst BYTES] [-default]
```

| Flag | |
|---|---|
| `-rate BYTES` | bytes a second of the services' output the console gets (0: none of it) |
| `-burst BYTES` | at most this much at once |
| `-default` | janusd's default again |

#### janusctl system node-exporter

Prometheus-node-exporter: show or change.

```text
janusctl system node-exporter [-enable] [-disable] [-address IP|auto|*] [-port PORT] [-collectors A,B]
```

| Flag | |
|---|---|
| `-enable` | run it |
| `-disable` | stop it, and keep it stopped |
| `-address IP\|auto\|*` | listen on this address only (auto: the main interface's, *: all) |
| `-port PORT` | listen on this port |
| `-collectors A,B` | the collectors to run |

#### janusctl system state

The node's state carried to its replacement (docs/guide/replacing-a-node.md).

##### janusctl system state export

The node's configuration, certificates and keys, network: a tar to import on its replacement.

```text
janusctl system state export [-identity] FILE|-
```

| Flag | |
|---|---|
| `-identity` | the node's CA and keys too: the replacement is this node to its clients |

##### janusctl system state import

Take another node's state, then restart into it - the node must be unused.

```text
janusctl system state import [-identity] [-network] [-save-network FILE] [-no-reboot] FILE|-
```

| Flag | |
|---|---|
| `-identity` | take the exported node's identity: its credentials work here, this node's own stop |
| `-network` | the exported network configuration too: the node takes the other's addresses |
| `-save-network FILE` | without -network: save the exported network configuration, for the takeover |
| `-no-reboot` | don't reboot: the import waits for janusd's next start |

#### janusctl system sysctl

Kernel parameters: HAProxy's on trial, the CIS benchmark's read-only (docs/guide/kernel-tuning.md).

##### janusctl system sysctl list

The parameters, their values, defaults and suggested values, the CIS benchmark.

```text
janusctl system sysctl list [-cis]
```

| Flag | |
|---|---|
| `-cis` | every CIS control too |

##### janusctl system sysctl get

One parameter: value, default, bounds, effect on HAProxy, risk, a suggested value and why.

```text
janusctl system sysctl get NAME
```

##### janusctl system sysctl set

Change parameters on trial, then confirm them.

```text
janusctl system sysctl set [-timeout DURATION] [-no-confirm] [-no-reload] NAME=VALUE...
```

| Flag | |
|---|---|
| `-timeout DURATION` | how long the node waits for the confirmation before reverting |
| `-no-confirm` | apply only - confirm yourself before the timeout |
| `-no-reload` | don't reload HAProxy for what it reads at its listeners |

##### janusctl system sysctl reset

Put parameters back to Janus's defaults on trial, then confirm.

```text
janusctl system sysctl reset [-timeout DURATION] [-no-confirm] [-no-reload] [-all] NAME...
```

| Flag | |
|---|---|
| `-timeout DURATION` | how long the node waits for the confirmation before reverting |
| `-no-confirm` | apply only - confirm yourself before the timeout |
| `-no-reload` | don't reload HAProxy for what it reads at its listeners |
| `-all` | every parameter |

##### janusctl system sysctl confirm

Save the values on trial: every boot applies them.

```text
janusctl system sysctl confirm
```

##### janusctl system sysctl cancel

Put the values from before the trial back now.

```text
janusctl system sysctl cancel
```

##### janusctl system sysctl history

Who changed what, when.

```text
janusctl system sysctl history [-n N]
```

| Flag | |
|---|---|
| `-n N` | the newest N changes |

##### janusctl system sysctl observed

What the node observed for its suggestions: each signal, by hour.

```text
janusctl system sysctl observed [-v]
```

| Flag | |
|---|---|
| `-v` | what each signal measures, and when an hour counts |

#### janusctl system reboot

Soft-stop HAProxy, then reboot.

```text
janusctl system reboot [-powercycle] [-kexec]
```

| Flag | |
|---|---|
| `-powercycle` | a power cycle |
| `-kexec` | jump into the active kernel without the firmware (seconds, not a POST) |

#### janusctl system shutdown

Soft-stop HAProxy, then power off.

```text
janusctl system shutdown
```

#### janusctl system restart

Restart janusd only - HAProxy keeps serving.

```text
janusctl system restart
```

#### janusctl system reset

Wipe the STATE partition and reboot: a new CA on the console.

```text
janusctl system reset [-wipe-state] [-wipe-ephemeral]
```

| Flag | |
|---|---|
| `-wipe-state` | wipe the persistent STATE partition |
| `-wipe-ephemeral` | wipe ephemeral state |

## HAProxy

### janusctl haproxy

HAProxy: config, runtime, certificates, files.

#### janusctl haproxy show-info

Version, uptime, connections.

```text
janusctl haproxy show-info
```

#### janusctl haproxy stats

Raw 'show stat' CSV.

```text
janusctl haproxy stats
```

#### janusctl haproxy backends

Backends, their servers and states.

```text
janusctl haproxy backends
```

#### janusctl haproxy get-config

The running haproxy.cfg, or a version of its history.

```text
janusctl haproxy get-config [-version ID]
```

| Flag | |
|---|---|
| `-version ID` | this version of config-history instead |

#### janusctl haproxy apply-config

Validate, apply and reload seamlessly.

```text
janusctl haproxy apply-config FILE
```

#### janusctl haproxy config-history

The configurations applied: who, when, lines changed.

```text
janusctl haproxy config-history [-limit N]
```

| Flag | |
|---|---|
| `-limit N` | the newest N only |

#### janusctl haproxy config-revert

Apply a version of config-history again: back to it.

```text
janusctl haproxy config-revert ID
```

#### janusctl haproxy map-list

The running config's file-backed maps.

```text
janusctl haproxy map-list
```

#### janusctl haproxy map-get

A map's entries.

```text
janusctl haproxy map-get MAP
```

#### janusctl haproxy map-set

Set one entry.

```text
janusctl haproxy map-set MAP KEY VALUE
```

#### janusctl haproxy map-delete

Delete one entry.

```text
janusctl haproxy map-delete MAP KEY
```

#### janusctl haproxy map-lookup

The entry a value matches, as a request's would (exit status 1: none).

```text
janusctl haproxy map-lookup MAP VALUE
```

#### janusctl haproxy acl-add

Add a pattern to an ACL.

```text
janusctl haproxy acl-add ACL VALUE
```

#### janusctl haproxy acl-delete

Delete a pattern from an ACL.

```text
janusctl haproxy acl-delete ACL VALUE
```

#### janusctl haproxy acl-lookup

The pattern a value matches (exit status 1: none).

```text
janusctl haproxy acl-lookup ACL VALUE
```

#### janusctl haproxy tables

The stick tables: key type, entries used, size.

```text
janusctl haproxy tables
```

#### janusctl haproxy table

A stick table's entries.

```text
janusctl haproxy table [-key KEY] [-filter DATA:OP:VALUE,...] [-limit N] TABLE
```

| Flag | |
|---|---|
| `-key KEY` | that entry only |
| `-filter DATA:OP:VALUE,...` | the entries whose data match, e.g. http_req_rate:gt:100 (eq ne lt le gt ge; 4 at most) |
| `-limit N` | entries shown: 500 by default, 5000 at most |

#### janusctl haproxy table-clear

Remove stick table entries - a blocked client let through.

```text
janusctl haproxy table-clear [-key KEY] [-filter DATA:OP:VALUE,...] [-all] TABLE
```

| Flag | |
|---|---|
| `-key KEY` | that entry |
| `-filter DATA:OP:VALUE,...` | the entries whose data match |
| `-all` | every entry |

#### janusctl haproxy sessions

The streams HAProxy is processing.

```text
janusctl haproxy sessions [-frontend NAME] [-backend NAME] [-source ADDRESS] [-limit N]
```

| Flag | |
|---|---|
| `-frontend NAME` | a frontend's only |
| `-backend NAME` | a backend's only |
| `-source ADDRESS` | a client's only, with or without its port |
| `-limit N` | streams shown: 500 by default, 5000 at most |

#### janusctl haproxy session-kill

End a stream, its ID from sessions.

```text
janusctl haproxy session-kill ID
```

#### janusctl haproxy errors

The last invalid request and response of each proxy (credentials masked).

```text
janusctl haproxy errors [-proxy NAME] [-direction request|response]
```

| Flag | |
|---|---|
| `-proxy NAME` | a frontend's or backend's only |
| `-direction request\|response` | one direction only |

#### janusctl haproxy cert-list

Certificates in HAProxy's store.

```text
janusctl haproxy cert-list
```

#### janusctl haproxy cert-upload

Upload a PEM certificate and key as NAME.

```text
janusctl haproxy cert-upload [-crt-list PATH] [-sni HOST,HOST] NAME FILE
```

| Flag | |
|---|---|
| `-crt-list PATH` | bind it into this crt-list |
| `-sni HOST,HOST` | with -crt-list: the SNI names |

#### janusctl haproxy cert-delete

Delete a certificate.

```text
janusctl haproxy cert-delete [-crt-list PATH] NAME
```

| Flag | |
|---|---|
| `-crt-list PATH` | unbind it from this crt-list first |

#### janusctl haproxy files

HAProxy's own files (/etc/haproxy/files).

```text
janusctl haproxy files
```

#### janusctl haproxy file-get

Print a file.

```text
janusctl haproxy file-get NAME
```

#### janusctl haproxy file-put

Write a file.

```text
janusctl haproxy file-put [-reload] NAME FILE
```

| Flag | |
|---|---|
| `-reload` | HAProxy uses it at once |

#### janusctl haproxy file-delete

Remove a file.

```text
janusctl haproxy file-delete [-reload] NAME
```

| Flag | |
|---|---|
| `-reload` | HAProxy uses it at once |

#### janusctl haproxy acme

Let's Encrypt (letsencrypt extension).

##### janusctl haproxy acme status

The account, each certificate's state and expiry.

```text
janusctl haproxy acme status
```

##### janusctl haproxy acme get

The configuration, as JSON.

```text
janusctl haproxy acme get
```

##### janusctl haproxy acme check

Check a configuration.

```text
janusctl haproxy acme check [-account-key FILE] FILE
```

| Flag | |
|---|---|
| `-account-key FILE` | an existing account's private key |

##### janusctl haproxy acme apply

Save a configuration.

```text
janusctl haproxy acme apply [-account-key FILE] FILE
```

| Flag | |
|---|---|
| `-account-key FILE` | an existing account's private key |

##### janusctl haproxy acme renew

Obtain certificates now.

```text
janusctl haproxy acme renew [NAME...]
```

##### janusctl haproxy acme path

How the node reaches the CA: show it, or set it.

```text
janusctl haproxy acme path [-user NAME] [-password-file FILE] [-ca FILE] [direct | proxy URL | controller URL | ordered THUMBPRINT]
```

| Flag | |
|---|---|
| `-user NAME` | the proxy's username |
| `-password-file FILE` | a file holding the proxy's password (kept when not given) |
| `-ca FILE` | the CA (PEM) of an https:// proxy's certificate; with controller, the Controller's identity |

##### janusctl haproxy acme test-path

Fetch the CA's directory the saved way, or the given one.

```text
janusctl haproxy acme test-path [-user NAME] [-password-file FILE] [-ca FILE] [direct | proxy URL | controller URL]
```

| Flag | |
|---|---|
| `-user NAME` | the proxy's username |
| `-password-file FILE` | a file holding the proxy's password (kept when not given) |
| `-ca FILE` | the CA (PEM) of an https:// proxy's certificate; with controller, the Controller's identity |

##### janusctl haproxy acme csr

A new key kept on the node, and its certificate signing request - to order the certificate elsewhere.

```text
janusctl haproxy acme csr NAME
```

##### janusctl haproxy acme install

Install the certificate issued for the request waiting.

```text
janusctl haproxy acme install NAME CHAIN-FILE
```

## Network

### janusctl network

Addresses, routes, firewall, VRRP, BGP, Consul.

#### janusctl network status

Interfaces, addresses, routes, DNS, clock.

```text
janusctl network status
```

#### janusctl network neighbors

The neighbour table: ARP and NDP entries, their MAC and state.

```text
janusctl network neighbors [-interface IF]
```

| Flag | |
|---|---|
| `-interface IF` | only this interface's |

#### janusctl network get

The network configuration, as JSON.

```text
janusctl network get
```

#### janusctl network apply

Apply a configuration on trial, then confirm it.

```text
janusctl network apply [-timeout DURATION] [-no-confirm] FILE
```

| Flag | |
|---|---|
| `-timeout DURATION` | how long the node waits for the confirmation before reverting |
| `-no-confirm` | apply only - confirm yourself before the timeout |

#### janusctl network confirm

Confirm the configuration on trial.

```text
janusctl network confirm
```

#### janusctl network modules

Optional modules and whether this image has them.

```text
janusctl network modules
```

#### janusctl network firewall

Nftables (nftables extension).

##### janusctl network firewall status

Saved, on trial, live.

```text
janusctl network firewall status
```

##### janusctl network firewall get

The saved ruleset.

```text
janusctl network firewall get
```

##### janusctl network firewall check

Validate a ruleset.

```text
janusctl network firewall check FILE
```

##### janusctl network firewall apply

Apply a ruleset on trial, then confirm it.

```text
janusctl network firewall apply [-timeout DURATION] [-no-confirm] FILE
```

| Flag | |
|---|---|
| `-timeout DURATION` | how long the node waits for the confirmation before reverting |
| `-no-confirm` | apply only - confirm yourself before the timeout |

##### janusctl network firewall confirm

Confirm the ruleset on trial.

```text
janusctl network firewall confirm
```

##### janusctl network firewall sets

The live named sets and their elements.

```text
janusctl network firewall sets
```

##### janusctl network firewall set-add

Add elements live.

```text
janusctl network firewall set-add [-timeout DURATION] FAMILY TABLE SET ELEMENT...
```

| Flag | |
|---|---|
| `-timeout DURATION` | the elements' timeout |

##### janusctl network firewall set-del

Delete elements.

```text
janusctl network firewall set-del [-timeout DURATION] FAMILY TABLE SET ELEMENT...
```

| Flag | |
|---|---|
| `-timeout DURATION` | unused |

#### janusctl network vrrp

VRRP (keepalived extension).

##### janusctl network vrrp status

Each instance's state and virtual IPs.

```text
janusctl network vrrp status
```

##### janusctl network vrrp get

The saved keepalived.conf.

```text
janusctl network vrrp get
```

##### janusctl network vrrp check

Have keepalived check it.

```text
janusctl network vrrp check FILE
```

##### janusctl network vrrp apply

Check, save and reload it.

```text
janusctl network vrrp apply FILE
```

#### janusctl network bgp

BGP (bird extension).

##### janusctl network bgp status

Protocols, sessions, routes.

```text
janusctl network bgp status
```

##### janusctl network bgp get

The saved bird.conf.

```text
janusctl network bgp get
```

##### janusctl network bgp check

Have BIRD check it.

```text
janusctl network bgp check FILE
```

##### janusctl network bgp apply

Check, save and reconfigure.

```text
janusctl network bgp apply FILE
```

#### janusctl network consul

Consul agent (consul extension).

##### janusctl network consul status

The service, the node, its cluster.

```text
janusctl network consul status
```

##### janusctl network consul get

The saved configuration.

```text
janusctl network consul get
```

##### janusctl network consul check

Have consul validate it.

```text
janusctl network consul check [-file NAME=PATH] [-only-files] FILE
```

| Flag | |
|---|---|
| `-file NAME=PATH` | a file it names (repeatable) |
| `-only-files` | unused here |

##### janusctl network consul apply

Check, save and apply (the agent restarts).

```text
janusctl network consul apply [-file NAME=PATH] [-only-files] FILE
```

| Flag | |
|---|---|
| `-file NAME=PATH` | a file it names (repeatable) |
| `-only-files` | drop the saved files not given |

## Trust

### janusctl access

The fleet the node trusts, its own CA.

#### janusctl access trust

The fleet the node trusts besides its own CA.

```text
janusctl access trust
```

#### janusctl access trust-set

Pin the fleet's root and apply a bundle.

```text
janusctl access trust-set [-root FILE] BUNDLE
```

| Flag | |
|---|---|
| `-root FILE` | the fleet's root (the first time) |

#### janusctl access trust-reset

Forget the fleet (the node's own CA only).

```text
janusctl access trust-reset
```

#### janusctl access rotate-ca

Replace the node's own CA.

```text
janusctl access rotate-ca [-console] DIR
```

| Flag | |
|---|---|
| `-console` | the node prints the admin key on its console |

## Cluster

### janusctl cluster

Clusters of a fleet without a Controller (create, apply, check...), and a node's membership of one (-n NODE: get, status, release, drain...).

#### janusctl cluster list

The clusters this fleet context orchestrates.

```text
janusctl cluster list
```

#### janusctl cluster create

Make a cluster of these nodes of the fleet context: checked, member certificates, the source's configuration as version 1 on each, its secrets pulled.

```text
janusctl cluster create [-source NODE] [-peers-port N] [-address NODE=IP] [-dir DIR] NAME NODE...
```

| Flag | |
|---|---|
| `-source NODE` | the node whose configuration becomes version 1, its certificates and keys every member's |
| `-peers-port N` | HAProxy's peers port (10057) |
| `-address NODE=IP` | the address the other members reach NODE on (default: the context's), repeatable |
| `-dir DIR` | write version 1's files there, to edit and apply |

#### janusctl cluster checkout

Write a version's files into DIR, to edit and apply.

```text
janusctl cluster checkout [-version N] NAME DIR
```

| Flag | |
|---|---|
| `-version N` | this version (default: the cluster's) |

#### janusctl cluster apply

DIR's haproxy.cfg, files/ and templates as the cluster's next version, applied member by member.

```text
janusctl cluster apply [-manage LIST] [-dry-run] [-m TEXT] NAME DIR
```

| Flag | |
|---|---|
| `-manage LIST` | what the cluster manages from now on: haproxy,files,vrrp,bgp,firewall (default: as before) |
| `-dry-run` | say what would change, change nothing |
| `-m TEXT` | what this version changes |

#### janusctl cluster check

Each member as it is: version, peers, drift, certificates - renewed, the ticket ring rotated when due; exits 1 when one wants a look (cron).

```text
janusctl cluster check NAME
```

#### janusctl cluster add

A node of the fleet context joins the cluster: checked, its certificate, the secrets, the cluster's version.

```text
janusctl cluster add [-address IP] [-var KEY=VALUE] NAME NODE
```

| Flag | |
|---|---|
| `-address IP` | the address the other members reach it on (default: the context's) |
| `-var KEY=VALUE` | a variable the cluster's templates use for it, repeatable |

#### janusctl cluster remove

A member leaves: drained first, the others told.

```text
janusctl cluster remove [-force] NAME NODE
```

| Flag | |
|---|---|
| `-force` | even if it doesn't drain or answer - a node gone for good |

#### janusctl cluster replace

A node of the fleet context takes a member's place, the traffic moving once: it joins drained, learns the stick tables, comes into service as OLD is drained, and OLD leaves.

```text
janusctl cluster replace [-address IP] [-var KEY=VALUE] [-new-image] NAME OLD NEW
```

| Flag | |
|---|---|
| `-address IP` | the address the other members reach NEW on (default: the context's) |
| `-var KEY=VALUE` | a variable the cluster's templates use for NEW, repeatable (default: OLD's) |
| `-new-image` | NEW may run another image - the cluster changing image member by member |

#### janusctl cluster delete

Every member leaves; the cluster and its CA are gone.

```text
janusctl cluster delete [-force] NAME
```

| Flag | |
|---|---|
| `-force` | even if a member doesn't answer |

#### janusctl cluster vars

A member's template variables, set (KEY= unsets one) or shown.

```text
janusctl cluster vars NAME MEMBER [KEY=VALUE...]
```

#### janusctl cluster certificate

A certificate and its key for every member: uploaded to the one the secrets come from, pulled by the others.

```text
janusctl cluster certificate [-name NAME] [-crt-list PATH] [-sni NAMES] NAME FILE
```

| Flag | |
|---|---|
| `-name NAME` | its name in HAProxy's store (default: the file's) |
| `-crt-list PATH` | a crt-list to bind it into |
| `-sni NAMES` | the names it's served for there, comma-separated |

#### janusctl cluster tickets

A new TLS ticket key on the member the secrets come from, pulled by the others (check does it daily).

```text
janusctl cluster tickets [-renew] NAME
```

| Flag | |
|---|---|
| `-renew` | replace every key of the ring at once - no key it held is used or accepted any more (a member's departure does it) |

#### janusctl cluster balancer

The hook telling a balancer in front which members take the traffic - told before a member is drained or removed, after one comes into service; shown without a flag.

```text
janusctl cluster balancer [-url URL] [-ca FILE] [-secret-file FILE] [-no-secret] [-drain-delay SECONDS] [-off] [-sync] NAME
```

| Flag | |
|---|---|
| `-url URL` | the hook: an http(s) URL the members are POSTed to |
| `-ca FILE` | the only CA its certificate may chain to (PEM) |
| `-secret-file FILE` | a file holding the secret the calls are signed with (X-Janus-Signature-256) |
| `-no-secret` | calls unsigned from now on |
| `-drain-delay SECONDS` | how long a member serves on once the balancer was told it goes (0) |
| `-off` | no hook any more |
| `-sync` | call it with the members now |

#### janusctl cluster journal

What was done to the cluster, by whom.

```text
janusctl cluster journal [-n N] NAME
```

| Flag | |
|---|---|
| `-n N` | the newest N entries (50) |

#### janusctl cluster get

What the node knows of its cluster, and the peers section HAProxy runs with.

```text
janusctl cluster get
```

#### janusctl cluster status

HAProxy's peers: connected, and in sync.

```text
janusctl cluster status
```

#### janusctl cluster csr

A signing request for the node's member certificate - its key stays on the node.

```text
janusctl cluster csr [OUT]
```

#### janusctl cluster set

Install or update the membership (MEMBER: name=address[,api=host:port]).

```text
janusctl cluster set [-id UUID] [-name NAME] [-member NAME] [-port N] [-ca FILE] [-cert FILE] [-managed-by TEXT] [-managed LIST] MEMBER...
```

| Flag | |
|---|---|
| `-id UUID` | the cluster's id |
| `-name NAME` | the cluster's name |
| `-member NAME` | this node's member name (default: its hostname) |
| `-port N` | the peers port (default 10057) |
| `-ca FILE` | the cluster's CA (PEM): the first time, or to replace it |
| `-cert FILE` | the member certificate (PEM) for the last csr: the first time, or to renew it |
| `-managed-by TEXT` | who sets the composition |
| `-managed LIST` | what the cluster manages here, locked to the orchestrator: haproxy,files,vrrp,bgp,firewall |

#### janusctl cluster leave

Forget the membership: the node on its own again.

```text
janusctl cluster leave
```

#### janusctl cluster release

Let this node's own users change what the cluster manages (all of it when none is named) - the cluster then sees it drifted.

```text
janusctl cluster release [DOMAIN...]
```

#### janusctl cluster drain

Out of service without stopping: the virtual IPs and routes move to the peers.

```text
janusctl cluster drain [-timeout SECONDS]
```

| Flag | |
|---|---|
| `-timeout SECONDS` | how long to wait for VRRP and BGP to show it (60) |

#### janusctl cluster undrain

Back in service.

```text
janusctl cluster undrain
```

#### janusctl cluster pull

Fetch another member's secrets - certificates, files, ACME certificates, ticket keys - over mTLS.

```text
janusctl cluster pull [-from MEMBER] [-items LIST]
```

| Flag | |
|---|---|
| `-from MEMBER` | the member to pull from |
| `-items LIST` | runtime-certs, files, acme-certs, tls-ticket-keys (default: all) |

#### janusctl cluster rotate-tickets

Add a TLS ticket key to the ring, handed to HAProxy without a reload.

```text
janusctl cluster rotate-tickets [-renew]
```

| Flag | |
|---|---|
| `-renew` | replace every key of the ring at once - no key it held is used or accepted any more |

### janusctl pki

Client certificates of the node's own CA.

#### janusctl pki generate-client-config

Issue a client certificate into DIR.

```text
janusctl pki generate-client-config [-role ROLE] [-name NAME] [-ttl DURATION] DIR
```

| Flag | |
|---|---|
| `-role ROLE` | os:admin, os:operator or os:reader |
| `-name NAME` | its common name |
| `-ttl DURATION` | how long it's valid (a year at most) |

### janusctl fleet

A fleet without a Controller (docs/fleet-without-controller.md).

The kit's passphrase is asked, or read from JANUS_KIT_PASSPHRASE.

#### janusctl fleet init

A new fleet: its root's key in KIT.

```text
janusctl fleet init [-name FLEET] [-issuer NAME] [-user NAME] [-role ROLE] [-yes] KIT
```

| Flag | |
|---|---|
| `-name FLEET` | the fleet's name |
| `-issuer NAME` | this machine's issuing CA's name |
| `-user NAME` | who this machine's certificates are for |
| `-role ROLE` | their role |
| `-yes` | don't ask the passphrase back |

#### janusctl fleet recover

A fleet from its kit.

```text
janusctl fleet recover [-kit KIT] [-name FLEET] [-issuer NAME] [-user NAME] [-role ROLE]
```

| Flag | |
|---|---|
| `-kit KIT` | the fleet's recovery kit |
| `-name FLEET` | the fleet's name |
| `-issuer NAME` | this machine's issuing CA's name |
| `-user NAME` | who this machine's certificates are for |
| `-role ROLE` | their role |

#### janusctl fleet adopt

A node into the fleet.

```text
janusctl fleet adopt [-endpoint HOST:PORT] [-ca FILE] [-cert FILE] [-key FILE] [-ca-fingerprint SHA256] [-kit KIT] NAME
```

| Flag | |
|---|---|
| `-endpoint HOST:PORT` | the node's API |
| `-ca FILE` | its first boot's CA (ca.crt) |
| `-cert FILE` | admin.crt |
| `-key FILE` | admin.key |
| `-ca-fingerprint SHA256` | a node already in the fleet: its CA's SHA-256 |
| `-kit KIT` | the fleet's recovery kit |

#### janusctl fleet sync

The newest bundle everywhere.

```text
janusctl fleet sync [-kit KIT]
```

| Flag | |
|---|---|
| `-kit KIT` | the fleet's recovery kit |

#### janusctl fleet status

Each node's bundle, this machine's certificate.

```text
janusctl fleet status
```

#### janusctl fleet export

Root.crt, bundle.json, user-data.json.

```text
janusctl fleet export DIR
```

#### janusctl fleet forget

A node out of this context.

```text
janusctl fleet forget NAME
```

#### janusctl fleet issuer

The fleet's issuing CAs.

##### janusctl fleet issuer list

The issuing CAs of the bundle.

```text
janusctl fleet issuer list
```

##### janusctl fleet issuer request

A new machine: REQUEST to sign where the kit is.

```text
janusctl fleet issuer request [-issuer NAME] [-user NAME] [-role ROLE] REQUEST
```

| Flag | |
|---|---|
| `-issuer NAME` | this machine's issuing CA's name |
| `-user NAME` | who its certificates are for |
| `-role ROLE` | the most they carry |

##### janusctl fleet issuer sign

Sign a machine's issuing CA into the bundle.

```text
janusctl fleet issuer sign [-kit KIT] [-replace] [-role ROLE] REQUEST GRANT
```

| Flag | |
|---|---|
| `-kit KIT` | the fleet's recovery kit |
| `-replace` | replace an issuing CA of that name |
| `-role ROLE` | the most its certificates carry |

##### janusctl fleet issuer accept

The machine's issuing CA, the fleet and its nodes.

```text
janusctl fleet issuer accept GRANT
```

##### janusctl fleet issuer revoke

A bundle without NAME's issuing CA.

```text
janusctl fleet issuer revoke [-kit KIT] NAME
```

| Flag | |
|---|---|
| `-kit KIT` | the fleet's recovery kit |

## Lifecycle

### janusctl lifecycle

Install, upgrade, roll back.

#### janusctl lifecycle install

Partition a blank disk and write a release (paths on the node).

```text
janusctl lifecycle install [-sha256 HEX] [-controller-address HOST:PORT] [-controller-ca FILE] [-controller-fleet-root FILE] [-network-config FILE] [-fleet-root FILE] [-fleet-bundle FILE] [-registration-token TOKEN] [-logs-size-mb MIB] [-insecure-skip-signature-check] DISK BUNDLE_DIR
```

| Flag | |
|---|---|
| `-sha256 HEX` | rootfs.squashfs's expected SHA-256 |
| `-controller-address HOST:PORT` | a Controller to self-register with |
| `-controller-ca FILE` | the Controller's CA |
| `-controller-fleet-root FILE` | the Controller's fleet root |
| `-network-config FILE` | a network configuration (JSON) |
| `-fleet-root FILE` | a fleet to trust from the first boot |
| `-fleet-bundle FILE` | its bundle |
| `-registration-token TOKEN` | a Controller enrollment token |
| `-logs-size-mb MIB` | the LOGS partition's size (-1: none; default 5 % of the disk) |
| `-insecure-skip-signature-check` | accept unsigned UKIs - development only |

#### janusctl lifecycle upgrade

Write a release to the inactive slot and reboot into it.

```text
janusctl lifecycle upgrade [-sha256 HEX] [-wait-for-health] [-health-timeout SECONDS] [-insecure-skip-signature-check] [-allow-schematic-change] [-kexec] BUNDLE_DIR|URL
```

| Flag | |
|---|---|
| `-sha256 HEX` | rootfs.squashfs's expected SHA-256 |
| `-wait-for-health` | revert by itself if the new slot isn't healthy |
| `-health-timeout SECONDS` | how long it has to be healthy |
| `-insecure-skip-signature-check` | accept unsigned UKIs - development only |
| `-allow-schematic-change` | accept another image schematic |
| `-kexec` | reboot into the new slot without the firmware (seconds, not a POST) |

#### janusctl lifecycle rollback

Boot the other slot.

```text
janusctl lifecycle rollback
```

#### janusctl lifecycle upload-release

Stream a local release bundle to the node.

```text
janusctl lifecycle upload-release BUNDLE_DIR
```

### janusctl image

Write onto a disk image, offline.

#### janusctl image seed-controller

A Controller to self-register with.

```text
janusctl image seed-controller [-controller-address HOST:PORT] [-controller-ca FILE] [-controller-fleet-root FILE] [-registration-token TOKEN] DISK
```

| Flag | |
|---|---|
| `-controller-address HOST:PORT` | the Controller's registration address |
| `-controller-ca FILE` | its CA |
| `-controller-fleet-root FILE` | its fleet root |
| `-registration-token TOKEN` | an enrollment token |

#### janusctl image seed-fleet

A fleet to trust from the first boot.

```text
janusctl image seed-fleet [-fleet-root FILE] [-fleet-bundle FILE] DISK
```

| Flag | |
|---|---|
| `-fleet-root FILE` | the fleet's root |
| `-fleet-bundle FILE` | its bundle |

#### janusctl image seed-network

A network configuration from the first boot.

```text
janusctl image seed-network [-config FILE] DISK
```

| Flag | |
|---|---|
| `-config FILE` | the configuration (JSON) |

## Shell

### janusctl completion

The shell's completion script (README: Shell completion).

```text
janusctl completion bash|zsh|fish
```

Runs on this machine: no node is contacted.

### janusctl help

This help, or a command's.

```text
janusctl help [COMMAND...]
```

Runs on this machine: no node is contacted.
