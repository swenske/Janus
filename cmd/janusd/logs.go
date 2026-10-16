package main

import (
	"encoding/hex"
	"log"
	"os"
	"strings"

	"github.com/swenske/Janus/internal/bootslot"
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

// logTee is what a service's output also goes to on the LOGS partition
// (nil without one).
func logTee(v *logvol.Volume, service string) func(string) {
	if v == nil {
		return nil
	}
	return v.Line(service)
}

// haproxyLogTee keeps HAProxy's own messages ("[WARNING]  (85) : ...")
// on the LOGS partition, not its access logs: those can be thousands a
// second, and are the log shipper's.
func haproxyLogTee(v *logvol.Volume) func(string) {
	if v == nil {
		return nil
	}
	return func(line string) {
		if sev, ok := haproxyMessage(line); ok {
			v.Append(logvol.Record{Service: "haproxy", Severity: sev, Message: line})
		}
	}
}

// haproxyMessage says whether line is one of HAProxy's own messages, and
// its syslog severity.
func haproxyMessage(line string) (uint8, bool) {
	level, _, ok := strings.Cut(strings.TrimPrefix(line, "["), "]")
	if !ok || !strings.HasPrefix(line, "[") {
		return 0, false
	}
	switch level {
	case "EMERG":
		return 0, true
	case "ALERT":
		return 1, true
	case "ERROR", "ERR":
		return 3, true
	case "WARNING":
		return 4, true
	case "NOTICE":
		return 5, true
	case "DIAG", "DEBUG":
		return 7, true
	}
	return 0, false
}
