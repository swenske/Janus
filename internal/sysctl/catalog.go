// Package sysctl is the node's kernel tuning (docs/guide/kernel-tuning.md):
// the CIS baseline rootfs/init enforces at every boot, and the whitelist
// of parameters HAProxy depends on that an operator may change - tested on
// trial, saved once confirmed, reset to Janus's defaults.
//
// Everything is checked against a whitelist, never a blacklist: a
// parameter Catalog doesn't mark Editable can't be written through this
// package, and no Editable parameter is one the CIS benchmark covers (a
// test proves it, and that no allowed value of any of them breaks a CIS
// control). SELinux enforces the same list underneath: only the files of
// the Editable parameters are labelled sysctl_tunable_t, the only /proc/sys
// files janusd may write.
package sysctl

import "strings"

// Class is what an operator may do with a parameter.
type Class int

const (
	// Editable parameters are the whitelist: changed on trial, saved
	// once confirmed.
	Editable Class = iota + 1
	// ReadOnly parameters are shown for diagnosis, never changed.
	ReadOnly
	// Forbidden parameters are never changed nor offered, for Janus's
	// own security - the CIS benchmark's are in CISControls.
	Forbidden
)

// Kind is the shape of a parameter's value.
type Kind int

const (
	KindInt    Kind = iota + 1 // one integer
	KindEnum                   // one integer among Allowed
	KindPair                   // two integers, "low high"
	KindTriple                 // three integers, "min default max"
	KindPorts                  // ports and ranges, "8080,9100-9110"
	KindText                   // read-only text (a list of names)
)

// Applies is when a change reaches what it governs.
type Applies int

const (
	Immediately    Applies = iota + 1
	NewConnections         // connections opened after the change
	HAProxyReload          // HAProxy reads it when it opens its listeners
)

// Bound is the range one component of a value may take.
type Bound struct{ Min, Max int64 }

// Source is a document a parameter's description comes from.
type Source struct{ Title, URL string }

// Param is one parameter of Catalog.
type Param struct {
	Name  string // as sysctl(8) names it: "net.core.somaxconn"
	Class Class
	Kind  Kind
	Unit  string

	// Default is Janus's value, the one the node gets at every boot and
	// a reset puts back. Dynamic: Janus keeps the kernel's own, which
	// depends on the machine's memory - read at boot before anything
	// changes it (BootDefaults).
	Default string
	Dynamic bool

	Bounds   []Bound // one per component
	Allowed  []int64 // KindEnum
	MaxItems int     // KindPorts
	Applies  Applies

	// Requires names what the parameter only matters with ("nftables":
	// the firewall extension's conntrack rules).
	Requires string

	Summary string // what it is
	Effect  string // what it does to HAProxy
	Risk    string // what can go wrong
	Why     string // ReadOnly and Forbidden: why it can't change

	KernelDefault string // the kernel's own default, for the docs
	HAProxyValue  string // HAProxy's recommendation, "" when it gives none
	Sources       []Source

	// check validates a value beyond its bounds, given the values every
	// Editable parameter would have once the change is made.
	check func(v []int64, env *env) error
	// warn reports a problem with the live value.
	warn func(live string, env *env) []string

	// Rules suggest a value for this machine (phase 2 - none yet).
	Rules []Rule
}

// Path is the parameter's file under root (/proc/sys).
func (p *Param) Path(root string) string {
	return root + "/" + strings.ReplaceAll(p.Name, ".", "/")
}

// The documents the descriptions come from. HAProxy's are the default
// branch's (variants.mk); these sections are the same on every 3.x.
var (
	srcHAProxyBacklog   = Source{"HAProxy configuration: backlog", "https://docs.haproxy.org/3.4/configuration.html#4.2-backlog"}
	srcHAProxySource    = Source{"HAProxy configuration: source", "https://docs.haproxy.org/3.4/configuration.html#4.2-source"}
	srcHAProxyTFO       = Source{"HAProxy configuration: tfo", "https://docs.haproxy.org/3.4/configuration.html#5.1-tfo"}
	srcHAProxyCC        = Source{"HAProxy configuration: cc", "https://docs.haproxy.org/3.4/configuration.html#5.1-cc"}
	srcHAProxyTCPKA     = Source{"HAProxy configuration: clitcpka-idle", "https://docs.haproxy.org/3.4/configuration.html#4.2-clitcpka-idle"}
	srcHAProxyTCPKAIntv = Source{"HAProxy configuration: clitcpka-intvl", "https://docs.haproxy.org/3.4/configuration.html#4.2-clitcpka-intvl"}
	srcHAProxyTCPKACnt  = Source{"HAProxy configuration: clitcpka-cnt", "https://docs.haproxy.org/3.4/configuration.html#4.2-clitcpka-cnt"}
	srcHAProxyRcvbuf    = Source{"HAProxy configuration: tune.rcvbuf.client", "https://docs.haproxy.org/3.4/configuration.html#3.2-tune.rcvbuf.client"}
	srcHAProxySndbuf    = Source{"HAProxy configuration: tune.sndbuf.client", "https://docs.haproxy.org/3.4/configuration.html#3.2-tune.sndbuf.client"}
	srcHAProxyFDLimit   = Source{"HAProxy configuration: fd-hard-limit", "https://docs.haproxy.org/3.4/configuration.html#3.1-fd-hard-limit"}
	srcHAProxyDumpable  = Source{"HAProxy configuration: set-dumpable", "https://docs.haproxy.org/3.4/configuration.html#3.1-set-dumpable"}
	srcHAProxyFDs       = Source{"HAProxy management: file-descriptor limitations", "https://docs.haproxy.org/3.4/management.html#5"}
	srcHAProxyTraps     = Source{"HAProxy management: well-known traps to avoid", "https://docs.haproxy.org/3.4/management.html#11"}
	srcHAProxySYN       = Source{"HAProxy: doc/linux-syn-cookies.txt", "https://github.com/haproxy/haproxy/blob/master/doc/linux-syn-cookies.txt"}
	srcHAPEE            = Source{"HAProxy Enterprise: tune the operating system", "https://www.haproxy.com/documentation/haproxy-enterprise/administration/performance-tuning/#tune-the-operating-system"}
	srcKernelIP         = Source{"Linux: IP sysctl", "https://docs.kernel.org/networking/ip-sysctl.html"}
	srcKernelConntrack  = Source{"Linux: netfilter conntrack sysctl", "https://docs.kernel.org/networking/nf_conntrack-sysctl.html"}
)

const (
	kib = 1 << 10
	mib = 1 << 20
)

// keepaliveEffect is the three keepalive parameters' shared effect.
const keepaliveEffect = "HAProxy uses it for option tcpka, clitcpka and srvtcpka when clitcpka-idle, -intvl and -cnt (srvtcpka-*) aren't set."

// Catalog is every parameter the node shows, in the order it shows them:
// the Editable whitelist, then the ReadOnly and Forbidden ones. The CIS
// benchmark's are CISControls.
var Catalog = []*Param{
	{
		Name: "net.core.somaxconn", Class: Editable, Kind: KindInt, Unit: "connections",
		Default: "60000", Bounds: []Bound{{4096, 65535}}, Applies: HAProxyReload,
		Summary:       "Upper limit of every listening socket's accept queue - the backlog listen() asks for.",
		Effect:        "HAProxy asks for a backlog equal to the frontend's maxconn (or its backlog setting) and the kernel caps it here. SYN cookies being always on (CIS 3.3.1.18), it also bounds the SYN queue before cookies take over. HAProxy reads it when it opens its listeners: a change reaches it at its next reload.",
		Risk:          "A full queue holds connections HAProxy hasn't accepted yet, per listener: memory, on a small node. A very high limit also hides saturation - clients wait instead of failing fast.",
		KernelDefault: "4096", HAProxyValue: "60000",
		Sources: []Source{srcHAProxyBacklog, srcHAPEE, srcHAProxySYN},
	},
	{
		Name: "net.ipv4.ip_local_port_range", Class: Editable, Kind: KindPair, Unit: "ports",
		Default: "10240 65023", Bounds: []Bound{{1024, 32768}, {1024 + minPorts - 1, 65535}}, Applies: NewConnections,
		Summary:       "Source ports of outgoing connections: HAProxy's connections to its servers.",
		Effect:        "Bounds the connections (TIME_WAIT ones included) HAProxy can have at once to one server address and port; when they run out, it fails with \"Out of local source ports on the system\". Janus starts at 10240, above its own listeners (janusd 9505, the Janus exporter 10056), as HAProxy's guidance asks.",
		Risk:          "An outgoing connection can take a listening port inside the range, and a reload binding that port then fails: a change is refused while a port the node listens on is inside the range without being reserved (net.ipv4.ip_local_reserved_ports).",
		KernelDefault: "32768 60999", HAProxyValue: "1024 65023",
		Sources: []Source{srcHAPEE, srcHAProxySource},
		check:   checkPortRange,
		warn:    warnPortRange,
	},
	{
		Name: "net.ipv4.ip_local_reserved_ports", Class: Editable, Kind: KindPorts, Unit: "ports",
		Default: "", Bounds: []Bound{{1024, 65535}}, MaxItems: 32, Applies: NewConnections,
		Summary:       "Ports never handed out as source ports.",
		Effect:        "Keeps a port HAProxy - or another service - listens on, inside the source port range, from being taken by an outgoing connection.",
		Risk:          "Each reserved port is one source port less.",
		KernelDefault: "(none)",
		Sources:       []Source{srcKernelIP},
	},
	{
		Name: "net.ipv4.tcp_tw_reuse", Class: Editable, Kind: KindEnum,
		Default: "1", Allowed: []int64{0, 1, 2}, Applies: NewConnections,
		Summary:       "Reuse of a TIME_WAIT socket for a new outgoing connection when TCP timestamps make it safe: 0 off, 1 on, 2 loopback only.",
		Effect:        "Frees the source ports of HAProxy's connections to its servers - needed above a few hundred connections per second.",
		Risk:          "Low: a server that sends no TCP timestamps just gets no reuse.",
		KernelDefault: "2", HAProxyValue: "1",
		Sources: []Source{srcHAPEE},
	},
	{
		Name: "net.ipv4.tcp_fin_timeout", Class: Editable, Kind: KindInt, Unit: "seconds",
		Default: "30", Bounds: []Bound{{30, 120}}, Applies: Immediately,
		Summary:       "How long an orphaned connection stays in FIN_WAIT_2.",
		Effect:        "HAProxy closes connections itself: a shorter timeout releases dead ones sooner.",
		Risk:          "HAProxy warns of problems below 25 to 30 seconds - hence 30 at least.",
		KernelDefault: "60", HAProxyValue: "30",
		Sources: []Source{srcHAPEE},
	},
	{
		Name: "net.ipv4.tcp_synack_retries", Class: Editable, Kind: KindInt, Unit: "retries",
		Default: "3", Bounds: []Bound{{1, 5}}, Applies: Immediately,
		Summary:       "SYN-ACK retransmissions for an incoming connection attempt.",
		Effect:        "The amplification factor of a SYN flood: with 3, a half-open connection is dropped after about 15 seconds instead of 63.",
		Risk:          "Too low and clients on lossy networks can't connect - 1 only during an attack.",
		KernelDefault: "5", HAProxyValue: "3",
		Sources: []Source{srcHAPEE},
	},
	{
		Name: "net.ipv4.ip_nonlocal_bind", Class: Editable, Kind: KindEnum,
		Default: "1", Allowed: []int64{0, 1}, Applies: HAProxyReload,
		Summary:       "Binding to an IPv4 address the node doesn't have (yet).",
		Effect:        "HAProxy starts and reloads with a bind on a virtual IP (VRRP) on every node of the group, not only the one holding it - HAProxy's advice is to always leave it on.",
		Risk:          "A mistyped bind address no longer fails HAProxy's start, where Janus would put the previous configuration back.",
		KernelDefault: "0", HAProxyValue: "1",
		Sources: []Source{srcHAProxyTraps, srcHAPEE},
	},
	{
		Name: "net.ipv6.ip_nonlocal_bind", Class: Editable, Kind: KindEnum,
		Default: "1", Allowed: []int64{0, 1}, Applies: HAProxyReload,
		Summary:       "Binding to an IPv6 address the node doesn't have (yet).",
		Effect:        "As for IPv4: a bind on an IPv6 virtual IP works on every node of the group.",
		Risk:          "A mistyped bind address no longer fails HAProxy's start.",
		KernelDefault: "0", HAProxyValue: "1",
		Sources: []Source{srcHAProxyTraps, srcHAPEE},
	},
	{
		Name: "net.core.netdev_max_backlog", Class: Editable, Kind: KindInt, Unit: "packets",
		Default: "10000", Bounds: []Bound{{1000, 65536}}, Applies: Immediately,
		Summary:       "Received packets waiting for the network stack, per CPU.",
		Effect:        "Absorbs bursts of traffic - an effect HAProxy calls minimal.",
		Risk:          "Memory and latency while the queue is full.",
		KernelDefault: "1000", HAProxyValue: "10000",
		Sources: []Source{srcHAPEE},
	},
	{
		Name: "net.ipv4.tcp_rmem", Class: Editable, Kind: KindTriple, Unit: "bytes",
		Dynamic: true, Bounds: []Bound{{4 * kib, 64 * kib}, {4 * kib, 4 * mib}, {64 * kib, 64 * mib}}, Applies: NewConnections,
		Summary:       "Receive buffer of each TCP socket: minimum, default and maximum, auto-tuned in between.",
		Effect:        "Kernel memory per connection: smaller buffers let a node hold more connections at once.",
		Risk:          "A low maximum caps a connection's throughput at about the maximum divided by the round-trip time - 256 KiB at 50 ms is about 40 Mbit/s.",
		KernelDefault: "4096 131072, maximum from the memory (32 MiB at most)", HAProxyValue: "4096 16060 262144 (optional)",
		Sources: []Source{srcHAPEE, srcHAProxyRcvbuf},
		check:   checkBuffers,
	},
	{
		Name: "net.ipv4.tcp_wmem", Class: Editable, Kind: KindTriple, Unit: "bytes",
		Dynamic: true, Bounds: []Bound{{4 * kib, 64 * kib}, {4 * kib, 4 * mib}, {64 * kib, 64 * mib}}, Applies: NewConnections,
		Summary:       "Send buffer of each TCP socket: minimum, default and maximum, auto-tuned in between.",
		Effect:        "Kernel memory per connection: smaller buffers let a node hold more connections at once.",
		Risk:          "A low maximum caps a connection's throughput at about the maximum divided by the round-trip time.",
		KernelDefault: "4096 16384, maximum from the memory (4 MiB at most)", HAProxyValue: "4096 16384 262144 (optional)",
		Sources: []Source{srcHAPEE, srcHAProxySndbuf},
		check:   checkBuffers,
	},
	{
		Name: "fs.file-max", Class: Editable, Kind: KindInt, Unit: "files",
		Dynamic: true, Bounds: []Bound{{8192, 64 * mib}}, Applies: Immediately,
		Summary:       "Open files on the whole system.",
		Effect:        "HAProxy runs as uid 1000 without CAP_SYS_ADMIN, so it's held to it: at the limit, accept() and socket() fail with ENFILE (\"Too many sockets on the system\").",
		Risk:          "More descriptors, more memory they can take.",
		KernelDefault: "about 10% of the memory, at 1 KiB a file",
		Sources:       []Source{srcHAProxyFDs},
		check:         checkFileMax,
	},
	{
		Name: "net.ipv4.tcp_keepalive_time", Class: Editable, Kind: KindInt, Unit: "seconds",
		Default: "7200", Bounds: []Bound{{60, 7200}}, Applies: Immediately,
		Summary:       "Idle time before TCP probes a connection that has keepalive on.",
		Effect:        keepaliveEffect,
		Risk:          "Aggressive values add traffic and drop connections over unstable links.",
		KernelDefault: "7200",
		Sources:       []Source{srcHAProxyTCPKA},
	},
	{
		Name: "net.ipv4.tcp_keepalive_intvl", Class: Editable, Kind: KindInt, Unit: "seconds",
		Default: "75", Bounds: []Bound{{10, 75}}, Applies: Immediately,
		Summary:       "Time between two keepalive probes.",
		Effect:        keepaliveEffect,
		Risk:          "Aggressive values add traffic and drop connections over unstable links.",
		KernelDefault: "75",
		Sources:       []Source{srcHAProxyTCPKAIntv},
	},
	{
		Name: "net.ipv4.tcp_keepalive_probes", Class: Editable, Kind: KindInt, Unit: "probes",
		Default: "9", Bounds: []Bound{{3, 20}}, Applies: Immediately,
		Summary:       "Unanswered keepalive probes before the connection is dropped.",
		Effect:        keepaliveEffect,
		Risk:          "Few probes drop connections over unstable links.",
		KernelDefault: "9",
		Sources:       []Source{srcHAProxyTCPKACnt},
	},
	{
		Name: "net.ipv4.tcp_fastopen", Class: Editable, Kind: KindEnum,
		Default: "1", Allowed: []int64{0, 1, 2, 3}, Applies: Immediately,
		Summary:       "TCP Fast Open: 1 as a client, 2 as a server, 3 both.",
		Effect:        "A bind line's tfo only works with the server bit (2): data in the SYN saves a round trip on repeat connections. The flags without a cookie or for every listener (0x4, 0x200, 0x400) aren't offered.",
		Risk:          "Some middleboxes drop SYNs that carry data, and that data can be replayed - HAProxy advises enabling it only once well tested.",
		KernelDefault: "1",
		Sources:       []Source{srcHAProxyTFO, srcKernelIP},
	},
	{
		Name: "net.netfilter.nf_conntrack_max", Class: Editable, Kind: KindInt, Unit: "entries",
		Dynamic: true, Bounds: []Bound{{16384, 4 * mib}}, Applies: Immediately, Requires: "nftables",
		Summary:       "Size of the connection-tracking table the firewall's stateful (ct) rules use.",
		Effect:        "With the nftables extension and stateful rules, each of HAProxy's client and server connections takes an entry: a full table drops the packets of new connections.",
		Risk:          "About 320 bytes of memory an entry. Without conntrack rules it has no effect.",
		KernelDefault: "from the memory: 1 per 128 KiB up to 1 GiB, then 65536, 262144 above 4 GiB",
		Sources:       []Source{srcKernelConntrack},
		check:         checkConntrack,
	},

	{
		Name: "net.ipv4.tcp_max_syn_backlog", Class: ReadOnly, Kind: KindInt,
		Summary:       "Remembered connection requests that haven't completed their handshake, per listener.",
		Why:           "No effect on Janus: SYN cookies are always on (CIS 3.3.1.18) and the kernel only reads it with them off - the SYN queue is bounded by the backlog, so net.core.somaxconn is the one to tune.",
		KernelDefault: "from the memory", HAProxyValue: "60000",
		Sources: []Source{srcHAPEE, srcHAProxySYN},
	},
	{
		Name: "fs.nr_open", Class: ReadOnly, Kind: KindInt,
		Summary:       "Highest open-files limit a process may be given.",
		Why:           "No effect: Janus's init gives janusd and HAProxy a limit of 524288, below it - HAProxy derives its maxconn from that limit.",
		KernelDefault: "1048576",
		Sources:       []Source{srcHAProxyFDs, srcHAProxyFDLimit},
	},
	{
		Name: "net.ipv4.tcp_congestion_control", Class: ReadOnly, Kind: KindText,
		Summary:       "TCP congestion control algorithm of new connections.",
		Why:           "Only cubic is built into Janus's kernel - no BBR.",
		KernelDefault: "cubic",
		Sources:       []Source{srcHAProxyCC},
	},
	{
		Name: "net.ipv4.tcp_available_congestion_control", Class: ReadOnly, Kind: KindText,
		Summary:       "The algorithms a bind or server line's cc may name.",
		Why:           "The kernel's list - what Janus builds in.",
		KernelDefault: "reno cubic",
		Sources:       []Source{srcHAProxyCC},
	},

	{
		Name: "kernel.core_pattern", Class: Forbidden, Kind: KindText,
		Summary: "Where the kernel writes a crashed process's core dump.",
		Why:     "A pattern starting with | would run a program as root on every crash: never changed on Janus.",
		Sources: []Source{srcHAProxyDumpable},
	},
}

// minPorts is the fewest source ports ip_local_port_range may leave.
const minPorts = 4096

var byName = func() map[string]*Param {
	m := map[string]*Param{}
	for _, p := range Catalog {
		m[p.Name] = p
	}
	return m
}()

// Lookup is the parameter named name, or nil.
func Lookup(name string) *Param { return byName[name] }

// EditableParams is Catalog's whitelist, in order.
func EditableParams() []*Param {
	var out []*Param
	for _, p := range Catalog {
		if p.Class == Editable {
			out = append(out, p)
		}
	}
	return out
}
