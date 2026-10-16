// Package logship sends a node's logs - and the Controller's - to log
// servers: syslog (RFC 5424 or 3164, over UDP, TCP or TLS) and
// Elasticsearch. One shipper for everything, so every source gets the
// same targets, trust, metadata and counters: janusd's log, HAProxy's
// own messages and its access logs, the extensions' output, the
// kernel's messages, the event log and the audit.
//
// Where a source is also kept on disk - the node's LOGS partition
// (internal/logvol), the event log, the audit journal on STATE - each
// target reads it behind a cursor saved as it goes: what a target
// couldn't take while it was down, or the node rebooted meanwhile, goes
// once it's back ("at least once"). What's only in memory (HAProxy's
// access logs by default, everything on a node without a LOGS partition)
// waits in a bounded queue per target, the oldest dropped and counted
// when it's full. Nothing ever waits for a target: a line printed is
// queued or dropped, never held.
package logship

import (
	"regexp"
	"strings"
	"time"
)

// Sources: what a target picks from.
const (
	SourceJanusd     = "janusd"     // janusd's own log
	SourceHAProxy    = "haproxy"    // HAProxy's own messages ([NOTICE], [WARNING], [ALERT])
	SourceAccess     = "access"     // HAProxy's access logs (log stdout)
	SourceExtensions = "extensions" // the extensions' services, letsencrypt
	SourceKernel     = "kernel"     // the kernel's messages
	SourceEvents     = "events"     // the event log (internal/events)
	SourceAudit      = "audit"      // every change made through the API, and every refusal
	SourceController = "controller" // the Controller's own log (on the Controller)
)

// Sources is every source a node has, in a stable order.
var Sources = []string{SourceJanusd, SourceHAProxy, SourceAccess, SourceExtensions, SourceKernel, SourceEvents, SourceAudit}

// DefaultSources is what a target takes when it names none: all but
// HAProxy's access logs - thousands a second under load, asked for
// explicitly.
var DefaultSources = []string{SourceJanusd, SourceHAProxy, SourceExtensions, SourceKernel, SourceEvents, SourceAudit}

// Syslog severities.
const (
	SevEmergency = 0
	SevAlert     = 1
	SevCritical  = 2
	SevError     = 3
	SevWarning   = 4
	SevNotice    = 5
	SevInfo      = 6
	SevDebug     = 7
)

var severityNames = []string{"emergency", "alert", "critical", "error", "warning", "notice", "info", "debug"}

// SeverityName is ECS's log.level for a syslog severity.
func SeverityName(sev int) string {
	if sev < 0 || sev >= len(severityNames) {
		return "info"
	}
	return severityNames[sev]
}

// Record is one thing to send.
type Record struct {
	Time     time.Time
	Source   string // Sources
	Service  string // janusd, haproxy, keepalived, kernel...
	Severity int
	Message  string
	// Fields are structured data: an event's payload, an audit record,
	// an access log HAProxy formatted as JSON.
	Fields map[string]any
	// ID, when set, makes the record's copy in Elasticsearch unique
	// (sent twice, stored once).
	ID string
}

// goLogPrefix is the time janusd's log puts before each line.
var goLogPrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? `)

// Classify says which source a line a service printed belongs to, its
// severity, and the line as it's sent (janusd's own time stripped: the
// record has its own).
func Classify(service, line string) (source string, severity int, message string) {
	switch service {
	case "janusd":
		message = goLogPrefix.ReplaceAllString(line, "")
		severity = SevInfo
		if strings.HasPrefix(message, "api: ") {
			severity = SevNotice
		}
		return SourceJanusd, severity, message
	case "haproxy":
		if sev, ok := HAProxyMessage(line); ok {
			return SourceHAProxy, sev, line
		}
		return SourceAccess, priorityOr(line, SevInfo), line
	case "kernel":
		return SourceKernel, SevInfo, line
	}
	return SourceExtensions, extensionSeverity(line), line
}

// HAProxyMessage says whether line is one of HAProxy's own messages
// ("[WARNING]  (85) : Server app/app1 is DOWN") rather than an access
// log, and its severity.
func HAProxyMessage(line string) (int, bool) {
	if !strings.HasPrefix(line, "[") {
		return 0, false
	}
	level, _, ok := strings.Cut(line[1:], "]")
	if !ok {
		return 0, false
	}
	switch level {
	case "EMERG":
		return SevEmergency, true
	case "ALERT":
		return SevAlert, true
	case "ERROR", "ERR":
		return SevError, true
	case "WARNING":
		return SevWarning, true
	case "NOTICE":
		return SevNotice, true
	case "DIAG", "DEBUG":
		return SevDebug, true
	}
	return 0, false
}

// priorityOr reads a syslog priority at the start of line ("<134>...",
// HAProxy's log formats short, rfc3164, rfc5424) - its severity, or def.
func priorityOr(line string, def int) int {
	if len(line) < 3 || line[0] != '<' {
		return def
	}
	end := strings.IndexByte(line, '>')
	if end < 2 || end > 4 {
		return def
	}
	pri := 0
	for _, c := range line[1:end] {
		if c < '0' || c > '9' {
			return def
		}
		pri = pri*10 + int(c-'0')
	}
	return pri & 7
}

// extensionSeverity reads the level the extensions put in their lines:
// consul "[ERROR]", BIRD "<ERR>", node_exporter "level=error",
// keepalived plain (info).
func extensionSeverity(line string) int {
	l := line
	if len(l) > 80 {
		l = l[:80]
	}
	for _, m := range []struct {
		marks []string
		sev   int
	}{
		{[]string{"[ERROR]", "<ERR>", "<FATAL>", "<BUG>", "level=error", "level=ERROR", "[FATAL]"}, SevError},
		{[]string{"[WARN]", "<WARN>", "level=warn", "level=WARN", "[WARNING]"}, SevWarning},
		{[]string{"[DEBUG]", "[TRACE]", "<DBG>", "<TRACE>", "level=debug", "level=DEBUG"}, SevDebug},
	} {
		for _, mark := range m.marks {
			if strings.Contains(l, mark) {
				return m.sev
			}
		}
	}
	return SevInfo
}
