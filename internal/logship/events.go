package logship

import (
	"encoding/json"
	"fmt"
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

// AuditRecord is an entry of the node's audit (internal/api.AuditEntry,
// its payload) as a record: "SystemService/Reboot by admin (os:admin)
// from 10.0.2.2:41234: OK in 3 ms", the entry as fields.
func AuditRecord(id uint64, t time.Time, method string, payload []byte) Record {
	var e struct {
		Caller     string   `json:"caller"`
		Roles      []string `json:"roles"`
		Via        string   `json:"via"`
		Peer       string   `json:"peer"`
		Code       string   `json:"code"`
		Error      string   `json:"error"`
		Denied     bool     `json:"denied"`
		DurationMs int64    `json:"duration_ms"`
	}
	_ = json.Unmarshal(payload, &e)
	who := e.Caller
	if len(e.Roles) > 0 {
		who += " (" + strings.Join(e.Roles, ", ") + ")"
	}
	if e.Via != "" {
		who += " via " + e.Via
	}
	msg := method + " by " + who
	if e.Peer != "" {
		msg += " from " + e.Peer
	}
	sev := SevNotice
	switch {
	case e.Denied:
		msg += ": refused: " + e.Error
		sev = SevWarning
	case e.Code != "OK":
		msg += ": " + e.Code + ": " + e.Error
	default:
		msg += fmt.Sprintf(": OK in %d ms", e.DurationMs)
	}
	fields := map[string]any{"event": map[string]any{"id": id, "type": method}}
	var all map[string]any
	if json.Unmarshal(payload, &all) == nil {
		fields["audit"] = all
	}
	return Record{Time: t, Source: SourceAudit, Service: "janusd", Severity: sev, Message: msg, Fields: fields}
}
