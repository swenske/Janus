package sysctl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The history of every change, on STATE: who did what, when, each value
// before and after - the node's own audit log of its kernel parameters,
// whoever called (the Controller, janusctl, an orchestrator).
const (
	historyName = "sysctl-history.jsonl"
	historyMax  = 1000 // the newest entries kept
)

// The actions an entry records.
const (
	ActionTrial       = "trial"        // values applied on trial
	ActionConfirm     = "confirm"      // the trial's values saved
	ActionCancel      = "cancel"       // the trial undone on request
	ActionRevert      = "revert"       // the trial undone when its time ran out
	ActionBootRefused = "boot-refused" // saved lines the boot didn't apply
)

// Actor is who an entry's action is for.
type Actor struct {
	Name  string   `json:"name"`
	Roles []string `json:"roles,omitempty"`
	Via   string   `json:"via,omitempty"` // the Controller acting for Name
}

func (a Actor) String() string {
	s := a.Name
	if len(a.Roles) > 0 {
		s += " (" + strings.Join(a.Roles, ", ") + ")"
	}
	if a.Via != "" {
		s += " via " + a.Via
	}
	return s
}

// ChangeRecord is one parameter an entry changed.
type ChangeRecord struct {
	Name  string `json:"name"`
	Old   string `json:"old"`
	New   string `json:"new"`
	Reset bool   `json:"reset,omitempty"` // back to Janus's default
}

// Entry is one line of the history.
type Entry struct {
	Time    time.Time      `json:"time"`
	Actor   Actor          `json:"actor"`
	Action  string         `json:"action"`
	Changes []ChangeRecord `json:"changes,omitempty"`
	Detail  string         `json:"detail,omitempty"`
}

// appendHistory adds e, keeping the newest historyMax entries. The file
// is rewritten whole, atomically: changes are rare, and a power cut
// leaves the old history or the new one.
func appendHistory(e Entry) error {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	path := filepath.Join(Dir, historyName)
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := bytes.Split(bytes.TrimRight(old, "\n"), []byte("\n"))
	if len(old) == 0 {
		lines = nil
	}
	lines = append(lines, line)
	if len(lines) > historyMax {
		lines = lines[len(lines)-historyMax:]
	}
	return writeAtomic(path, append(bytes.Join(lines, []byte("\n")), '\n'))
}

// History is the newest entries first, limit of them at most (0: all).
func History(limit int) ([]Entry, error) {
	f, err := os.Open(filepath.Join(Dir, historyName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var all []Entry
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64<<10), 1<<20)
	for s.Scan() {
		var e Entry
		if json.Unmarshal(s.Bytes(), &e) == nil {
			all = append(all, e)
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		out = append(out, all[i])
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}
