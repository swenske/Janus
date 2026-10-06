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
janusctl system logs [-f] [-n LINES] SERVICE
```

| Flag | |
|---|---|
| `-f` | follow |
| `-n LINES` | only the last LINES lines |

#### janusctl system events

The node's event log, live.

```text
janusctl system events [-since ID]
```

| Flag | |
|---|---|
| `-since ID` | only events after this ID |

#### janusctl system dmesg

The kernel's messages.

```text
janusctl system dmesg [-f]
```

| Flag | |
|---|---|
| `-f` | follow |

#### janusctl system stats

CPU and memory of janusd and haproxy.

```text
janusctl system stats
```

#### janusctl system systemstat

Boot time, context switches, processes created.

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

#### janusctl system metrics

The node's Prometheus exporter: show or change.

```text
janusctl system metrics [-enable] [-disable] [-port PORT]
```

| Flag | |
|---|---|
| `-enable` | turn it on |
| `-disable` | turn it off |
| `-port PORT` | serve on this port |

#### janusctl system node-exporter

Prometheus-node-exporter: show or change.

```text
janusctl system node-exporter [-enable] [-disable] [-address IP] [-port PORT] [-collectors A,B]
```

| Flag | |
|---|---|
| `-enable` | run it |
| `-disable` | stop it, and keep it stopped |
| `-address IP` | listen on this address only (* for all) |
| `-port PORT` | listen on this port |
| `-collectors A,B` | the collectors to run |

#### janusctl system sysctl

Kernel parameters: HAProxy's on trial, the CIS benchmark's read-only (docs/guide/kernel-tuning.md).

##### janusctl system sysctl list

The parameters, their values and defaults, the CIS benchmark.

```text
janusctl system sysctl list [-cis]
```

| Flag | |
|---|---|
| `-cis` | every CIS control too |

##### janusctl system sysctl get

One parameter: value, default, bounds, effect on HAProxy, risk.

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

#### janusctl system reboot

Soft-stop HAProxy, then reboot.

```text
janusctl system reboot [-powercycle]
```

| Flag | |
|---|---|
| `-powercycle` | a power cycle |

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

The running haproxy.cfg.

```text
janusctl haproxy get-config
```

#### janusctl haproxy apply-config

Validate, apply and reload seamlessly.

```text
janusctl haproxy apply-config FILE
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

## Network

### janusctl network

Addresses, routes, firewall, VRRP, BGP, Consul.

#### janusctl network status

Interfaces, addresses, routes, DNS, clock.

```text
janusctl network status
```

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
janusctl lifecycle install [-sha256 HEX] [-controller-address HOST:PORT] [-controller-ca FILE] [-controller-fleet-root FILE] [-network-config FILE] [-fleet-root FILE] [-fleet-bundle FILE] [-registration-token TOKEN] [-insecure-skip-signature-check] DISK BUNDLE_DIR
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
| `-insecure-skip-signature-check` | accept unsigned UKIs - development only |

#### janusctl lifecycle upgrade

Write a release to the inactive slot and reboot into it.

```text
janusctl lifecycle upgrade [-sha256 HEX] [-wait-for-health] [-health-timeout SECONDS] [-insecure-skip-signature-check] [-allow-schematic-change] BUNDLE_DIR|URL
```

| Flag | |
|---|---|
| `-sha256 HEX` | rootfs.squashfs's expected SHA-256 |
| `-wait-for-health` | revert by itself if the new slot isn't healthy |
| `-health-timeout SECONDS` | how long it has to be healthy |
| `-insecure-skip-signature-check` | accept unsigned UKIs - development only |
| `-allow-schematic-change` | accept another image schematic |

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
