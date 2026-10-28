<!-- Generated from cmd/janusctl/commands.go and effects.go: go test ./cmd/janusctl -run TestSkillCommands -update -->

# janusctl commands

Every janusctl command, with what it does to the node: **reads** - nothing
changes; **changes** - the node, or files on this machine; **critical** -
reboots, interrupts or moves traffic, wipes, or touches the node's trust or
credentials. SKILL.md says when each may run. What a flag means:
`janusctl help COMMAND` - the installed janusctl's own words, which win
over this list when they differ.

Global flags come before the command:

```text
janusctl [-context NAME] [-n NODE[,NODE] | -all] COMMAND ...
janusctl -endpoint HOST:PORT -ca FILE -cert FILE -key FILE COMMAND ...
```

## Sign in

- `janusctl login [-context NAME] [-controller HOST[:PORT]] [-controller-ca FILE] [-controller-fingerprint SHA256] [-user NAME] [-ssh-key FILE] [-browser] [-no-open] [-device]` - **changes** - sign in to a Controller: a certificate of its fleet, and its nodes. Interactive (browser, device code, SSH key passphrase): the user signs in, not the assistant
- `janusctl context [list | use NAME | delete NAME]` - **changes** - the Controllers and fleets signed in to. List only reads
- `janusctl nodes` - reads - the context's nodes (refreshed with JANUS_TOKEN)

## Node

- `janusctl version` - reads - janusctl's version, and the node's
- `janusctl tui [-interval DURATION] [-once] [-theme NAME]` - reads - a live dashboard in the terminal: the node, or every node of the context. Interactive: only -once prints a frame and exits
- `janusctl system info` - reads - version, kernel, slot, memory, CPU, load, disks
- `janusctl system hostname` - reads - the node's hostname
- `janusctl system services` - reads - managed services and their health
- `janusctl system service start|stop|restart ID` - **critical** - control a managed service (janusd: restart only). Stopping or restarting a service interrupts what it does
- `janusctl system logs [-f] [-n LINES] [-boot N] SERVICE` - reads - janusd or haproxy output. -f follows forever: use -n LINES
- `janusctl system events [-since ID]` - reads - the node's event log, live. Streams forever: run it under timeout
- `janusctl system audit [-since ID]` - reads - who changed what, from where, how it ended - and every call refused, live. Streams forever: run it under timeout
- `janusctl system dmesg [-f] [-boot N]` - reads - the kernel's messages. -f follows forever
- `janusctl system boots` - reads - the boots whose logs the LOGS partition keeps
- `janusctl system log-forwarding status` - reads - each target's state: sent, dropped, waiting, the last error
- `janusctl system log-forwarding get` - reads - the configuration as JSON, without its secrets
- `janusctl system log-forwarding set FILE` - **changes** - replace the configuration (JSON, as get prints it; a secret left out keeps its saved value)
- `janusctl system log-forwarding test [-file FILE] TARGET` - **changes** - send one record to a target now. Sends a record to the target
- `janusctl system stats` - reads - CPU and memory of janusd and haproxy
- `janusctl system systemstat` - reads - boot time, context switches, processes created, the CPUs' ticks (idle, steal)
- `janusctl system ps` - reads - every process
- `janusctl system netdev` - reads - network interface counters
- `janusctl system netstat` - reads - TCP and UDP sockets
- `janusctl system clients` - reads - the connections to the API: a Controller and the users it acts for, janusctl...
- `janusctl system mounts` - reads - mounted filesystems
- `janusctl system du [-r] PATH...` - reads - disk usage
- `janusctl system ls [-r] PATH` - reads - list a directory
- `janusctl system cat PATH` - reads - print a file
- `janusctl system cp [-o FILE] PATH` - reads - a tar archive of PATH (stdout by default). A tar: give -o FILE
- `janusctl system support [-o FILE]` - reads - a diagnostic bundle of the node, without its secrets (docs/guide/diagnostic-bundle.md)
- `janusctl system pcap [-i IFACE] [-f FILTER] [-promisc] [-include-own-stream] [-snaplen BYTES] [-duration DURATION] [-o FILE]` - reads - live packet capture, as a pcap file (docs/packet-capture.md). Packets may hold clients' data: -duration and -o FILE, never stdout
- `janusctl system probe [-source IP] [-type TYPE] [-server HOST[:PORT]] [-connect-to HOST[:PORT]] [-sni NAME] [-alpn P,P] [-ca-file FILE] [-method METHOD] [-H 'NAME: VALUE'] [-count N] [-interval DURATION] [-size BYTES] [-df] [-max-hops N] [-timeout DURATION] [-v] KIND TARGET` - reads - test the network from the node: dns, tcp, tls, http, ping, traceroute (docs/guide/connectivity-probe.md). Sends traffic from the node
- `janusctl system metrics [-enable] [-disable] [-address IP|auto|*] [-port PORT]` - **changes** - the node's Prometheus exporter: show or change. Shows the settings without flags
- `janusctl system console-budget [-rate BYTES] [-burst BYTES] [-default]` - **changes** - the services' share of the node's console (docs/guide/log-forwarding.md): show or change. Shows the budget without flags
- `janusctl system node-exporter [-enable] [-disable] [-address IP|auto|*] [-port PORT] [-collectors A,B]` - **changes** - prometheus-node-exporter: show or change. Shows the settings without flags
- `janusctl system state export [-identity] FILE|-` - **critical** - the node's configuration, certificates and keys, network: a tar to import on its replacement. The file holds the node's private keys
- `janusctl system state import [-identity] [-network] [-save-network FILE] [-no-reboot] [-from NODE|HOST:PORT] [-from-address HOST:PORT] [-from-ca FILE] [-from-cert FILE] [-from-key FILE] FILE|- | -from NODE` - **critical** - take another node's state - from a file, a pipe, or pulled from the node itself -, then restart into it; the node must be unused. The node takes another's state and restarts into it
- `janusctl system sysctl list [-cis]` - reads - the parameters, their values, defaults and suggested values, the CIS benchmark
- `janusctl system sysctl get NAME` - reads - one parameter: value, default, bounds, effect on HAProxy, risk, a suggested value and why
- `janusctl system sysctl set [-timeout DURATION] [-no-confirm] [-no-reload] NAME=VALUE...` - **changes** - change parameters on trial, then confirm them. On trial: reverts by itself unless confirmed
- `janusctl system sysctl reset [-timeout DURATION] [-no-confirm] [-no-reload] [-all] NAME...` - **changes** - put parameters back to Janus's defaults on trial, then confirm. On trial: reverts by itself unless confirmed
- `janusctl system sysctl confirm` - **changes** - save the values on trial: every boot applies them
- `janusctl system sysctl cancel` - **changes** - put the values from before the trial back now
- `janusctl system sysctl history [-n N]` - reads - who changed what, when
- `janusctl system sysctl observed [-v]` - reads - what the node observed for its suggestions: each signal, by hour
- `janusctl system reboot [-powercycle] [-kexec]` - **critical** - soft-stop HAProxy, then reboot
- `janusctl system shutdown` - **critical** - soft-stop HAProxy, then power off. Nothing powers it back on remotely
- `janusctl system restart` - **changes** - restart janusd only - HAProxy keeps serving. Janusd only: HAProxy keeps serving
- `janusctl system reset [-wipe-state] [-wipe-ephemeral]` - **critical** - wipe the STATE partition and reboot: a new CA on the console. Wipes the node's state: a new CA, configuration gone

## HAProxy

- `janusctl haproxy show-info` - reads - version, uptime, connections
- `janusctl haproxy stats` - reads - raw 'show stat' CSV
- `janusctl haproxy backends` - reads - backends, their servers and states
- `janusctl haproxy server-state BACKEND SERVER ready|drain|maint` - **changes** - set a server ready, drain or maint - until HAProxy's next reload. Drain and maint take the server out of the traffic; HAProxy's next reload (apply-config, file-put -reload...) takes haproxy.cfg's state again
- `janusctl haproxy reload` - **changes** - reload HAProxy seamlessly: haproxy.cfg and its files read again, the runtime changes (server-state, map-set, acl-add) gone. Seamless, but what server-state, map-set and acl-add changed is gone: a drained server takes traffic again - check haproxy backends first
- `janusctl haproxy get-config [-version ID]` - reads - the running haproxy.cfg, or a version of its history
- `janusctl haproxy apply-config FILE` - **changes** - validate, apply and reload seamlessly. Validated on the node first, reloaded without dropping connections
- `janusctl haproxy config-history [-limit N]` - reads - the configurations applied: who, when, lines changed
- `janusctl haproxy config-revert ID` - **changes** - apply a version of config-history again: back to it
- `janusctl haproxy map-list` - reads - the running config's file-backed maps
- `janusctl haproxy map-get MAP` - reads - a map's entries
- `janusctl haproxy map-set MAP KEY VALUE` - **changes** - set one entry. In memory only: the next reload reads the map's file again
- `janusctl haproxy map-delete MAP KEY` - **changes** - delete one entry. In memory only: the next reload reads the map's file again
- `janusctl haproxy map-lookup MAP VALUE` - reads - the entry a value matches, as a request's would (exit status 1: none)
- `janusctl haproxy acl-add ACL VALUE` - **changes** - add a pattern to an ACL. In memory only: the next reload reads the list's file again
- `janusctl haproxy acl-delete ACL VALUE` - **changes** - delete a pattern from an ACL. In memory only: the next reload reads the list's file again
- `janusctl haproxy acl-lookup ACL VALUE` - reads - the pattern a value matches (exit status 1: none)
- `janusctl haproxy tables` - reads - the stick tables: key type, entries used, size
- `janusctl haproxy table [-key KEY] [-filter DATA:OP:VALUE,...] [-limit N] TABLE` - reads - a stick table's entries
- `janusctl haproxy table-clear [-key KEY] [-filter DATA:OP:VALUE,...] [-all] TABLE` - **changes** - remove stick table entries - a blocked client let through
- `janusctl haproxy sessions [-frontend NAME] [-backend NAME] [-source ADDRESS] [-limit N]` - reads - the streams HAProxy is processing
- `janusctl haproxy session-kill ID` - **changes** - end a stream, its ID from sessions
- `janusctl haproxy errors [-proxy NAME] [-direction request|response]` - reads - the last invalid request and response of each proxy (credentials masked)
- `janusctl haproxy cert-list` - reads - certificates in HAProxy's store
- `janusctl haproxy cert-upload [-crt-list PATH] [-sni HOST,HOST] NAME FILE` - **changes** - upload a PEM certificate and key as NAME. Pass the PEM by path, never print it
- `janusctl haproxy cert-delete [-crt-list PATH] NAME` - **changes** - delete a certificate
- `janusctl haproxy files` - reads - HAProxy's own files (/etc/haproxy/files)
- `janusctl haproxy file-get NAME` - reads - print a file
- `janusctl haproxy file-put [-reload] NAME FILE` - **changes** - write a file
- `janusctl haproxy file-delete [-reload] NAME` - **changes** - remove a file
- `janusctl haproxy acme status` - reads - the account, each certificate's state and expiry
- `janusctl haproxy acme get` - reads - the configuration, as JSON
- `janusctl haproxy acme check [-account-key FILE] FILE` - reads - check a configuration
- `janusctl haproxy acme apply [-account-key FILE] FILE` - **changes** - save a configuration
- `janusctl haproxy acme renew [NAME...]` - **changes** - obtain certificates now. Orders certificates: the CA's rate limits count
- `janusctl haproxy acme path [-user NAME] [-password-file FILE] [-ca FILE] [direct | proxy URL | controller URL | ordered THUMBPRINT]` - **changes** - how the node reaches the CA: show it, or set it. Shows the path without arguments
- `janusctl haproxy acme test-path [-user NAME] [-password-file FILE] [-ca FILE] [direct | proxy URL | controller URL]` - reads - fetch the CA's directory the saved way, or the given one
- `janusctl haproxy acme csr NAME` - **changes** - a new key kept on the node, and its certificate signing request - to order the certificate elsewhere
- `janusctl haproxy acme install NAME CHAIN-FILE` - **changes** - install the certificate issued for the request waiting

## Network

- `janusctl network status` - reads - interfaces, addresses, routes, DNS, clock
- `janusctl network neighbors [-interface IF]` - reads - the neighbour table: ARP and NDP entries, their MAC and state
- `janusctl network get` - reads - the network configuration, as JSON
- `janusctl network apply [-timeout DURATION] [-no-confirm] FILE` - **changes** - apply a configuration on trial, then confirm it. On trial: janusctl confirms over a new connection, else the node reverts by itself
- `janusctl network confirm` - **changes** - confirm the configuration on trial
- `janusctl network modules` - reads - optional modules and whether this image has them
- `janusctl network firewall status` - reads - saved, on trial, live
- `janusctl network firewall get` - reads - the saved ruleset
- `janusctl network firewall check FILE` - reads - validate a ruleset
- `janusctl network firewall apply [-timeout DURATION] [-no-confirm] FILE` - **changes** - apply a ruleset on trial, then confirm it. On trial: janusctl confirms over a new connection, else the node reverts by itself
- `janusctl network firewall confirm` - **changes** - confirm the ruleset on trial
- `janusctl network firewall sets` - reads - the live named sets and their elements
- `janusctl network firewall set-add [-timeout DURATION] FAMILY TABLE SET ELEMENT...` - **changes** - add elements live
- `janusctl network firewall set-del [-timeout DURATION] FAMILY TABLE SET ELEMENT...` - **changes** - delete elements
- `janusctl network vrrp status` - reads - each instance's state and virtual IPs
- `janusctl network vrrp get` - reads - the saved keepalived.conf
- `janusctl network vrrp check FILE` - reads - have keepalived check it
- `janusctl network vrrp apply FILE` - **changes** - check, save and reload it. No trial: a priority change can move the virtual IPs at once
- `janusctl network bgp status` - reads - protocols, sessions, routes
- `janusctl network bgp get` - reads - the saved bird.conf
- `janusctl network bgp check FILE` - reads - have BIRD check it
- `janusctl network bgp apply FILE` - **changes** - check, save and reconfigure. No trial: routes change at once
- `janusctl network bgp exporter [-enable] [-disable] [-address IP|auto|*] [-port PORT]` - **changes** - prometheus-bird-exporter: show or change. Shows the settings without flags
- `janusctl network consul status` - reads - the service, the node, its cluster
- `janusctl network consul get` - reads - the saved configuration
- `janusctl network consul check [-file NAME=PATH] [-only-files] FILE` - reads - have consul validate it
- `janusctl network consul apply [-file NAME=PATH] [-only-files] FILE` - **changes** - check, save and apply (the agent restarts). The agent restarts

## Trust

- `janusctl access trust` - reads - the fleet the node trusts besides its own CA
- `janusctl access trust-set [-root FILE] BUNDLE` - **critical** - pin the fleet's root and apply a bundle. Pins a fleet's root on the node
- `janusctl access trust-reset` - **critical** - forget the fleet (the node's own CA only). The fleet - a Controller too - loses the node
- `janusctl access rotate-ca [-console] DIR` - **critical** - replace the node's own CA

## Cluster

- `janusctl cluster list` - reads - the clusters this fleet context orchestrates
- `janusctl cluster create [-source NODE] [-peers-port N] [-address NODE=IP] [-dir DIR] NAME NODE...` - **critical** - make a cluster of these nodes of the fleet context: checked, member certificates, the source's configuration as version 1 on each, its secrets pulled
- `janusctl cluster checkout [-version N] NAME DIR` - **changes** - write a version's files into DIR, to edit and apply. Writes files on this machine
- `janusctl cluster apply [-manage LIST] [-dry-run] [-m TEXT] NAME DIR` - **changes** - DIR's haproxy.cfg, files/ and templates as the cluster's next version, applied member by member. Member by member, waiting for the stick tables
- `janusctl cluster check NAME` - **changes** - each member as it is: version, peers, drift, certificates - renewed, the ticket ring rotated when due; exits 1 when one wants a look (cron). Renews certificates and rotates the ticket ring when due
- `janusctl cluster add [-address IP] [-var KEY=VALUE] NAME NODE` - **changes** - a node of the fleet context joins the cluster: checked, its certificate, the secrets, the cluster's version
- `janusctl cluster remove [-force] NAME NODE` - **critical** - a member leaves: drained first, the others told. The member is drained, then leaves
- `janusctl cluster replace [-address IP] [-var KEY=VALUE] [-new-image] NAME OLD NEW` - **critical** - a node of the fleet context takes a member's place, the traffic moving once: it joins drained, learns the stick tables, comes into service as OLD is drained, and OLD leaves. The traffic moves from OLD to NEW
- `janusctl cluster delete [-force] NAME` - **critical** - every member leaves; the cluster and its CA are gone
- `janusctl cluster vars NAME MEMBER [KEY=VALUE...]` - **changes** - a member's template variables, set (KEY= unsets one) or shown. Shows them without KEY=VALUE
- `janusctl cluster certificate [-name NAME] [-crt-list PATH] [-sni NAMES] NAME FILE` - **changes** - a certificate and its key for every member: uploaded to the one the secrets come from, pulled by the others. Pass the PEM by path, never print it
- `janusctl cluster tickets [-renew] NAME` - **changes** - a new TLS ticket key on the member the secrets come from, pulled by the others (check does it daily)
- `janusctl cluster balancer [-kind KIND] [-url URL] [-ca FILE] [-fingerprint SHA256] [-secret-file FILE] [-router-user USER] [-password-file FILE] [-prefix PREFIX] [-distance N] [-routing-table TABLE] [-allow-http] [-check-gateway ping|none] [-read-only yes|no] [-no-secret] [-drain-delay SECONDS] [-off] [-sync] NAME` - **changes** - the hook telling a balancer in front which members take the traffic - told before a member is drained or removed, after one comes into service -, or a RouterOS router whose ECMP routes the cluster keeps itself; shown without a flag. Shows the hook without flags
- `janusctl cluster journal [-n N] NAME` - reads - what was done to the cluster, by whom
- `janusctl cluster get` - reads - what the node knows of its cluster, and the peers section HAProxy runs with
- `janusctl cluster status` - reads - HAProxy's peers: connected, and in sync
- `janusctl cluster csr [OUT]` - **changes** - a signing request for the node's member certificate - its key stays on the node
- `janusctl cluster set [-id UUID] [-name NAME] [-member NAME] [-port N] [-ca FILE] [-cert FILE] [-managed-by TEXT] [-managed LIST] MEMBER...` - **critical** - install or update the membership (MEMBER: name=address[,api=host:port]). HAProxy restarts with the peers
- `janusctl cluster leave` - **critical** - forget the membership: the node on its own again
- `janusctl cluster release [DOMAIN...]` - **changes** - let this node's own users change what the cluster manages (all of it when none is named) - the cluster then sees it drifted. The cluster then reports the node as drifted
- `janusctl cluster drain [-timeout SECONDS]` - **critical** - out of service without stopping: the virtual IPs and routes move to the peers. The virtual IPs and routes move to the peers
- `janusctl cluster undrain` - **critical** - back in service. The node takes traffic again
- `janusctl cluster pull [-from MEMBER] [-items LIST]` - **changes** - fetch another member's secrets - certificates, files, ACME certificates, ticket keys - over mTLS
- `janusctl cluster rotate-tickets [-renew]` - **changes** - add a TLS ticket key to the ring, handed to HAProxy without a reload
- `janusctl pki generate-client-config [-role ROLE] [-name NAME] [-ttl DURATION] DIR` - **critical** - issue a client certificate into DIR. Issues a credential
- `janusctl fleet init [-name FLEET] [-issuer NAME] [-user NAME] [-role ROLE] [-yes] KIT` - **critical** - a new fleet: its root's key in KIT. A new fleet: its recovery kit goes offline, kept by the user
- `janusctl fleet recover [-kit KIT] [-name FLEET] [-issuer NAME] [-user NAME] [-role ROLE]` - **critical** - a fleet from its kit
- `janusctl fleet adopt [-endpoint HOST:PORT] [-ca FILE] [-cert FILE] [-key FILE] [-ca-fingerprint SHA256] [-kit KIT] NAME` - **critical** - a node into the fleet. Pins the fleet's root on the node
- `janusctl fleet sync [-kit KIT]` - **changes** - the newest bundle everywhere
- `janusctl fleet status` - reads - each node's bundle, this machine's certificate
- `janusctl fleet export DIR` - **changes** - root.crt, bundle.json, user-data.json. Writes files on this machine
- `janusctl fleet forget NAME` - **changes** - a node out of this context
- `janusctl fleet issuer list` - reads - the issuing CAs of the bundle
- `janusctl fleet issuer request [-issuer NAME] [-user NAME] [-role ROLE] REQUEST` - **changes** - a new machine: REQUEST to sign where the kit is. Writes a request and a key on this machine
- `janusctl fleet issuer sign [-kit KIT] [-replace] [-role ROLE] REQUEST GRANT` - **critical** - sign a machine's issuing CA into the bundle. A new issuing CA the fleet trusts
- `janusctl fleet issuer accept GRANT` - **critical** - the machine's issuing CA, the fleet and its nodes
- `janusctl fleet issuer revoke [-kit KIT] NAME` - **critical** - a bundle without NAME's issuing CA

## Lifecycle

- `janusctl lifecycle install [-sha256 HEX] [-controller-address HOST:PORT] [-controller-ca FILE] [-controller-fleet-root FILE] [-network-config FILE] [-fleet-root FILE] [-fleet-bundle FILE] [-registration-token TOKEN] [-logs-size-mb MIB] [-insecure-skip-signature-check] DISK BUNDLE_DIR` - **critical** - partition a blank disk and write a release (paths on the node). Partitions and writes a disk
- `janusctl lifecycle upgrade [-sha256 HEX] [-wait-for-health] [-health-timeout SECONDS] [-insecure-skip-signature-check] [-allow-schematic-change] [-kexec] BUNDLE_DIR|URL` - **critical** - write a release to the inactive slot and reboot into it. Reboots into the new release
- `janusctl lifecycle rollback` - **critical** - boot the other slot. Reboots into the other slot
- `janusctl lifecycle upload-release BUNDLE_DIR` - **changes** - stream a local release bundle to the node. Stages a bundle: nothing runs it until upgrade
- `janusctl image seed-controller [-controller-address HOST:PORT] [-controller-ca FILE] [-controller-fleet-root FILE] [-registration-token TOKEN] DISK` - **changes** - a Controller to self-register with. Writes onto a disk image on this machine
- `janusctl image seed-fleet [-fleet-root FILE] [-fleet-bundle FILE] DISK` - **changes** - a fleet to trust from the first boot. Writes onto a disk image on this machine
- `janusctl image seed-network [-config FILE] DISK` - **changes** - a network configuration from the first boot. Writes onto a disk image on this machine

## Shell

- `janusctl completion bash|zsh|fish` - reads - the shell's completion script (README: Shell completion)
- `janusctl help [COMMAND...]` - reads - this help, or a command's
