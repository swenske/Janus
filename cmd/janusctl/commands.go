package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// The command tree: every command janusctl dispatches, its flags and
// positional arguments - what `janusctl help` prints, what shell
// completion (complete.go) and the interactive pickers (pick.go) offer.
// The dispatch itself stays in each run* function's switch;
// commands_test.go checks the two agree, flags included.

// argKind is what a positional argument or a flag's value is - what
// completing it offers.
type argKind int

const (
	argNone          argKind = iota
	argFile                  // a file on this machine
	argDir                   // a directory on this machine
	argRemote                // a path on the node
	argContext               // a context's name
	argNodes                 // node names or IDs, comma-separated (-n)
	argFleetNode             // a node of the context
	argIssuer                // an issuing CA of the local fleet
	argRole                  // os:admin, os:operator, os:reader
	argService               // a managed service's ID
	argServiceAction         // start, stop, restart
	argLogService            // janusd, haproxy
	argMap                   // a map of the running config
	argMapKey                // a key of the map given before it
	argCert                  // a certificate in HAProxy's store
	argHAProxyFile           // a file in /etc/haproxy/files
	argACMEName              // a Let's Encrypt certificate's name
	argFamily                // an nftables family
	argTable                 // an nftables table
	argSet                   // an nftables named set
	argInterface             // a network interface of the node
	argContextAction         // list, use, delete
	argShell                 // bash, zsh, fish
	argSysctl                // a kernel parameter the node lets change
	argSysctlAssign          // NAME=VALUE for such a parameter
)

// fixedValues are the kinds whose values are known here.
var fixedValues = map[argKind][]candidate{
	argRole:          {{"os:admin", "everything"}, {"os:operator", "runs the node: HAProxy, services, reboots"}, {"os:reader", "reads only"}},
	argServiceAction: {{"start", ""}, {"stop", ""}, {"restart", ""}},
	argLogService:    {{"janusd", "the node's daemon"}, {"haproxy", "HAProxy's output"}},
	argFamily:        {{"inet", "IPv4 and IPv6"}, {"ip", "IPv4"}, {"ip6", "IPv6"}, {"arp", ""}, {"bridge", ""}, {"netdev", ""}},
	argContextAction: {{"list", "the contexts"}, {"use", "switch to a context"}, {"delete", "forget a context"}},
	argShell:         {{"bash", ""}, {"zsh", ""}, {"fish", ""}},
}

// flagDef is a flag: value is its placeholder ("" for a boolean).
type flagDef struct {
	name, value, help string
	kind              argKind
}

// command is a node of the tree: a group (subs) or a command (args).
type command struct {
	name string
	// args is the positional arguments' usage, help what it does.
	args, help string
	// group heads a top-level command's section in the help.
	group string
	flags []flagDef
	// pos are the positional arguments' kinds, the last one repeated
	// when variadic; required counts those that must be given.
	pos      []argKind
	required int
	variadic bool
	subs     []*command
	// offline: dispatched before any node connection (main's first
	// switch) - no context needed.
	offline bool
	// note follows a group's usage line.
	note   string
	hidden bool
}

// valFlag is a flag taking a value, boolFlag one that doesn't.
func valFlag(name, value, help string, kind argKind) flagDef { return flagDef{name, value, help, kind} }
func boolFlag(name, help string) flagDef                     { return flagDef{name: name, help: help} }

var (
	timeoutFlag = valFlag("timeout", "DURATION", "how long the node waits for the confirmation before reverting", argNone)
	noConfirm   = boolFlag("no-confirm", "apply only - confirm yourself before the timeout")
	// sysctlApplyFlags are system sysctl set's and reset's.
	sysctlApplyFlags = []flagDef{timeoutFlag, noConfirm, boolFlag("no-reload", "don't reload HAProxy for what it reads at its listeners")}
	reloadFlag       = boolFlag("reload", "HAProxy uses it at once")
	kitFlag          = valFlag("kit", "KIT", "the fleet's recovery kit", argFile)
)

// globalFlags come before the command.
var globalFlags = []flagDef{
	valFlag("context", "NAME", "the context to use (default: the current one)", argContext),
	valFlag("n", "NODE[,NODE]", "the node(s) to run the command on - '?' to pick", argNodes),
	boolFlag("all", "run the command on every node of the fleet"),
	valFlag("endpoint", "HOST:PORT", "a node's API, with its own certificate", argNone),
	valFlag("ca", "FILE", "the node's CA certificate", argFile),
	valFlag("cert", "FILE", "the client certificate", argFile),
	valFlag("key", "FILE", "the client private key", argFile),
	valFlag("as-user", "NAME", "with a Controller certificate: the user the calls are made for", argNone),
	valFlag("as-roles", "ROLES", "with -as-user: that user's roles, comma-separated", argRole),
}

var commands = &command{name: "janusctl", subs: []*command{
	{name: "login", group: "🔑 Sign in", offline: true, help: "sign in to a Controller: a certificate of its fleet, and its nodes", flags: []flagDef{
		valFlag("context", "NAME", "the context to sign in", argContext),
		valFlag("controller", "HOST[:PORT]", "the Controller's address (the first time)", argNone),
		valFlag("controller-ca", "FILE", "check the Controller against this CA (or certificate)", argFile),
		valFlag("controller-fingerprint", "SHA256", "trust the Controller's certificate with this SHA-256", argNone),
		valFlag("user", "NAME", "your account (with an SSH key)", argNone),
		valFlag("ssh-key", "FILE", "the SSH key: its private key file, or its .pub for ssh-agent", argFile),
		boolFlag("browser", "sign in on the Controller's page, in the browser"),
		boolFlag("no-open", "with -browser: only print the page's address"),
		boolFlag("device", "sign in on the Controller's page from another machine"),
	}},
	{name: "context", offline: true, args: "[list | use NAME | delete NAME]", help: "the Controllers and fleets signed in to", pos: []argKind{argContextAction, argContext}},
	{name: "nodes", offline: true, help: "the context's nodes (refreshed with JANUS_TOKEN)"},
	{name: "version", group: "🖥  Node", help: "janusctl's version, and the node's"},
	{name: "system", help: "the node: state, logs, files, power", subs: []*command{
		{name: "info", help: "version, kernel, slot, memory, CPU, load, disks"},
		{name: "hostname", help: "the node's hostname"},
		{name: "services", help: "managed services and their health"},
		{name: "service", args: "start|stop|restart ID", help: "control a managed service (janusd: restart only)", pos: []argKind{argServiceAction, argService}, required: 2},
		{name: "logs", args: "SERVICE", help: "janusd or haproxy output", flags: []flagDef{boolFlag("f", "follow"), valFlag("n", "LINES", "only the last LINES lines", argNone)}, pos: []argKind{argLogService}, required: 1},
		{name: "events", help: "the node's event log, live", flags: []flagDef{valFlag("since", "ID", "only events after this ID", argNone)}},
		{name: "dmesg", help: "the kernel's messages", flags: []flagDef{boolFlag("f", "follow")}},
		{name: "stats", help: "CPU and memory of janusd and haproxy"},
		{name: "systemstat", help: "boot time, context switches, processes created"},
		{name: "ps", help: "every process"},
		{name: "netdev", help: "network interface counters"},
		{name: "netstat", help: "TCP and UDP sockets"},
		{name: "mounts", help: "mounted filesystems"},
		{name: "du", args: "PATH...", help: "disk usage", flags: []flagDef{boolFlag("r", "one line per directory")}, pos: []argKind{argRemote}, required: 1, variadic: true},
		{name: "ls", args: "PATH", help: "list a directory", flags: []flagDef{boolFlag("r", "recursive")}, pos: []argKind{argRemote}, required: 1},
		{name: "cat", args: "PATH", help: "print a file", pos: []argKind{argRemote}, required: 1},
		{name: "cp", args: "PATH", help: "a tar archive of PATH (stdout by default)", flags: []flagDef{valFlag("o", "FILE", "the archive's file, - for stdout", argFile)}, pos: []argKind{argRemote}, required: 1},
		{name: "pcap", help: "live packet capture, as a pcap file (docs/packet-capture.md)", flags: []flagDef{
			valFlag("i", "IFACE", "the interface to capture on", argInterface),
			valFlag("f", "FILTER", "a tcpdump-style filter", argNone),
			boolFlag("promisc", "promiscuous mode"),
			boolFlag("include-own-stream", "capture this capture's own connection too"),
			valFlag("snaplen", "BYTES", "bytes kept per packet", argNone),
			valFlag("duration", "DURATION", "stop after this long", argNone),
			valFlag("o", "FILE", "the pcap file, - for stdout", argFile),
		}},
		{name: "metrics", help: "the node's Prometheus exporter: show or change", flags: []flagDef{boolFlag("enable", "turn it on"), boolFlag("disable", "turn it off"), valFlag("port", "PORT", "serve on this port", argNone)}},
		{name: "node-exporter", help: "prometheus-node-exporter: show or change", flags: []flagDef{
			boolFlag("enable", "run it"), boolFlag("disable", "stop it, and keep it stopped"),
			valFlag("address", "IP", "listen on this address only (* for all)", argNone),
			valFlag("port", "PORT", "listen on this port", argNone),
			valFlag("collectors", "A,B", "the collectors to run", argNone),
		}},
		{name: "sysctl", help: "kernel parameters: HAProxy's on trial, the CIS benchmark's read-only (docs/guide/kernel-tuning.md)", subs: []*command{
			{name: "list", help: "the parameters, their values, defaults and suggested values, the CIS benchmark", flags: []flagDef{boolFlag("cis", "every CIS control too")}},
			{name: "get", args: "NAME", help: "one parameter: value, default, bounds, effect on HAProxy, risk, a suggested value and why", pos: []argKind{argSysctl}, required: 1},
			{name: "set", args: "NAME=VALUE...", help: "change parameters on trial, then confirm them", flags: sysctlApplyFlags, pos: []argKind{argSysctlAssign}, required: 1, variadic: true},
			{name: "reset", args: "NAME...", help: "put parameters back to Janus's defaults on trial, then confirm", flags: append(slices.Clone(sysctlApplyFlags), boolFlag("all", "every parameter")), pos: []argKind{argSysctl}, variadic: true},
			{name: "confirm", help: "save the values on trial: every boot applies them"},
			{name: "cancel", help: "put the values from before the trial back now"},
			{name: "history", help: "who changed what, when", flags: []flagDef{valFlag("n", "N", "the newest N changes", argNone)}},
			{name: "observed", help: "what the node observed for its suggestions: each signal, by hour", flags: []flagDef{boolFlag("v", "what each signal measures, and when an hour counts")}},
		}},
		{name: "reboot", help: "soft-stop HAProxy, then reboot", flags: []flagDef{boolFlag("powercycle", "a power cycle")}},
		{name: "shutdown", help: "soft-stop HAProxy, then power off"},
		{name: "restart", help: "restart janusd only - HAProxy keeps serving"},
		{name: "reset", help: "wipe the STATE partition and reboot: a new CA on the console", flags: []flagDef{boolFlag("wipe-state", "wipe the persistent STATE partition"), boolFlag("wipe-ephemeral", "wipe ephemeral state")}},
	}},
	{name: "haproxy", group: "⚖️  HAProxy", help: "HAProxy: config, runtime, certificates, files", subs: []*command{
		{name: "show-info", help: "version, uptime, connections"},
		{name: "stats", help: "raw 'show stat' CSV"},
		{name: "backends", help: "backends, their servers and states"},
		{name: "get-config", help: "the running haproxy.cfg"},
		{name: "apply-config", args: "FILE", help: "validate, apply and reload seamlessly", pos: []argKind{argFile}, required: 1},
		{name: "map-list", help: "the running config's file-backed maps"},
		{name: "map-get", args: "MAP", help: "a map's entries", pos: []argKind{argMap}, required: 1},
		{name: "map-set", args: "MAP KEY VALUE", help: "set one entry", pos: []argKind{argMap, argMapKey, argNone}, required: 3},
		{name: "map-delete", args: "MAP KEY", help: "delete one entry", pos: []argKind{argMap, argMapKey}, required: 2},
		{name: "acl-add", args: "ACL VALUE", help: "add a pattern to an ACL", required: 2},
		{name: "acl-delete", args: "ACL VALUE", help: "delete a pattern from an ACL", required: 2},
		{name: "cert-list", help: "certificates in HAProxy's store"},
		{name: "cert-upload", args: "NAME FILE", help: "upload a PEM certificate and key as NAME", flags: []flagDef{
			valFlag("crt-list", "PATH", "bind it into this crt-list", argNone),
			valFlag("sni", "HOST,HOST", "with -crt-list: the SNI names", argNone),
		}, pos: []argKind{argNone, argFile}, required: 2},
		{name: "cert-delete", args: "NAME", help: "delete a certificate", flags: []flagDef{valFlag("crt-list", "PATH", "unbind it from this crt-list first", argNone)}, pos: []argKind{argCert}, required: 1},
		{name: "files", help: "HAProxy's own files (/etc/haproxy/files)"},
		{name: "file-get", args: "NAME", help: "print a file", pos: []argKind{argHAProxyFile}, required: 1},
		{name: "file-put", args: "NAME FILE", help: "write a file", flags: []flagDef{reloadFlag}, pos: []argKind{argHAProxyFile, argFile}, required: 2},
		{name: "file-delete", args: "NAME", help: "remove a file", flags: []flagDef{reloadFlag}, pos: []argKind{argHAProxyFile}, required: 1},
		{name: "acme", help: "Let's Encrypt (letsencrypt extension)", subs: []*command{
			{name: "status", help: "the account, each certificate's state and expiry"},
			{name: "get", help: "the configuration, as JSON"},
			{name: "check", args: "FILE", help: "check a configuration", flags: []flagDef{valFlag("account-key", "FILE", "an existing account's private key", argFile)}, pos: []argKind{argFile}, required: 1},
			{name: "apply", args: "FILE", help: "save a configuration", flags: []flagDef{valFlag("account-key", "FILE", "an existing account's private key", argFile)}, pos: []argKind{argFile}, required: 1},
			{name: "renew", args: "[NAME...]", help: "obtain certificates now", pos: []argKind{argACMEName}, variadic: true},
		}},
	}},
	{name: "network", group: "🌐 Network", help: "addresses, routes, firewall, VRRP, BGP, Consul", subs: []*command{
		{name: "status", help: "interfaces, addresses, routes, DNS, clock"},
		{name: "get", help: "the network configuration, as JSON"},
		{name: "apply", args: "FILE", help: "apply a configuration on trial, then confirm it", flags: []flagDef{timeoutFlag, noConfirm}, pos: []argKind{argFile}, required: 1},
		{name: "confirm", help: "confirm the configuration on trial"},
		{name: "modules", help: "optional modules and whether this image has them"},
		{name: "firewall", help: "nftables (nftables extension)", subs: []*command{
			{name: "status", help: "saved, on trial, live"},
			{name: "get", help: "the saved ruleset"},
			{name: "check", args: "FILE", help: "validate a ruleset", pos: []argKind{argFile}, required: 1},
			{name: "apply", args: "FILE", help: "apply a ruleset on trial, then confirm it", flags: []flagDef{timeoutFlag, noConfirm}, pos: []argKind{argFile}, required: 1},
			{name: "confirm", help: "confirm the ruleset on trial"},
			{name: "sets", help: "the live named sets and their elements"},
			{name: "set-add", args: "FAMILY TABLE SET ELEMENT...", help: "add elements live", flags: []flagDef{valFlag("timeout", "DURATION", "the elements' timeout", argNone)}, pos: []argKind{argFamily, argTable, argSet, argNone}, required: 4, variadic: true},
			{name: "set-del", args: "FAMILY TABLE SET ELEMENT...", help: "delete elements", flags: []flagDef{valFlag("timeout", "DURATION", "unused", argNone)}, pos: []argKind{argFamily, argTable, argSet, argNone}, required: 4, variadic: true},
		}},
		{name: "vrrp", help: "VRRP (keepalived extension)", subs: []*command{
			{name: "status", help: "each instance's state and virtual IPs"},
			{name: "get", help: "the saved keepalived.conf"},
			{name: "check", args: "FILE", help: "have keepalived check it", pos: []argKind{argFile}, required: 1},
			{name: "apply", args: "FILE", help: "check, save and reload it", pos: []argKind{argFile}, required: 1},
		}},
		{name: "bgp", help: "BGP (bird extension)", subs: []*command{
			{name: "status", help: "protocols, sessions, routes"},
			{name: "get", help: "the saved bird.conf"},
			{name: "check", args: "FILE", help: "have BIRD check it", pos: []argKind{argFile}, required: 1},
			{name: "apply", args: "FILE", help: "check, save and reconfigure", pos: []argKind{argFile}, required: 1},
		}},
		{name: "consul", help: "Consul agent (consul extension)", subs: []*command{
			{name: "status", help: "the service, the node, its cluster"},
			{name: "get", help: "the saved configuration"},
			{name: "check", args: "FILE", help: "have consul validate it", flags: []flagDef{valFlag("file", "NAME=PATH", "a file it names (repeatable)", argFile), boolFlag("only-files", "unused here")}, pos: []argKind{argFile}, required: 1},
			{name: "apply", args: "FILE", help: "check, save and apply (the agent restarts)", flags: []flagDef{valFlag("file", "NAME=PATH", "a file it names (repeatable)", argFile), boolFlag("only-files", "drop the saved files not given")}, pos: []argKind{argFile}, required: 1},
		}},
	}},
	{name: "access", group: "🔐 Trust", help: "the fleet the node trusts, its own CA", subs: []*command{
		{name: "trust", help: "the fleet the node trusts besides its own CA"},
		{name: "trust-set", args: "BUNDLE", help: "pin the fleet's root and apply a bundle", flags: []flagDef{valFlag("root", "FILE", "the fleet's root (the first time)", argFile)}, pos: []argKind{argFile}, required: 1},
		{name: "trust-reset", help: "forget the fleet (the node's own CA only)"},
		{name: "rotate-ca", args: "DIR", help: "replace the node's own CA", flags: []flagDef{boolFlag("console", "the node prints the admin key on its console")}, pos: []argKind{argDir}, required: 1},
	}},
	{name: "pki", help: "client certificates of the node's own CA", subs: []*command{
		{name: "generate-client-config", args: "DIR", help: "issue a client certificate into DIR", flags: []flagDef{
			valFlag("role", "ROLE", "os:admin, os:operator or os:reader", argRole),
			valFlag("name", "NAME", "its common name", argNone),
			valFlag("ttl", "DURATION", "how long it's valid (a year at most)", argNone),
		}, pos: []argKind{argDir}, required: 1},
	}},
	{name: "fleet", offline: true, help: "a fleet without a Controller (docs/fleet-without-controller.md)", note: "The kit's passphrase is asked, or read from JANUS_KIT_PASSPHRASE.", subs: []*command{
		{name: "init", args: "KIT", help: "a new fleet: its root's key in KIT", flags: []flagDef{
			valFlag("name", "FLEET", "the fleet's name", argNone), valFlag("issuer", "NAME", "this machine's issuing CA's name", argNone),
			valFlag("user", "NAME", "who this machine's certificates are for", argNone), valFlag("role", "ROLE", "their role", argRole),
			boolFlag("yes", "don't ask the passphrase back"),
		}, pos: []argKind{argFile}, required: 1},
		{name: "recover", help: "a fleet from its kit", flags: []flagDef{
			kitFlag, valFlag("name", "FLEET", "the fleet's name", argNone), valFlag("issuer", "NAME", "this machine's issuing CA's name", argNone),
			valFlag("user", "NAME", "who this machine's certificates are for", argNone), valFlag("role", "ROLE", "their role", argRole),
		}},
		{name: "adopt", args: "NAME", help: "a node into the fleet", flags: []flagDef{
			valFlag("endpoint", "HOST:PORT", "the node's API", argNone),
			valFlag("ca", "FILE", "its first boot's CA (ca.crt)", argFile), valFlag("cert", "FILE", "admin.crt", argFile), valFlag("key", "FILE", "admin.key", argFile),
			valFlag("ca-fingerprint", "SHA256", "a node already in the fleet: its CA's SHA-256", argNone), kitFlag,
		}, required: 1},
		{name: "sync", help: "the newest bundle everywhere", flags: []flagDef{kitFlag}},
		{name: "status", help: "each node's bundle, this machine's certificate"},
		{name: "export", args: "DIR", help: "root.crt, bundle.json, user-data.json", pos: []argKind{argDir}, required: 1},
		{name: "forget", args: "NAME", help: "a node out of this context", pos: []argKind{argFleetNode}, required: 1},
		{name: "issuer", help: "the fleet's issuing CAs", subs: []*command{
			{name: "list", help: "the issuing CAs of the bundle"},
			{name: "request", args: "REQUEST", help: "a new machine: REQUEST to sign where the kit is", flags: []flagDef{
				valFlag("issuer", "NAME", "this machine's issuing CA's name", argNone), valFlag("user", "NAME", "who its certificates are for", argNone), valFlag("role", "ROLE", "the most they carry", argRole),
			}, pos: []argKind{argFile}, required: 1},
			{name: "sign", args: "REQUEST GRANT", help: "sign a machine's issuing CA into the bundle", flags: []flagDef{kitFlag, boolFlag("replace", "replace an issuing CA of that name"), valFlag("role", "ROLE", "the most its certificates carry", argRole)}, pos: []argKind{argFile, argFile}, required: 2},
			{name: "accept", args: "GRANT", help: "the machine's issuing CA, the fleet and its nodes", pos: []argKind{argFile}, required: 1},
			{name: "revoke", args: "NAME", help: "a bundle without NAME's issuing CA", flags: []flagDef{kitFlag}, pos: []argKind{argIssuer}, required: 1},
		}},
	}},
	{name: "lifecycle", group: "🚀 Lifecycle", help: "install, upgrade, roll back", subs: []*command{
		{name: "install", args: "DISK BUNDLE_DIR", help: "partition a blank disk and write a release (paths on the node)", flags: []flagDef{
			valFlag("sha256", "HEX", "rootfs.squashfs's expected SHA-256", argNone),
			valFlag("controller-address", "HOST:PORT", "a Controller to self-register with", argNone),
			valFlag("controller-ca", "FILE", "the Controller's CA", argFile),
			valFlag("controller-fleet-root", "FILE", "the Controller's fleet root", argFile),
			valFlag("network-config", "FILE", "a network configuration (JSON)", argFile),
			valFlag("fleet-root", "FILE", "a fleet to trust from the first boot", argFile),
			valFlag("fleet-bundle", "FILE", "its bundle", argFile),
			valFlag("registration-token", "TOKEN", "a Controller enrollment token", argNone),
			boolFlag("insecure-skip-signature-check", "accept unsigned UKIs - development only"),
		}, pos: []argKind{argRemote, argRemote}, required: 2},
		{name: "upgrade", args: "BUNDLE_DIR|URL", help: "write a release to the inactive slot and reboot into it", flags: []flagDef{
			valFlag("sha256", "HEX", "rootfs.squashfs's expected SHA-256", argNone),
			boolFlag("wait-for-health", "revert by itself if the new slot isn't healthy"),
			valFlag("health-timeout", "SECONDS", "how long it has to be healthy", argNone),
			boolFlag("insecure-skip-signature-check", "accept unsigned UKIs - development only"),
			boolFlag("allow-schematic-change", "accept another image schematic"),
		}, pos: []argKind{argRemote}, required: 1},
		{name: "rollback", help: "boot the other slot"},
		{name: "upload-release", args: "BUNDLE_DIR", help: "stream a local release bundle to the node", pos: []argKind{argDir}, required: 1},
	}},
	{name: "image", offline: true, help: "write onto a disk image, offline", subs: []*command{
		{name: "seed-controller", args: "DISK", help: "a Controller to self-register with", flags: []flagDef{
			valFlag("controller-address", "HOST:PORT", "the Controller's registration address", argNone),
			valFlag("controller-ca", "FILE", "its CA", argFile),
			valFlag("controller-fleet-root", "FILE", "its fleet root", argFile),
			valFlag("registration-token", "TOKEN", "an enrollment token", argNone),
		}, pos: []argKind{argFile}, required: 1},
		{name: "seed-fleet", args: "DISK", help: "a fleet to trust from the first boot", flags: []flagDef{valFlag("fleet-root", "FILE", "the fleet's root", argFile), valFlag("fleet-bundle", "FILE", "its bundle", argFile)}, pos: []argKind{argFile}, required: 1},
		{name: "seed-network", args: "DISK", help: "a network configuration from the first boot", flags: []flagDef{valFlag("config", "FILE", "the configuration (JSON)", argFile)}, pos: []argKind{argFile}, required: 1},
	}},
	{name: "completion", group: "✨ Shell", offline: true, args: "bash|zsh|fish", help: "the shell's completion script (README: Shell completion)", pos: []argKind{argShell}, required: 1},
	{name: "help", offline: true, args: "[COMMAND...]", help: "this help, or a command's", hidden: false},
	{name: "__complete", offline: true, hidden: true},
}}

// find walks path down the tree: the deepest command it names, and the
// words left.
func (c *command) find(path []string) (*command, []string) {
	for len(path) > 0 {
		next := c.sub(path[0])
		if next == nil {
			break
		}
		c, path = next, path[1:]
	}
	return c, path
}

func (c *command) sub(name string) *command {
	for _, s := range c.subs {
		if s.name == name {
			return s
		}
	}
	return nil
}

func (c *command) flag(name string) *flagDef {
	for i := range c.flags {
		if c.flags[i].name == name {
			return &c.flags[i]
		}
	}
	return nil
}

func flagUsage(fl flagDef) string {
	if fl.value == "" {
		return "[-" + fl.name + "]"
	}
	return "[-" + fl.name + " " + fl.value + "]"
}

// leaves calls fn for every command under c (not the groups), with its
// path below the root.
func (c *command) leaves(path []string, fn func(path []string, leaf *command)) {
	for _, s := range c.subs {
		if s.hidden {
			continue
		}
		p := append(append([]string{}, path...), s.name)
		if len(s.subs) == 0 {
			fn(p, s)
			continue
		}
		s.leaves(p, fn)
	}
}

// printHelp writes c's help - path is c's place in the tree: every
// command for the root and a group (their arguments, what they do), a
// command's flags too.
func printHelp(w io.Writer, c *command, path []string) {
	st := styleFor(w)
	if len(c.subs) == 0 && c != commands {
		printCommandHelp(w, st, c, path)
		return
	}
	if c == commands {
		fmt.Fprintln(w, st.bold("janusctl")+st.dim(" - Janus nodes from your terminal, over their mTLS API"))
		fmt.Fprintln(w)
		fmt.Fprintln(w, "usage: janusctl "+st.dim("[-context NAME] [-n NODE[,NODE] | -all]")+" COMMAND ...")
		fmt.Fprintln(w, "       janusctl "+st.dim("-endpoint HOST:PORT -ca FILE -cert FILE -key FILE")+" COMMAND ...")
	} else {
		fmt.Fprintln(w, "usage: janusctl "+strings.Join(path, " ")+" COMMAND"+st.dim("   "+c.help))
		if c.note != "" {
			fmt.Fprintln(w, st.dim(c.note))
		}
	}
	// Collected first: one column width for every group.
	const widest = 36
	type row struct{ header, cmd, args, help string }
	var rows []row
	group := ""
	var walk func(c *command, prefix []string, top bool)
	walk = func(c *command, prefix []string, top bool) {
		for _, s := range c.subs {
			if s.hidden {
				continue
			}
			if top && s.group != "" && s.group != group {
				group = s.group
				rows = append(rows, row{header: group})
			}
			p := append(append([]string{}, prefix...), s.name)
			if len(s.subs) > 0 {
				walk(s, p, false)
				continue
			}
			rows = append(rows, row{cmd: strings.Join(p, " "), args: s.args, help: s.help})
		}
	}
	if c == commands {
		walk(c, nil, true)
	} else {
		rows = append(rows, row{header: " "})
		walk(c, path, false)
	}
	width := 0
	for _, r := range rows {
		if n := len(r.cmd) + len(r.args) + 1; r.header == "" && n <= widest {
			width = max(width, n)
		}
	}
	for _, r := range rows {
		if r.header != "" {
			fmt.Fprintln(w)
			if r.header != " " {
				fmt.Fprintln(w, st.bold(r.header))
			}
			continue
		}
		left := st.bold(r.cmd)
		n := len(r.cmd)
		if r.args != "" {
			left += " " + st.cyan(r.args)
			n += 1 + len(r.args)
		}
		if n > widest {
			fmt.Fprintf(w, "  %s\n  %s  %s\n", left, strings.Repeat(" ", width), st.dim(r.help))
			continue
		}
		fmt.Fprintf(w, "  %s%s  %s\n", left, strings.Repeat(" ", width-n), st.dim(r.help))
	}
	fmt.Fprintln(w)
	if c == commands {
		fmt.Fprintln(w, st.dim("janusctl help COMMAND: its flags. janusctl alone, in a terminal: pick a command."))
		fmt.Fprintln(w, st.dim("Completion: janusctl completion bash|zsh|fish (README)."))
	} else {
		fmt.Fprintln(w, st.dim("janusctl help "+strings.Join(path, " ")+" COMMAND: its flags."))
	}
}

// printCommandHelp is one command's help: its usage, its flags.
func printCommandHelp(w io.Writer, st style, c *command, path []string) {
	line := "usage: janusctl " + strings.Join(path, " ")
	for _, fl := range c.flags {
		line += " " + st.dim(flagUsage(fl))
	}
	if c.args != "" {
		line += " " + st.cyan(c.args)
	}
	fmt.Fprintln(w, line)
	fmt.Fprintln(w, "  "+c.help)
	if len(c.flags) == 0 {
		return
	}
	fmt.Fprintln(w)
	width := 0
	for _, fl := range c.flags {
		width = max(width, len(fl.name)+len(fl.value)+2)
	}
	for _, fl := range c.flags {
		left := "-" + fl.name
		if fl.value != "" {
			left += " " + fl.value
		}
		fmt.Fprintf(w, "  %s%s  %s\n", st.bold(left), strings.Repeat(" ", width-len(left)), st.dim(fl.help))
	}
}

// usage prints the help of what the command line names as far as the
// tree knows it - the whole tree when nothing - on stderr.
func usage() {
	args := []string{}
	if len(os.Args) > 1 {
		args = commandWords(os.Args[1:])
	}
	c, _ := commands.find(args)
	path := args[:depth(c, args)]
	printHelp(os.Stderr, c, path)
}

// depth is how many of args lead to c.
func depth(c *command, args []string) int {
	n := 0
	cur := commands
	for n < len(args) && cur != c {
		cur = cur.sub(args[n])
		if cur == nil {
			return n
		}
		n++
	}
	return n
}

// commandWords are args without the global flags before the command.
func commandWords(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		name := strings.TrimLeft(args[0], "-")
		args = args[1:]
		if strings.Contains(name, "=") {
			continue
		}
		for _, g := range globalFlags {
			if g.name == name && g.value != "" && len(args) > 0 {
				args = args[1:]
			}
		}
	}
	return args
}

// runHelp is janusctl help [COMMAND...].
func runHelp(args []string) {
	c, rest := commands.find(args)
	if len(rest) > 0 {
		fmt.Fprintf(os.Stderr, "janusctl help: no command %q\n", strings.Join(args, " "))
		os.Exit(2)
	}
	printHelp(os.Stdout, c, args)
}
