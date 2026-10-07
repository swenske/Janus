package sysctl

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
)

// The rules each parameter tries, in order: what the node observed
// before what its hardware says. Every one is a formula on named
// measurements; TestKernelTuningDoc writes them into the documentation.
// No rule for the keepalives, TCP Fast Open, ip_nonlocal_bind,
// tcp_fin_timeout and the reserved ports: they depend on the use, not on
// a measurement. Set here rather than in Catalog: a rule reads Catalog
// (Lookup), which a Catalog entry can't refer to.
func init() {
	attach := func(name string, rules ...Rule) { Lookup(name).Rules = rules }
	attach("net.core.somaxconn", ruleAcceptOverflows, ruleQueueMemory)
	attach("net.ipv4.ip_local_port_range", ruleSourcePorts)
	attach("net.ipv4.tcp_tw_reuse", ruleOutgoingRate)
	attach("net.ipv4.tcp_synack_retries", ruleSYNCookies)
	attach("net.core.netdev_max_backlog", ruleBacklogDrops)
	attach("net.ipv4.tcp_rmem", ruleTCPMemory("net.ipv4.tcp_rmem", "tcp_rmem.memory", "4096 16060 262144"))
	attach("net.ipv4.tcp_wmem", ruleTCPMemory("net.ipv4.tcp_wmem", "tcp_wmem.memory", "4096 16384 262144"))
	attach("fs.file-max", ruleOpenFiles)
	attach("net.netfilter.nf_conntrack_max", ruleConntrackFull, ruleConntrackMemory)
}

const lastWeek = "the last 7 days"

// queuedBytes is about what a connection waiting in an accept queue
// takes: its socket, and the request it already sent.
const queuedBytes = 4 * kib

var ruleAcceptOverflows = Rule{
	ID:      "somaxconn.overflows",
	Text:    "Accept queues overflowed in 3 different hours of the last 7 days or more, and one of HAProxy's listeners has net.core.somaxconn as its backlog - the kernel's limit, not one HAProxy asked for: twice the value, at most 65535.",
	Sources: []Source{srcHAProxyBacklog, srcKernelIP},
	Eval: func(m Metrics) (Recommendation, bool) {
		s, ok := m.seen("accept_overflows")
		cur, known := m.liveInt("net.core.somaxconn")
		if !ok || !known {
			return Recommendation{}, false
		}
		var capped []Listener
		for _, l := range m.Listeners {
			if int64(l.Backlog) >= cur {
				capped = append(capped, l)
			}
		}
		if len(capped) == 0 {
			return Recommendation{}, false
		}
		return Recommendation{Value: itoa(min(2*cur, 65535)), Measured: []Measurement{
			counted("accept_overflows", s),
			{Name: "HAProxy's listeners at net.core.somaxconn", Value: listenersText(capped)},
		}}, true
	},
}

var ruleQueueMemory = Rule{
	ID:      "somaxconn.memory",
	Text:    "An eighth of the node's memory for full accept queues during an overload, at about 4 KiB a waiting connection, shared by HAProxy's listeners: the power of two below, 4096 at least - only when lower than the value.",
	Sources: []Source{srcHAProxyBacklog},
	Eval: func(m Metrics) (Recommendation, bool) {
		cur, known := m.liveInt("net.core.somaxconn")
		n := int64(len(m.Listeners))
		if !known || n == 0 || m.MemTotal <= 0 {
			return Recommendation{}, false
		}
		v := max(floorPow2(m.MemTotal/8/(n*queuedBytes)), 4096)
		if v >= cur {
			return Recommendation{}, false
		}
		return Recommendation{Value: itoa(v), Measured: []Measurement{
			{Name: "Memory", Value: bytesText(m.MemTotal)},
			{Name: "HAProxy's listeners", Value: listenersText(m.Listeners)},
		}}, true
	},
}

var ruleSourcePorts = Rule{
	ID:      "port_range.usage",
	Text:    "Sockets to one destination took half the source ports or more, in 3 different hours of the last 7 days or more: the widest range the node allows - from the first multiple of 1024 above the ports it listens on (reserved ones aside) to 65023, HAProxy's upper end - when wider than the current one.",
	Sources: []Source{srcHAPEE, srcHAProxySource},
	Eval: func(m Metrics) (Recommendation, bool) {
		s, ok := m.seen("source_ports")
		cur := m.live("net.ipv4.ip_local_port_range")
		if !ok || len(cur) != 2 {
			return Recommendation{}, false
		}
		const high = 65023
		reserved := portSet(m.Live["net.ipv4.ip_local_reserved_ports"])
		highest := int64(1023)
		for _, port := range m.ListeningPorts {
			if port <= high && !reserved[port] {
				highest = max(highest, port)
			}
		}
		low := (highest/1024 + 1) * 1024
		if high-low+1 < minPorts || high-low <= cur[1]-cur[0] {
			return Recommendation{}, false
		}
		return Recommendation{Value: fmt.Sprintf("%d %d", low, high), Measured: []Measurement{peaked("source_ports", s)}}, true
	},
}

var ruleOutgoingRate = Rule{
	ID:      "tw_reuse.rate",
	Text:    "The node opened 300 connections a second or more, in 3 different hours of the last 7 days or more: TIME_WAIT sockets reused for new outgoing connections (1) - needed above a few hundred a second.",
	Sources: []Source{srcHAPEE},
	Eval: func(m Metrics) (Recommendation, bool) {
		s, ok := m.seen("outgoing_rate")
		if !ok || m.Live["net.ipv4.tcp_tw_reuse"] == "" {
			return Recommendation{}, false
		}
		return Recommendation{Value: "1", Measured: []Measurement{peaked("outgoing_rate", s)}}, true
	},
}

var ruleSYNCookies = Rule{
	ID:      "synack_retries.cookies",
	Text:    "SYN cookies went out in 3 different hours of the last 7 days or more - SYN queues full, from floods or bursts: 2, so a connection that never completes leaves the queue after about 7 seconds (15 with 3) - only lowered.",
	Sources: []Source{srcHAProxySYN, srcKernelIP},
	Eval: func(m Metrics) (Recommendation, bool) {
		s, ok := m.seen("syn_cookies")
		cur, known := m.liveInt("net.ipv4.tcp_synack_retries")
		if !ok || !known || cur <= 2 {
			return Recommendation{}, false
		}
		return Recommendation{Value: "2", Measured: []Measurement{counted("syn_cookies", s)}}, true
	},
}

var ruleBacklogDrops = Rule{
	ID:      "netdev_max_backlog.drops",
	Text:    "CPUs' input queues dropped packets in 3 different hours of the last 7 days or more: twice the value, at most 65536.",
	Sources: []Source{srcKernelIP, srcHAPEE},
	Eval: func(m Metrics) (Recommendation, bool) {
		s, ok := m.seen("backlog_drops")
		cur, known := m.liveInt("net.core.netdev_max_backlog")
		if !ok || !known {
			return Recommendation{}, false
		}
		return Recommendation{Value: itoa(min(2*cur, 65536)), Measured: []Measurement{counted("backlog_drops", s)}}, true
	},
}

// ruleTCPMemory suggests HAProxy Enterprise's smaller buffers, value,
// for the parameter name.
func ruleTCPMemory(name, id, value string) Rule {
	return Rule{
		ID:      id,
		Text:    "TCP's memory came under pressure in 3 different hours of the last 7 days or more: HAProxy's smaller buffers for many connections, " + value + " - only when the maximum is higher.",
		Sources: []Source{srcHAPEE},
		Eval: func(m Metrics) (Recommendation, bool) {
			s, ok := m.seen("tcp_memory")
			cur := m.live(name)
			if !ok || len(cur) != 3 || cur[2] <= 262144 {
				return Recommendation{}, false
			}
			return Recommendation{Value: value, Measured: []Measurement{counted("tcp_memory", s)}}, true
		},
	}
}

var ruleOpenFiles = Rule{
	ID:      "file_max.usage",
	Text:    "Open files reached 80% of fs.file-max in 3 different hours of the last 7 days or more: twice the highest count, rounded up to a multiple of 1024.",
	Sources: []Source{srcHAProxyFDs},
	Eval: func(m Metrics) (Recommendation, bool) {
		s, ok := m.seen("open_files")
		cur, known := m.liveInt("fs.file-max")
		if !ok || !known {
			return Recommendation{}, false
		}
		v := min(roundUp(2*s.PeakAbs, 1024), 64*mib)
		if v <= cur {
			return Recommendation{}, false
		}
		return Recommendation{Value: itoa(v), Measured: []Measurement{peaked("open_files", s)}}, true
	},
}

var ruleConntrackFull = Rule{
	ID:      "conntrack_max.usage",
	Text:    "The table reached 75% - or dropped connections for want of room - in 3 different hours of the last 7 days or more: twice the highest count, rounded up to a multiple of 1024, no less than the memory rule below gives, at most an eighth of the memory at 320 bytes an entry.",
	Sources: []Source{srcKernelConntrack},
	Eval: func(m Metrics) (Recommendation, bool) {
		usage, full := m.seen("conntrack_usage")
		drops, dropped := m.seen("conntrack_drops")
		cur, known := m.liveInt("net.netfilter.nf_conntrack_max")
		if !full && !dropped || !known || m.MemTotal <= 0 {
			return Recommendation{}, false
		}
		peak := usage.PeakAbs
		if dropped {
			peak = max(peak, cur) // full, at its size
		}
		v := min(max(roundUp(2*peak, 1024), conntrackForMemory(m.MemTotal)), m.MemTotal/8/conntrackEntryBytes/1024*1024, 4*mib)
		if v <= cur {
			return Recommendation{}, false
		}
		var measured []Measurement
		if full {
			measured = append(measured, peaked("conntrack_usage", usage))
		}
		if dropped {
			measured = append(measured, counted("conntrack_drops", drops))
		}
		return Recommendation{Value: itoa(v), Measured: append(measured, Measurement{Name: "Memory", Value: bytesText(m.MemTotal)})}, true
	},
}

var ruleConntrackMemory = Rule{
	ID:      "conntrack_max.memory",
	Text:    "Connections tracked (the nftables extension's stateful rules): two entries per HAProxy connection, closed ones kept 2 minutes - a thirty-second of the memory at 320 bytes an entry, rounded down to a multiple of 1024, at most 262144 (the kernel's own value above 4 GiB) - only when higher than the value.",
	Sources: []Source{srcKernelConntrack},
	Eval: func(m Metrics) (Recommendation, bool) {
		cur, known := m.liveInt("net.netfilter.nf_conntrack_max")
		if !m.Conntrack || !known || m.MemTotal <= 0 {
			return Recommendation{}, false
		}
		v := conntrackForMemory(m.MemTotal)
		if v <= cur {
			return Recommendation{}, false
		}
		return Recommendation{Value: itoa(v), Measured: []Measurement{{Name: "Memory", Value: bytesText(m.MemTotal)}}}, true
	},
}

// conntrackForMemory is ruleConntrackMemory's value: a thirty-second of
// mem at 320 bytes an entry, by 1024, at most 262144.
func conntrackForMemory(mem int64) int64 {
	return min(mem/32/conntrackEntryBytes/1024*1024, 262144)
}

// seen is signal id when it was seen in MinHours different hours of
// Window.
func (m Metrics) seen(id string) (SignalState, bool) {
	s, ok := m.Signals[id]
	return s, ok && s.Hours >= MinHours
}

func (m Metrics) live(name string) []int64 { return ints(m.Live[name]) }

func (m Metrics) liveInt(name string) (int64, bool) {
	v := m.live(name)
	if len(v) != 1 {
		return 0, false
	}
	return v[0], true
}

// counted is a Counter signal as a measurement.
func counted(id string, s SignalState) Measurement {
	return Measurement{Name: LookupSignal(id).Title, Value: fmt.Sprintf("%s, in %d different hours", countText(int64(s.Total)), s.Hours), Window: lastWeek}
}

// peaked is a Gauge signal as a measurement.
func peaked(id string, s SignalState) Measurement {
	return Measurement{Name: LookupSignal(id).Title, Value: GaugeText(id, s) + fmt.Sprintf(" at the highest; %s in %d different hours", ThresholdText(id), s.Hours), Window: lastWeek}
}

// PeakText is a signal's highest over the window, in words - "" when
// it has none yet.
func PeakText(id string, s SignalState) string {
	if s.Peak <= 0 && s.PeakAbs <= 0 {
		return ""
	}
	if LookupSignal(id).Kind == Counter {
		return countText(int64(s.Peak)) + " in its busiest hour"
	}
	return GaugeText(id, s)
}

// GaugeText is a Gauge signal's peak, in words.
func GaugeText(id string, s SignalState) string {
	def := LookupSignal(id)
	var b strings.Builder
	if def.Ratio {
		fmt.Fprintf(&b, "%.0f%%", 100*s.Peak)
	} else {
		fmt.Fprintf(&b, "%.0f%s", s.Peak, gaugeUnit(id))
	}
	if s.PeakAbs > 0 {
		fmt.Fprintf(&b, " (%s%s)", countText(s.PeakAbs), map[string]string{"source_ports": " sockets", "open_files": " files", "conntrack_usage": " entries"}[id])
	}
	if s.Detail != "" {
		fmt.Fprintf(&b, " to %s", s.Detail)
	}
	return b.String()
}

// ThresholdText is when a Gauge's hour counts: "from 80%".
func ThresholdText(id string) string {
	def := LookupSignal(id)
	if def.Ratio {
		return fmt.Sprintf("from %.0f%%", 100*def.Threshold)
	}
	return fmt.Sprintf("from %.0f%s", def.Threshold, gaugeUnit(id))
}

func gaugeUnit(id string) string {
	if id == "outgoing_rate" {
		return "/s"
	}
	return ""
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// floorPow2 is the highest power of two up to v, 0 below 1.
func floorPow2(v int64) int64 {
	if v < 1 {
		return 0
	}
	return 1 << (63 - bits.LeadingZeros64(uint64(v)))
}

func roundUp(v, step int64) int64 { return (v + step - 1) / step * step }

// countText writes a count with its thousands separated: 262,144.
func countText(v int64) string {
	s := strconv.FormatInt(v, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// bytesText is a size in MiB or GiB.
func bytesText(b int64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%d MiB", b/mib)
}

func listenersText(ls []Listener) string {
	var parts []string
	for i, l := range ls {
		if i == 4 {
			parts = append(parts, fmt.Sprintf("%d more", len(ls)-i))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (backlog %d)", l.Addr, l.Backlog))
	}
	return fmt.Sprintf("%d: %s", len(ls), strings.Join(parts, ", "))
}
