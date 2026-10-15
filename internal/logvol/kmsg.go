package logvol

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// KmsgRecord is one /dev/kmsg record, parsed.
type KmsgRecord struct {
	Priority int    // syslog facility*8 + level
	Seq      uint64 // the kernel's sequence number
	Usec     uint64 // microseconds since boot
	Message  string
}

// Line is the record as dmesg(1) prints it: "[seconds.micros] message".
func (k KmsgRecord) Line() string {
	return fmt.Sprintf("[%5d.%06d] %s", k.Usec/1e6, k.Usec%1e6, k.Message)
}

// ParseKmsg parses one /dev/kmsg record ("prio,seq,usec,flags;msg"
// followed by optional " KEY=value" continuation lines, which are left
// out).
func ParseKmsg(rec []byte) (KmsgRecord, bool) {
	header, rest, ok := strings.Cut(string(rec), ";")
	if !ok {
		return KmsgRecord{}, false
	}
	msg, _, _ := strings.Cut(rest, "\n")
	f := strings.Split(header, ",")
	if len(f) < 3 {
		return KmsgRecord{}, false
	}
	prio, err1 := strconv.Atoi(f[0])
	seq, err2 := strconv.ParseUint(f[1], 10, 64)
	usec, err3 := strconv.ParseUint(f[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return KmsgRecord{}, false
	}
	return KmsgRecord{Priority: prio, Seq: seq, Usec: usec, Message: msg}, true
}

// FollowKmsg appends the kernel's messages to v as service "kernel",
// from the start of its buffer, forever. seqFile (on a tmpfs: it
// outlives janusd, not the boot) keeps the last one appended, so a
// restarted janusd doesn't append them twice.
func FollowKmsg(v *Volume, path, seqFile string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var after uint64
	if b, err := os.ReadFile(seqFile); err == nil {
		after, _ = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	}
	var saved time.Time
	buf := make([]byte, 8192) // each read returns one record
	for {
		n, err := f.Read(buf)
		switch {
		case err == nil:
		case errors.Is(err, unix.EPIPE):
			continue // overwritten before being read: on to the oldest left
		case errors.Is(err, unix.EINTR):
			continue
		default:
			return err
		}
		k, ok := ParseKmsg(buf[:n])
		if !ok || (after > 0 && k.Seq <= after) {
			continue
		}
		v.Append(Record{Time: kmsgTime(k.Usec), Severity: uint8(k.Priority & 7), Service: "kernel", Message: k.Line()})
		after = k.Seq
		if time.Since(saved) >= time.Second {
			_ = os.WriteFile(seqFile, []byte(strconv.FormatUint(after, 10)), 0o600)
			saved = time.Now()
		}
	}
}

// kmsgTime turns a kernel timestamp (microseconds since boot) into the
// time of day.
func kmsgTime(usec uint64) time.Time {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return time.Now()
	}
	sinceBoot := time.Duration(ts.Nano())
	return time.Now().Add(time.Duration(usec)*time.Microsecond - sinceBoot)
}
