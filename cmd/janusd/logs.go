package main

import (
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/bootslot"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/logship"
	"github.com/swenske/Janus/internal/logvol"
)

// openLogVolume opens the LOGS partition of the disk the node booted
// from (internal/logvol) - nil when it has none: a node installed before
// LOGS existed, an ISO boot. Its outcome is logged by logVolumeSays once
// the log is set up.
func openLogVolume() (*logvol.Volume, string, string) {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, "", "no LOGS partition (/proc/cmdline: " + err.Error() + ")"
	}
	data, ok := bootslot.DataDevice(string(cmdline))
	if !ok {
		return nil, "", "no LOGS partition (not booted from a Janus disk)"
	}
	dev, ok := bootslot.LogsDevice(data)
	if !ok {
		return nil, "", "no LOGS partition on this disk - this boot's logs are kept in memory only"
	}
	v, err := logvol.Open(dev, logvol.Options{BootID: kernelBootID()})
	if err != nil {
		return nil, dev, "LOGS partition " + dev + " unusable - this boot's logs are kept in memory only: " + err.Error()
	}
	return v, dev, ""
}

// kernelBootID is the kernel's boot_id, the same for every janusd of
// one boot.
func kernelBootID() [16]byte {
	var id [16]byte
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return id
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(string(b)), "-", ""))
	if err == nil && len(raw) == len(id) {
		copy(id[:], raw)
	}
	return id
}

// logVolumeSays logs how the LOGS partition was opened.
func logVolumeSays(v *logvol.Volume, dev, why string) {
	if v == nil {
		if why != "" {
			log.Printf("logs: %s", why)
		}
		return
	}
	boots := v.Boots()
	log.Printf("logs: kept on %s (%d MiB, %d earlier boots) - this is boot %d", dev, v.Stats().SizeBytes>>20, len(boots), v.Current())
}

// logHub is where every line a service prints goes besides the console
// and the rings: the LOGS partition (internal/logvol) and the log
// shipper (internal/logship). Nothing in it waits.
type logHub struct {
	vol  *logvol.Volume // nil without a LOGS partition
	ship *logship.Shipper
}

// tee is what service's output also goes to (ring.LineWriter's Tee).
// The LOGS partition keeps all but HAProxy's access logs - unless the
// configuration says always: they can be thousands a second.
func (h *logHub) tee(service string) func(string) {
	return func(line string) {
		src, sev, msg := logship.Classify(service, line)
		if h.vol != nil {
			switch {
			case src != logship.SourceAccess:
				h.vol.Append(logvol.Record{Service: service, Severity: uint8(sev), Message: line})
			case h.ship.AccessOnDisk():
				h.vol.Append(logvol.Record{Service: logship.AccessService, Severity: uint8(sev), Message: line})
			}
		}
		h.ship.Offer(logship.Record{Time: time.Now(), Source: src, Service: service, Severity: sev, Message: msg})
	}
}

// kernel takes one of the kernel's messages (logvol.FollowKmsg).
func (h *logHub) kernel(k logvol.KmsgRecord, at time.Time) {
	sev := k.Priority & 7
	if h.vol != nil {
		h.vol.Append(logvol.Record{Time: at, Service: "kernel", Severity: uint8(sev), Message: k.Line()})
	}
	h.ship.Offer(logship.Record{Time: at, Source: logship.SourceKernel, Service: "kernel", Severity: sev, Message: k.Line()})
}

// eventsFeed is the event log (internal/events) as a source the log
// shipper's targets read behind their cursors.
func eventsFeed() logship.Feed {
	return logship.Feed{
		Name: logship.SourceEvents,
		Since: func(id uint64) ([]logship.FeedItem, uint64) {
			evs, last := events.Since(id)
			items := make([]logship.FeedItem, 0, len(evs))
			for _, e := range evs {
				r := logship.EventRecord(e.ID, e.Time, e.Type, e.Payload, "janusd")
				r.ID = fmt.Sprintf("ev-%d", e.ID)
				items = append(items, logship.FeedItem{ID: e.ID, Record: r})
			}
			return items, last
		},
		Latest: func() uint64 {
			_, last := events.Since(math.MaxUint64)
			return last
		},
		Changed: events.Changed,
	}
}

// auditFeed is the node's audit (api.AuditLog) as a source the log
// shipper's targets read behind their cursors.
func auditFeed(l *events.Log) logship.Feed {
	return logship.Feed{
		Name: logship.SourceAudit,
		Since: func(id uint64) ([]logship.FeedItem, uint64) {
			evs, last := l.Since(id)
			items := make([]logship.FeedItem, 0, len(evs))
			for _, e := range evs {
				r := logship.AuditRecord(e.ID, e.Time, e.Type, e.Payload)
				r.ID = fmt.Sprintf("au-%d", e.ID)
				items = append(items, logship.FeedItem{ID: e.ID, Record: r})
			}
			return items, last
		},
		Latest: func() uint64 {
			_, last := l.Since(math.MaxUint64)
			return last
		},
		Changed: l.Changed,
	}
}
