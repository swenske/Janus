package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/swenske/Janus/internal/termui"
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
	argTheme                 // a janusctl tui colour theme
	argStickTable            // a stick table of the running config
	argProbeKind             // dns, tcp, tls, http, ping, traceroute
	argBackend               // a backend of the running config
	argServer                // a server of the backend given before it
	argServerState           // ready, drain, maint
)

// fixedValues are the kinds whose values are known here.
var fixedValues = map[argKind][]candidate{
	argRole:          {{"os:admin", "everything"}, {"os:operator", "runs the node: HAProxy, services, reboots"}, {"os:reader", "reads only"}},
	argServiceAction: {{"start", ""}, {"stop", ""}, {"restart", ""}},
	argLogService:    {{"janusd", "the node's daemon"}, {"haproxy", "HAProxy's output"}},
	argFamily:        {{"inet", "IPv4 and IPv6"}, {"ip", "IPv4"}, {"ip6", "IPv6"}, {"arp", ""}, {"bridge", ""}, {"netdev", ""}},
	argContextAction: {{"list", "the contexts"}, {"use", "switch to a context"}, {"delete", "forget a context"}},
	argShell:         {{"bash", ""}, {"zsh", ""}, {"fish", ""}},
	argTheme:         themeCandidates(),
	argProbeKind: {{"dns", "a lookup"}, {"tcp", "a connection to host:port"}, {"tls", "a handshake: the certificate presented"}, {"http", "a request to a URL"},
		{"ping", "ICMP echo requests"}, {"traceroute", "the routers on the way"}},
	argServerState: {{"ready", "back in service"}, {"drain", "new clients go elsewhere, the current ones finish"}, {"maint", "no traffic, no health checks"}},
}

// themeCandidates are the dashboard's themes, then list.
func themeCandidates() []candidate {
	out := []candidate{}
	for _, t := range termui.Themes() {
		out = append(out, candidate{t.Name, t.About})
	}
	return append(out, candidate{"list", "print every theme"})
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
	// haproxy acme path / test-path: a proxy's settings, or the relay's.
	acmePathFlags = []flagDef{
		valFlag("user", "NAME", "the proxy's username", argNone),
		valFlag("password-file", "FILE", "a file holding the proxy's password (kept when not given)", argFile),
		valFlag("ca", "FILE", "the CA (PEM) of an https:// proxy's certificate; with controller, the Controller's identity", argFile),
	}
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
	{name: "tui", help: "a live dashboard in the terminal: the node, or every node of the context", flags: []flagDef{
		valFlag("interval", "DURATION", "how often the nodes are asked (default 2s, 1s to 30s)", argNone),
		boolFlag("once", "print one frame as text and exit"),
		valFlag("theme", "NAME", "the colour theme for this run (list: every theme; M in the dashboard saves one)", argTheme),
	}},
	{name: "system", help: "the node: state, logs, files, power", subs: []*command{
		{name: "info", help: "version, kernel, slot, memory, CPU, load, disks"},
		{name: "hostname", help: "the node's hostname"},
		{name: "services", help: "managed services and their health"},
		{name: "service", args: "start|stop|restart ID", help: "control a managed service (janusd: restart only)", pos: []argKind{argServiceAction, argService}, required: 2},
		{name: "logs", args: "SERVICE", help: "janusd or haproxy output", flags: []flagDef{boolFlag("f", "follow"), valFlag("n", "LINES", "only the last LINES lines", argNone), valFlag("boot", "N", "a previous boot's: -1 the one before this one (system boots)", argNone)}, pos: []argKind{argLogService}, required: 1},
		{name: "events", help: "the node's event log, live", flags: []flagDef{valFlag("since", "ID", "only events after this ID", argNone)}},
		{name: "audit", help: "who changed what, from where, how it ended - and every call refused, live", flags: []flagDef{valFlag("since", "ID", "only entries after this ID", argNone)}},
		{name: "dmesg", help: "the kernel's messages", flags: []flagDef{boolFlag("f", "follow"), valFlag("boot", "N", "a previous boot's: -1 the one before this one (system boots)", argNone)}},
		{name: "boots", help: "the boots whose logs the LOGS partition keeps"},
		{name: "log-forwarding", help: "where the node's logs go: syslog, Elasticsearch (docs/guide/log-forwarding.md)", subs: []*command{
			{name: "status", help: "each target's state: sent, dropped, waiting, the last error"},
			{name: "get", help: "the configuration as JSON, without its secrets"},
			{name: "set", args: "FILE", help: "replace the configuration (JSON, as get prints it; a secret left out keeps its saved value)", pos: []argKind{argFile}, required: 1},
			{name: "test", args: "TARGET", help: "send one record to a target now", flags: []flagDef{valFlag("file", "FILE", "take the target from this configuration rather than the saved one", argFile)}, required: 1},
		}},
		{name: "stats", help: "CPU and memory of janusd and haproxy"},
		{name: "systemstat", help: "boot time, context switches, processes created, the CPUs' ticks (idle, steal)"},
		{name: "ps", help: "every process"},
		{name: "netdev", help: "network interface counters"},
		{name: "netstat", help: "TCP and UDP sockets"},
		{name: "clients", help: "the connections to the API: a Controller and the users it acts for, janusctl..."},
		{name: "mounts", help: "mounted filesystems"},
		{name: "du", args: "PATH...", help: "disk usage", flags: []flagDef{boolFlag("r", "one line per directory")}, pos: []argKind{argRemote}, required: 1, variadic: true},
		{name: "ls", args: "PATH", help: "list a directory", flags: []flagDef{boolFlag("r", "recursive")}, pos: []argKind{argRemote}, required: 1},
		{name: "cat", args: "PATH", help: "print a file", pos: []argKind{argRemote}, required: 1},
		{name: "cp", args: "PATH", help: "a tar archive of PATH (stdout by default)", flags: []flagDef{valFlag("o", "FILE", "the archive's file, - for stdout", argFile)}, pos: []argKind{argRemote}, required: 1},
		{name: "support", help: "a diagnostic bundle of the node, without its secrets (docs/guide/diagnostic-bundle.md)", flags: []flagDef{
			valFlag("o", "FILE", "the bundle's file (janus-support-<hostname>-<time>.tar.gz), - for stdout", argFile),
		}},
		{name: "pcap", help: "live packet capture, as a pcap file (docs/packet-capture.md)", flags: []flagDef{
			valFlag("i", "IFACE", "the interface to capture on", argInterface),
			valFlag("f", "FILTER", "a tcpdump-style filter", argNone),
			boolFlag("promisc", "promiscuous mode"),
			boolFlag("include-own-stream", "capture this capture's own connection too"),
			valFlag("snaplen", "BYTES", "bytes kept per packet", argNone),
			valFlag("duration", "DURATION", "stop after this long", argNone),
			valFlag("o", "FILE", "the pcap file, - for stdout", argFile),
		}},
		{name: "probe", args: "KIND TARGET", help: "test the network from the node: dns, tcp, tls, http, ping, traceroute (docs/guide/connectivity-probe.md)", flags: []flagDef{
			valFlag("source", "IP", "send from this address of the node (a VIP, an anycast address)", argNone),
			valFlag("type", "TYPE", "dns: A, AAAA, CNAME, SRV, TXT, MX, NS or PTR", argNone),
			valFlag("server", "HOST[:PORT]", "dns: ask this server", argNone),
			valFlag("connect-to", "HOST[:PORT]", "tls, http: connect here, the target giving SNI and Host", argNone),
			valFlag("sni", "NAME", "tls, http: the server name sent", argNone),
			valFlag("alpn", "P,P", "tls, http: protocols offered", argNone),
			valFlag("ca-file", "FILE", "tls, http: verify against this CA (PEM)", argFile),
			valFlag("method", "METHOD", "http: GET, HEAD or OPTIONS", argNone),
			valFlag("H", "'NAME: VALUE'", "http: a header (repeatable)", argNone),
			valFlag("count", "N", "ping: echo requests (4)", argNone),
			valFlag("interval", "DURATION", "ping: between them (1s)", argNone),
			valFlag("size", "BYTES", "ping, traceroute: payload (56)", argNone),
			boolFlag("df", "ping, traceroute: the DF bit - an MTU test"),
			valFlag("max-hops", "N", "traceroute: 30 by default and at most", argNone),
			valFlag("timeout", "DURATION", "each step's", argNone),
			boolFlag("v", "the HTTP headers and body, every certificate"),
		}, pos: []argKind{argProbeKind, argNone}, required: 2},
		{name: "metrics", help: "the node's Prometheus exporter: show or change", flags: []flagDef{boolFlag("enable", "turn it on"), boolFlag("disable", "turn it off"), valFlag("address", "IP|auto|*", "serve on this address only (auto: the main interface's, *: all)", argNone), valFlag("port", "PORT", "serve on this port", argNone)}},
		{name: "console-budget", help: "the services' share of the node's console (docs/guide/log-forwarding.md): show or change", flags: []flagDef{
			valFlag("rate", "BYTES", "bytes a second of the services' output the console gets (0: none of it)", argNone),
			valFlag("burst", "BYTES", "at most this much at once", argNone),
			boolFlag("default", "janusd's default again"),
		}},
		{name: "node-exporter", help: "prometheus-node-exporter: show or change", flags: []flagDef{
			boolFlag("enable", "run it"), boolFlag("disable", "stop it, and keep it stopped"),
			valFlag("address", "IP|auto|*", "listen on this address only (auto: the main interface's, *: all)", argNone),
			valFlag("port", "PORT", "listen on this port", argNone),
			valFlag("collectors", "A,B", "the collectors to run", argNone),
		}},
		{name: "state", help: "the node's state carried to its replacement (docs/guide/replacing-a-node.md)", subs: []*command{
			{name: "export", args: "FILE|-", help: "the node's configuration, certificates and keys, network: a tar to import on its replacement", flags: []flagDef{boolFlag("identity", "the node's CA and keys too: the replacement is this node to its clients")}, pos: []argKind{argFile}, required: 1},
			{name: "import", args: "FILE|- | -from NODE", help: "take another node's state - from a file, a pipe, or pulled from the node itself -, then restart into it; the node must be unused", flags: []flagDef{
				boolFlag("identity", "take the exported node's identity: its credentials work here, this node's own stop"),
				boolFlag("network", "the exported network configuration too: the node takes the other's addresses"),
				valFlag("save-network", "FILE", "without -network: save the exported network configuration, for the takeover", argFile),
				boolFlag("no-reboot", "don't reboot: the import waits for janusd's next start"),
				valFlag("from", "NODE|HOST:PORT", "pull the state from that node itself, node to node: it signs this node's request for one export", argFleetNode),
				valFlag("from-address", "HOST:PORT", "with -from: where this node reaches the other (default: where janusctl does)", argNone),
				valFlag("from-ca", "FILE", "with -from HOST:PORT: the other node's CA (default: -ca)", argFile),
				valFlag("from-cert", "FILE", "with -from HOST:PORT: an admin certificate of the other node (default: -cert)", argFile),
				valFlag("from-key", "FILE", "with -from HOST:PORT: its key (default: -key)", argFile),
			}, pos: []argKind{argFile}},
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
		{name: "reboot", help: "soft-stop HAProxy, then reboot", flags: []flagDef{boolFlag("powercycle", "a power cycle"), boolFlag("kexec", "jump into the active kernel without the firmware (seconds, not a POST)")}},
		{name: "shutdown", help: "soft-stop HAProxy, then power off"},
		{name: "restart", help: "restart janusd only - HAProxy keeps serving"},
		{name: "reset", help: "wipe the STATE partition and reboot: a new CA on the console", flags: []flagDef{boolFlag("wipe-state", "wipe the persistent STATE partition"), boolFlag("wipe-ephemeral", "wipe ephemeral state")}},
	}},
	{name: "haproxy", group: "⚖️  HAProxy", help: "HAProxy: config, runtime, certificates, files", subs: []*command{
		{name: "show-info", help: "version, uptime, connections"},
		{name: "stats", help: "raw 'show stat' CSV"},
		{name: "backends", help: "backends, their servers and states"},
		{name: "server-state", args: "BACKEND SERVER ready|drain|maint", help: "set a server ready, drain or maint - until HAProxy's next reload", pos: []argKind{argBackend, argServer, argServerState}, required: 3},
		{name: "reload", help: "reload HAProxy seamlessly: haproxy.cfg and its files read again, the runtime changes (server-state, map-set, acl-add) gone"},
		{name: "get-config", help: "the running haproxy.cfg, or a version of its history", flags: []flagDef{valFlag("version", "ID", "this version of config-history instead", argNone)}},
		{name: "apply-config", args: "FILE", help: "validate, apply and reload seamlessly", pos: []argKind{argFile}, required: 1},
		{name: "config-history", help: "the configurations applied: who, when, lines changed", flags: []flagDef{valFlag("limit", "N", "the newest N only", argNone)}},
		{name: "config-revert", args: "ID", help: "apply a version of config-history again: back to it", required: 1},
		{name: "map-list", help: "the running config's file-backed maps"},
		{name: "map-get", args: "MAP", help: "a map's entries", pos: []argKind{argMap}, required: 1},
		{name: "map-set", args: "MAP KEY VALUE", help: "set one entry", pos: []argKind{argMap, argMapKey, argNone}, required: 3},
		{name: "map-delete", args: "MAP KEY", help: "delete one entry", pos: []argKind{argMap, argMapKey}, required: 2},
		{name: "map-lookup", args: "MAP VALUE", help: "the entry a value matches, as a request's would (exit status 1: none)", pos: []argKind{argMap, argNone}, required: 2},
		{name: "acl-add", args: "ACL VALUE", help: "add a pattern to an ACL", required: 2},
		{name: "acl-delete", args: "ACL VALUE", help: "delete a pattern from an ACL", required: 2},
		{name: "acl-lookup", args: "ACL VALUE", help: "the pattern a value matches (exit status 1: none)", required: 2},
		{name: "tables", help: "the stick tables: key type, entries used, size"},
		{name: "table", args: "TABLE", help: "a stick table's entries", flags: []flagDef{
			valFlag("key", "KEY", "that entry only", argNone),
			valFlag("filter", "DATA:OP:VALUE,...", "the entries whose data match, e.g. http_req_rate:gt:100 (eq ne lt le gt ge; 4 at most)", argNone),
			valFlag("limit", "N", "entries shown: 500 by default, 5000 at most", argNone),
		}, pos: []argKind{argStickTable}, required: 1},
		{name: "table-clear", args: "TABLE", help: "remove stick table entries - a blocked client let through", flags: []flagDef{
			valFlag("key", "KEY", "that entry", argNone),
			valFlag("filter", "DATA:OP:VALUE,...", "the entries whose data match", argNone),
			boolFlag("all", "every entry"),
		}, pos: []argKind{argStickTable}, required: 1},
		{name: "sessions", help: "the streams HAProxy is processing", flags: []flagDef{
			valFlag("frontend", "NAME", "a frontend's only", argNone),
			valFlag("backend", "NAME", "a backend's only", argNone),
			valFlag("source", "ADDRESS", "a client's only, with or without its port", argNone),
			valFlag("limit", "N", "streams shown: 500 by default, 5000 at most", argNone),
		}},
		{name: "session-kill", args: "ID", help: "end a stream, its ID from sessions", required: 1},
		{name: "errors", help: "the last invalid request and response of each proxy (credentials masked)", flags: []flagDef{
			valFlag("proxy", "NAME", "a frontend's or backend's only", argNone),
			valFlag("direction", "request|response", "one direction only", argNone),
		}},
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
			{name: "path", args: "[direct | proxy URL | controller URL | ordered THUMBPRINT]", help: "how the node reaches the CA: show it, or set it", flags: acmePathFlags, pos: []argKind{argNone, argNone}, variadic: true},
			{name: "test-path", args: "[direct | proxy URL | controller URL]", help: "fetch the CA's directory the saved way, or the given one", flags: acmePathFlags, pos: []argKind{argNone, argNone}, variadic: true},
			{name: "csr", args: "NAME", help: "a new key kept on the node, and its certificate signing request - to order the certificate elsewhere", pos: []argKind{argACMEName}, required: 1},
			{name: "install", args: "NAME CHAIN-FILE", help: "install the certificate issued for the request waiting", pos: []argKind{argACMEName, argFile}, required: 2},
		}},
	}},
	{name: "network", group: "🌐 Network", help: "addresses, routes, firewall, VRRP, BGP, Consul", subs: []*command{
		{name: "status", help: "interfaces, addresses, routes, DNS, clock"},
		{name: "neighbors", help: "the neighbour table: ARP and NDP entries, their MAC and state", flags: []flagDef{valFlag("interface", "IF", "only this interface's", argInterface)}},
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
	{name: "cluster", group: "🔗 Cluster", help: "clusters of a fleet without a Controller (create, apply, check...), and a node's membership of one (-n NODE: get, status, release, drain...)", subs: []*command{
		{name: "list", help: "the clusters this fleet context orchestrates"},
		{name: "create", args: "NAME NODE...", help: "make a cluster of these nodes of the fleet context: checked, member certificates, the source's configuration as version 1 on each, its secrets pulled", flags: []flagDef{
			valFlag("source", "NODE", "the node whose configuration becomes version 1, its certificates and keys every member's", argFleetNode),
			valFlag("peers-port", "N", "HAProxy's peers port (10057)", argNone),
			valFlag("address", "NODE=IP", "the address the other members reach NODE on (default: the context's), repeatable", argNone),
			valFlag("dir", "DIR", "write version 1's files there, to edit and apply", argDir),
		}, pos: []argKind{argNone, argFleetNode}, required: 2, variadic: true},
		{name: "checkout", args: "NAME DIR", help: "write a version's files into DIR, to edit and apply", flags: []flagDef{
			valFlag("version", "N", "this version (default: the cluster's)", argNone),
		}, pos: []argKind{argNone, argDir}, required: 2},
		{name: "apply", args: "NAME DIR", help: "DIR's haproxy.cfg, files/ and templates as the cluster's next version, applied member by member", flags: []flagDef{
			valFlag("manage", "LIST", "what the cluster manages from now on: haproxy,files,vrrp,bgp,firewall (default: as before)", argNone),
			boolFlag("dry-run", "say what would change, change nothing"),
			valFlag("m", "TEXT", "what this version changes", argNone),
		}, pos: []argKind{argNone, argDir}, required: 2},
		{name: "check", args: "NAME", help: "each member as it is: version, peers, drift, certificates - renewed, the ticket ring rotated when due; exits 1 when one wants a look (cron)", pos: []argKind{argNone}, required: 1},
		{name: "add", args: "NAME NODE", help: "a node of the fleet context joins the cluster: checked, its certificate, the secrets, the cluster's version", flags: []flagDef{
			valFlag("address", "IP", "the address the other members reach it on (default: the context's)", argNone),
			valFlag("var", "KEY=VALUE", "a variable the cluster's templates use for it, repeatable", argNone),
		}, pos: []argKind{argNone, argFleetNode}, required: 2},
		{name: "remove", args: "NAME NODE", help: "a member leaves: drained first, the others told", flags: []flagDef{
			boolFlag("force", "even if it doesn't drain or answer - a node gone for good"),
		}, pos: []argKind{argNone, argFleetNode}, required: 2},
		{name: "replace", args: "NAME OLD NEW", help: "a node of the fleet context takes a member's place, the traffic moving once: it joins drained, learns the stick tables, comes into service as OLD is drained, and OLD leaves", flags: []flagDef{
			valFlag("address", "IP", "the address the other members reach NEW on (default: the context's)", argNone),
			valFlag("var", "KEY=VALUE", "a variable the cluster's templates use for NEW, repeatable (default: OLD's)", argNone),
			boolFlag("new-image", "NEW may run another image - the cluster changing image member by member"),
		}, pos: []argKind{argNone, argFleetNode, argFleetNode}, required: 3},
		{name: "upgrade", args: "NAME VERSION|URL", help: "the members updated in place to a release, one at a time: each drained, its own A/B update (reverted unless healthy), back on its other slot, its stick tables learnt, undrained; a member already on it is skipped, so running it again resumes", flags: []flagDef{
			valFlag("members", "MEMBER,...", "these members, in this order (default: every member, a VRRP cluster's MASTERs last)", argNone),
			boolFlag("kexec", "each member reboots into its new slot through kexec, not the firmware"),
			boolFlag("allow-schematic-change", "the bundle may be another image than the members' - other extensions, HAProxy branch or kernel track"),
			boolFlag("insecure-skip-signature-check", "accept UKIs not signed by a Janus release key - development bundles only"),
			valFlag("sha256", "HEX", "rootfs.squashfs's SHA-256 (default: the bundle's rootfs.squashfs.sha256)", argNone),
			valFlag("version", "VERSION", "the release a URL's bundle holds, as the nodes report it (default: read from the bundle's image)", argNone),
			valFlag("timeout", "DURATION", "how long each member has to come back on the new release (20m)", argNone),
		}, pos: []argKind{argNone, argNone}, required: 2},
		{name: "delete", args: "NAME", help: "every member leaves; the cluster and its CA are gone", flags: []flagDef{
			boolFlag("force", "even if a member doesn't answer"),
		}, pos: []argKind{argNone}, required: 1},
		{name: "vars", args: "NAME MEMBER [KEY=VALUE...]", help: "a member's template variables, set (KEY= unsets one) or shown", pos: []argKind{argNone, argFleetNode, argNone}, required: 2, variadic: true},
		{name: "certificate", args: "NAME FILE", help: "a certificate and its key for every member: uploaded to the one the secrets come from, pulled by the others", flags: []flagDef{
			valFlag("name", "NAME", "its name in HAProxy's store (default: the file's)", argNone),
			valFlag("crt-list", "PATH", "a crt-list to bind it into", argNone),
			valFlag("sni", "NAMES", "the names it's served for there, comma-separated", argNone),
		}, pos: []argKind{argNone, argFile}, required: 2},
		{name: "tickets", args: "NAME", help: "a new TLS ticket key on the member the secrets come from, pulled by the others (check does it daily)", flags: []flagDef{
			boolFlag("renew", "replace every key of the ring at once - no key it held is used or accepted any more (a member's departure does it)"),
		}, pos: []argKind{argNone}, required: 1},
		{name: "balancer", args: "NAME", help: "the hook telling a balancer in front which members take the traffic - told before a member is drained or removed, after one comes into service -, or a RouterOS router whose ECMP routes the cluster keeps itself; shown without a flag", flags: []flagDef{
			valFlag("kind", "KIND", "webhook (the default) or routeros", argNone),
			valFlag("url", "URL", "the hook: an http(s) URL the members are POSTed to; routeros: the router's own HTTPS address", argNone),
			valFlag("ca", "FILE", "the only CA its certificate may chain to (PEM)", argFile),
			valFlag("fingerprint", "SHA256", "its HTTPS certificate's SHA-256, pinned (a router's self-signed one)", argNone),
			valFlag("secret-file", "FILE", "a file holding the secret the calls are signed with (X-Janus-Signature-256)", argFile),
			valFlag("router-user", "USER", "routeros: the router's user the cluster signs in as", argNone),
			valFlag("password-file", "FILE", "routeros: a file holding that user's password", argFile),
			valFlag("prefix", "PREFIX", "routeros: a service prefix the members carry (203.0.113.10/32), repeatable", argNone),
			valFlag("distance", "N", "routeros: the routes' distance (1)", argNone),
			valFlag("routing-table", "TABLE", "routeros: the routes' routing table (main)", argNone),
			boolFlag("allow-http", "routeros: a lab's router over plain HTTP"),
			valFlag("check-gateway", "ping|none", "routeros: the router pings each member and leaves one out that doesn't answer (check-gateway=ping)", argNone),
			valFlag("read-only", "yes|no", "routeros: the members announce the prefixes over BGP - the cluster reads the router's routes (each member's role), writes none", argNone),
			boolFlag("no-secret", "calls unsigned from now on"),
			valFlag("drain-delay", "SECONDS", "how long a member serves on once the balancer was told it goes (0)", argNone),
			boolFlag("off", "no hook any more"),
			boolFlag("sync", "call it with the members now"),
		}, pos: []argKind{argNone}, required: 1},
		{name: "journal", args: "NAME", help: "what was done to the cluster, by whom", flags: []flagDef{
			valFlag("n", "N", "the newest N entries (50)", argNone),
		}, pos: []argKind{argNone}, required: 1},
		{name: "get", help: "what the node knows of its cluster, and the peers section HAProxy runs with"},
		{name: "status", help: "HAProxy's peers: connected, and in sync"},
		{name: "csr", args: "[OUT]", help: "a signing request for the node's member certificate - its key stays on the node", pos: []argKind{argFile}},
		{name: "set", args: "MEMBER...", help: "install or update the membership (MEMBER: name=address[,api=host:port])", flags: []flagDef{
			valFlag("id", "UUID", "the cluster's id", argNone),
			valFlag("name", "NAME", "the cluster's name", argNone),
			valFlag("member", "NAME", "this node's member name (default: its hostname)", argNone),
			valFlag("port", "N", "the peers port (default 10057)", argNone),
			valFlag("ca", "FILE", "the cluster's CA (PEM): the first time, or to replace it", argFile),
			valFlag("cert", "FILE", "the member certificate (PEM) for the last csr: the first time, or to renew it", argFile),
			valFlag("managed-by", "TEXT", "who sets the composition", argNone),
			valFlag("managed", "LIST", "what the cluster manages here, locked to the orchestrator: haproxy,files,vrrp,bgp,firewall", argNone),
		}, pos: []argKind{argNone}, required: 1, variadic: true},
		{name: "leave", help: "forget the membership: the node on its own again"},
		{name: "release", args: "[DOMAIN...]", help: "let this node's own users change what the cluster manages (all of it when none is named) - the cluster then sees it drifted", pos: []argKind{argNone}, variadic: true},
		{name: "drain", help: "out of service without stopping: the virtual IPs and routes move to the peers", flags: []flagDef{valFlag("timeout", "SECONDS", "how long to wait for VRRP and BGP to show it (60)", argNone)}},
		{name: "undrain", help: "back in service"},
		{name: "pull", help: "fetch another member's secrets - certificates, files, ACME certificates, ticket keys - over mTLS", flags: []flagDef{
			valFlag("from", "MEMBER", "the member to pull from", argNone),
			valFlag("items", "LIST", "runtime-certs, files, acme-certs, tls-ticket-keys (default: all)", argNone),
		}},
		{name: "rotate-tickets", help: "add a TLS ticket key to the ring, handed to HAProxy without a reload", flags: []flagDef{
			boolFlag("renew", "replace every key of the ring at once - no key it held is used or accepted any more"),
		}},
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
			valFlag("logs-size-mb", "MIB", "the LOGS partition's size (-1: none; default 5 % of the disk)", argNone),
			boolFlag("insecure-skip-signature-check", "accept unsigned UKIs - development only"),
		}, pos: []argKind{argRemote, argRemote}, required: 2},
		{name: "upgrade", args: "BUNDLE_DIR|URL", help: "write a release to the inactive slot and reboot into it", flags: []flagDef{
			valFlag("sha256", "HEX", "rootfs.squashfs's expected SHA-256", argNone),
			boolFlag("wait-for-health", "revert by itself if the new slot isn't healthy"),
			valFlag("health-timeout", "SECONDS", "how long it has to be healthy", argNone),
			boolFlag("insecure-skip-signature-check", "accept unsigned UKIs - development only"),
			boolFlag("allow-schematic-change", "accept another image schematic"),
			boolFlag("kexec", "reboot into the new slot without the firmware (seconds, not a POST)"),
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

// helpAsked: args name a command and ask for its help (-h, -help or
// --help, before a "--"): the command and its path, for its help to be
// shown instead of anything run. Without it a command that takes no
// flag ran whatever followed it - "janusctl system restart -h" restarted
// janusd, "system shutdown -h" would have powered the node off
// (v2026.10.24-4's lab).
func helpAsked(args []string) (*command, []string, bool) {
	c, rest := commands.find(args)
	if c == commands || c.hidden {
		return nil, nil, false
	}
	for _, a := range rest {
		switch a {
		case "--":
			return nil, nil, false
		case "-h", "-help", "--help":
			return c, args[:len(args)-len(rest)], true
		}
	}
	return nil, nil, false
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
