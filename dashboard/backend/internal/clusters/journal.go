package clusters

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The journal: what was done to a cluster, by whom, member by member -
// joins, applies, pulls, renewals, drift seen. Append-only, the newest
// 1000 kept once past 2000.

const (
	journalFile = "journal.jsonl"
	journalKeep = 1000
	journalMax  = 2000
)

// Entry is one line of the journal.
type Entry struct {
	Time    time.Time `json:"time"`
	Actor   string    `json:"actor"` // the account, or "cluster" for the Controller's own loop
	Action  string    `json:"action"`
	Member  string    `json:"member,omitempty"`
	Version int       `json:"version,omitempty"`
	Message string    `json:"message"`
	Error   string    `json:"error,omitempty"`
}

// Journal appends e to cluster id's journal.
func (s *Store) Journal(id string, e Entry) error {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.clusterDir(id), journalFile)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return trimJournal(p)
}

func trimJournal(p string) error {
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) <= journalMax {
		return nil
	}
	kept := bytes.Join(lines[len(lines)-journalKeep:], []byte("\n"))
	return writeDurable(p, append(kept, '\n'))
}

// Entries are cluster id's newest n journal entries, newest first.
func (s *Store) Entries(id string, n int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(filepath.Join(s.clusterDir(id), journalFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			all = append(all, e)
		}
	}
	out := make([]Entry, 0, n)
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i])
	}
	return out
}
