// Package audit is the Controller's record of who changed what: every
// request that isn't a read, with the account that made it, and every
// sign-in, failed or not. One JSON object per line in
// <data-dir>/audit.jsonl, the previous file kept as audit.jsonl.1 once it
// reaches MaxSize - a bounded history, newest first when read.
package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxSize is how large the current file grows before it's rotated: two
// files of this size are kept - tens of thousands of entries.
var MaxSize int64 = 4 << 20

// Entry is one action.
type Entry struct {
	Time time.Time `json:"time"`
	// User is the account - the name tried, for a failed sign-in -, or
	// empty when the request had none.
	User string `json:"user,omitempty"`
	// Via is how it was made: "session", "token <id>", or "" (none).
	Via    string `json:"via,omitempty"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
	// Client is the address the request came from.
	Client string `json:"client,omitempty"`
}

// Log appends entries to a file.
type Log struct {
	path string

	mu   sync.Mutex
	size int64
}

func Open(dataDir string) (*Log, error) {
	l := &Log{path: filepath.Join(dataDir, "audit.jsonl")}
	fi, err := os.Stat(l.path)
	switch {
	case err == nil:
		l.size = fi.Size()
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return l, nil
}

// Append records e - an audit that can't be written is logged by the
// caller, never fails the request it records.
func (l *Log) Append(e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size > 0 && l.size+int64(len(line)) > MaxSize {
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return err
		}
		l.size = 0
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := f.Write(line)
	l.size += int64(n)
	return err
}

// Recent returns up to limit entries, newest first, matching keep (nil:
// all).
func (l *Log) Recent(limit int, keep func(Entry) bool) ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Entry
	for _, p := range []string{l.path, l.path + ".1"} {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var lines []Entry
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) == nil && (keep == nil || keep(e)) {
				lines = append(lines, e)
			}
		}
		for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
			out = append(out, lines[i])
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
