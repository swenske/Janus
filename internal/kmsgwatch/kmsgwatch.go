// Package kmsgwatch follows the kernel log (/dev/kmsg) from the start of
// its buffer and counts what the node's exporter reports: SELinux
// denials and OOM kills since boot - and tells of each new OOM kill
// (OnOOMKill).
package kmsgwatch

import (
	"errors"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Counts are the watched events since boot (as far back as the kernel's
// log buffer still reaches when janusd starts).
type Counts struct {
	SELinuxDenials atomic.Uint64
	OOMKills       atomic.Uint64
	// Running is set once the kernel log could be opened.
	Running atomic.Bool

	// OnOOMKill, if set, is called for each OOM kill of the kernel log,
	// once per boot: ToldFile keeps the last one's sequence number, so
	// a restarted janusd - the OOM killer's victim, maybe - tells those
	// after it, not the backlog again. pid and process are the victim's
	// (0 and "" when the line doesn't say).
	OnOOMKill func(pid int, process string)
	ToldFile  string
}

// OOMVictim reads the process an OOM kill message names: "Out of memory:
// Killed process 1234 (haproxy) total-vm:...".
func OOMVictim(msg string) (pid int, process string) {
	_, rest, ok := strings.Cut(msg, "Killed process ")
	if !ok {
		return 0, ""
	}
	num, rest, _ := strings.Cut(rest, " ")
	pid, _ = strconv.Atoi(num)
	if strings.HasPrefix(rest, "(") {
		if end := strings.Index(rest, ")"); end > 0 {
			process = rest[1:end]
		}
	}
	return pid, process
}

// recordSeq is a record's sequence number: its header's second field.
func recordSeq(header string) (uint64, bool) {
	f := strings.SplitN(header, ",", 3)
	if len(f) < 2 {
		return 0, false
	}
	seq, err := strconv.ParseUint(f[1], 10, 64)
	return seq, err == nil
}

// told reads the last told sequence number; ok false when none was told
// this boot.
func (c *Counts) told() (uint64, bool) {
	if c.ToldFile == "" {
		return 0, false
	}
	data, err := os.ReadFile(c.ToldFile)
	if err != nil {
		return 0, false
	}
	seq, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return seq, err == nil
}

// Classify tells what a kernel log message is.
func Classify(msg string) (denial, oomKill bool) {
	if strings.Contains(msg, "avc:  denied") {
		return true, false
	}
	// "Out of memory: Killed process 123 (x)" and the memory cgroup's
	// "Memory cgroup out of memory: Killed process ...".
	if strings.Contains(strings.ToLower(msg), "out of memory: killed process") {
		return false, true
	}
	return false, false
}

// Watch reads path (/dev/kmsg) until it can't, counting into c. Meant to
// run in its own goroutine for janusd's lifetime.
func Watch(path string, c *Counts) {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("kmsgwatch: %v - SELinux denial and OOM kill counts unavailable", err)
		return
	}
	defer f.Close()
	c.Running.Store(true)
	lastTold, anyTold := c.told()
	buf := make([]byte, 8192) // a read returns one record, up to ~1 KiB
	for {
		n, err := f.Read(buf)
		if err != nil {
			// EPIPE: records were overwritten before being read - the
			// next read continues from the oldest one still there.
			if errors.Is(err, syscall.EPIPE) {
				continue
			}
			if errors.Is(err, io.EOF) {
				time.Sleep(time.Second)
				continue
			}
			log.Printf("kmsgwatch: read %s: %v", path, err)
			c.Running.Store(false)
			return
		}
		// "<prio>,<seq>,<usec>,<flags>;<message>\n[ KEY=value continuation lines]"
		rec := string(buf[:n])
		header, msg, ok := strings.Cut(rec, ";")
		if !ok {
			continue
		}
		msg, _, _ = strings.Cut(msg, "\n")
		switch denial, oom := Classify(msg); {
		case denial:
			c.SELinuxDenials.Add(1)
		case oom:
			c.OOMKills.Add(1)
			seq, ok := recordSeq(header)
			if !ok || c.OnOOMKill == nil || anyTold && seq <= lastTold {
				break
			}
			c.OnOOMKill(OOMVictim(msg))
			lastTold, anyTold = seq, true
			if c.ToldFile != "" {
				if err := os.WriteFile(c.ToldFile, []byte(strconv.FormatUint(seq, 10)+"\n"), 0o644); err != nil {
					log.Printf("kmsgwatch: %v", err)
				}
			}
		}
	}
}
