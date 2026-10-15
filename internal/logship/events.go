package logship

import (
	"encoding/json"
	"strings"
	"time"
)

// EventRecord is an event of the node's event log (internal/events) as
// a record: its type and payload as the message, both as fields.
func EventRecord(id uint64, t time.Time, typ string, payload []byte, service string) Record {
	msg := typ
	fields := map[string]any{"event": map[string]any{"id": id, "type": typ}}
	if len(payload) > 0 {
		msg += " " + string(payload)
		var p any
		if json.Unmarshal(payload, &p) == nil {
			fields["payload"] = p
		}
	}
	return Record{Time: t, Source: SourceEvents, Service: service, Severity: eventSeverity(typ), Message: msg, Fields: fields}
}

// eventSeverity: failures and reverts stand out.
func eventSeverity(typ string) int {
	switch {
	case strings.Contains(typ, "failed"), strings.Contains(typ, "rejected"), strings.HasSuffix(typ, "oom_kill"),
		strings.Contains(typ, "failing"), strings.Contains(typ, "not_compliant"):
		return SevWarning
	case strings.Contains(typ, "reverted"), strings.Contains(typ, "exited"), strings.Contains(typ, "untracked"):
		return SevNotice
	}
	return SevInfo
}
