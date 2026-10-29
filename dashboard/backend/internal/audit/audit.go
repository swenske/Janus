// Package audit is the Controller's record of who changed what: every
// request that isn't a read, with the account that made it, and every
// sign-in, failed or not. One JSON object per line in
// <data-dir>/audit.jsonl, the previous file kept as audit.jsonl.1 once it
// reaches MaxSize - a bounded history, newest first when read. Each
// entry has an ID, one more than the previous, for a reader that keeps
// its place (the log shipper: Since, Changed), and names what it's
// about (Subject) as it was named then - a node removed since keeps its
// name in the audit. Query reads it a page at a time, filtered
// (query.go).
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
	// ID goes on from one entry to the next, across restarts (0: an
	// entry written before IDs existed).
	ID   uint64    `json:"id,omitempty"`
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
	// Subject is what the request was about, when its path names
	// something (a node, a machine...) or its handler said so (what it
	// created); nil otherwise - and in the entries written before
	// subjects existed, where Query derives it from the path.
	Subject *Subject `json:"subject,omitempty"`
}

// The kinds of Subject.
const (
	KindNode       = "node"
	KindPending    = "pending" // a node waiting for approval
	KindMachine    = "machine"
	KindCluster    = "cluster"
	KindHypervisor = "hypervisor"
	KindAccount    = "account"
	KindToken      = "token"            // an API token
	KindEnrollment = "enrollment-token" // a node enrollment token
	KindSSO        = "sso-provider"
)

// Subject is what an entry is about.
type Subject struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// Name is its name when the entry was written (empty: unknown - an
	// entry from before subjects, or something already gone then).
	Name string `json:"name,omitempty"`
	// Gone: it doesn't exist any more - set by the reader that checked
	// (GET /api/audit), never written.
	Gone bool `json:"gone,omitempty"`
}

// Key is the subject as a filter names it: "kind:id".
func (s Subject) Key() string { return s.Kind + ":" + s.ID }

// Log appends entries to a file.
type Log struct {
	path string

	mu      sync.Mutex
	size    int64
	next    uint64
	changed chan struct{}
}

func Open(dataDir string) (*Log, error) {
	l := &Log{path: filepath.Join(dataDir, "audit.jsonl"), next: 1, changed: make(chan struct{})}
	fi, err := os.Stat(l.path)
	switch {
	case err == nil:
		l.size = fi.Size()
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	// IDs go on from the newest entry kept.
	all, err := l.all()
	if err != nil {
		return nil, err
	}
	for _, e := range all {
		l.next = max(l.next, e.ID+1)
	}
	return l, nil
}

// Append records e - an audit that can't be written is logged by the
// caller, never fails the request it records.
func (l *Log) Append(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.ID = l.next
	if e.Subject != nil && e.Subject.Gone {
		s := *e.Subject
		s.Gone = false
		e.Subject = &s
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
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
	if err == nil {
		l.next++
		close(l.changed)
		l.changed = make(chan struct{})
	}
	return err
}

// Changed returns a channel closed on the next entry appended.
func (l *Log) Changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.changed
}

// Latest is the newest entry's ID (0: none).
func (l *Log) Latest() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.next - 1
}

// Since returns up to limit entries with an ID greater than id, oldest
// first, and the last ID returned (id when none).
func (l *Log) Since(id uint64, limit int) ([]Entry, uint64) {
	l.mu.Lock()
	if id >= l.next-1 {
		// Nothing after id - the log shipper asks at least twice a
		// flush: not worth reading and decoding the files.
		l.mu.Unlock()
		return nil, id
	}
	all, _ := l.all()
	l.mu.Unlock()
	var out []Entry
	last := id
	for _, e := range all {
		if e.ID > id && len(out) < limit {
			out = append(out, e)
			last = e.ID
		}
	}
	return out, last
}

// all is every entry kept, oldest first. Under mu.
func (l *Log) all() ([]Entry, error) {
	var out []Entry
	for _, p := range []string{l.path + ".1", l.path} {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

// Recent returns up to limit entries, newest first, matching keep (nil:
// all) - as they were written (Query reads a page of them, filtered).
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
