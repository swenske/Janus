// Package kmsgwatch follows the kernel log (/dev/kmsg) from the start of
// its buffer and counts what the node's exporter reports: SELinux
// denials and OOM kills since boot.
package kmsgwatch

import (
	"errors"
	"io"
	"log"
	"os"
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
		_, msg, ok := strings.Cut(rec, ";")
		if !ok {
			continue
		}
		msg, _, _ = strings.Cut(msg, "\n")
		switch denial, oom := Classify(msg); {
		case denial:
			c.SELinuxDenials.Add(1)
		case oom:
			c.OOMKills.Add(1)
		}
	}
}
