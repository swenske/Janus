package sysctl

import "time"

// What a suggestion rests on beyond the machine itself: signals the node
// observes (internal/sysctl/observe) - kernel counters summed by hour,
// gauges maxed by hour -, kept Keep on STATE. A rule acts on a signal
// seen in at least MinHours different hours of the last Window: a
// saturation that comes back, never one peak.
const (
	Window   = 7 * 24 * time.Hour
	Keep     = 14 * 24 * time.Hour
	MinHours = 3
)

// SignalKind is how a signal's hourly value is made.
type SignalKind int

const (
	// Counter: a kernel counter's increase in the hour; the hour counts
	// when it increased.
	Counter SignalKind = iota + 1
	// Gauge: the highest value in the hour; the hour counts from
	// Threshold.
	Gauge
)

// SignalDef is a signal the node observes.
type SignalDef struct {
	ID        string
	Title     string
	Measure   string // what's read, and where
	Seen      string // when an hour counts
	Kind      SignalKind
	Threshold float64 // Gauge
	Ratio     bool    // Gauge: a fraction, shown as a percentage
}

// Signals is every signal, in the order the node shows them.
var Signals = []SignalDef{
	{
		ID: "accept_overflows", Title: "Accept queue overflows", Kind: Counter,
		Measure: "TcpExt ListenOverflows (/proc/net/netstat): connections dropped because a listening socket's accept queue was full.",
		Seen:    "It increased.",
	},
	{
		ID: "syn_cookies", Title: "SYN cookies sent", Kind: Counter,
		Measure: "TcpExt SyncookiesSent (/proc/net/netstat): a listening socket's SYN queue was full - a flood, or a burst bigger than the backlog.",
		Seen:    "It increased.",
	},
	{
		ID: "outgoing_rate", Title: "Outgoing connections a second", Kind: Gauge, Threshold: 300,
		Measure: "Tcp ActiveOpens (/proc/net/snmp) over each minute: the connections the node opens - HAProxy's to its servers, mostly.",
		Seen:    "300 or more a second, over a minute.",
	},
	{
		ID: "source_ports", Title: "Source ports to one server", Kind: Gauge, Threshold: 0.5, Ratio: true,
		Measure: "Every minute, the sockets to each destination address and port whose source port is in net.ipv4.ip_local_port_range - TIME_WAIT ones included - over the ports the range offers (reserved ones left out).",
		Seen:    "Half the ports or more, to one destination.",
	},
	{
		ID: "open_files", Title: "Open files", Kind: Gauge, Threshold: 0.8, Ratio: true,
		Measure: "fs.file-nr's allocated files over fs.file-max.",
		Seen:    "80% or more.",
	},
	{
		ID: "conntrack_usage", Title: "Connection tracking table", Kind: Gauge, Threshold: 0.75, Ratio: true,
		Measure: "net.netfilter.nf_conntrack_count over net.netfilter.nf_conntrack_max.",
		Seen:    "75% or more.",
	},
	{
		ID: "conntrack_drops", Title: "Connection tracking drops", Kind: Counter,
		Measure: "drop, early_drop and insert_failed (/proc/net/stat/nf_conntrack): packets dropped, entries evicted or refused because the table was full.",
		Seen:    "They increased.",
	},
	{
		ID: "backlog_drops", Title: "Input backlog drops", Kind: Counter,
		Measure: "softnet_stat's dropped column (/proc/net/softnet_stat), every CPU: packets dropped because a CPU's input queue was full.",
		Seen:    "It increased.",
	},
	{
		ID: "tcp_memory", Title: "TCP memory pressure", Kind: Counter,
		Measure: "TcpExt TCPMemoryPressures and TCPAbortOnMemory (/proc/net/netstat): TCP's memory reached net.ipv4.tcp_mem's pressure mark, or connections were reset for lack of it.",
		Seen:    "They increased.",
	},
}

// LookupSignal is the signal id, nil when there's none.
func LookupSignal(id string) *SignalDef {
	for i := range Signals {
		if Signals[i].ID == id {
			return &Signals[i]
		}
	}
	return nil
}
