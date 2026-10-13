// Package events is janusd's event log, streamed by SystemService.Events:
// what the node did and why (boot, config applied/rejected, HAProxy
// reloads, service state changes, upgrades, reboots...), for an operator
// who has no shell to look around with.
//
// A node keeps it on STATE (Persist): its last Capacity events survive
// janusd's restarts, the node's reboots and its updates - what happened
// before an update is still there after it -, IDs going on from where
// they stopped. A janusd elsewhere (on a host, in a test) keeps it in
// memory only.
package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/ring"
)

// Capacity is how many events are kept - in memory for a newly connected
// reader, and on STATE across restarts.
const Capacity = 1000

// FileName is the log's file in Persist's directory: one JSON object per
// line, oldest first.
const FileName = "events.jsonl"

// syncEvery is how often new events are forced to the disk: a power cut
// loses at most that much (a reboot through janusd loses nothing - it
// syncs first).
const syncEvery = 5 * time.Second

type Event struct {
	// ID starts at 1 and increases by one per event, across restarts
	// where the log is kept (Persist).
	ID      uint64
	Time    time.Time
	Type    string
	Payload []byte // JSON
}

var (
	mu     sync.Mutex
	held          = ring.New[Event](Capacity)
	nextID uint64 = 1
	disk   *store // nil: in memory only
)

// Publish records an event. payload is JSON-encoded; nil means none.
func Publish(typ string, payload any) {
	var data []byte
	if payload != nil {
		data, _ = json.Marshal(payload)
	}
	mu.Lock()
	defer mu.Unlock()
	e := Event{ID: nextID, Time: time.Now(), Type: typ, Payload: data}
	nextID++
	held.Append(e)
	if disk != nil {
		disk.add(e)
	}
}

// Since returns the events with an ID greater than id still held, and
// the ID to pass next time. A reader ahead of the log - it remembered an
// ID from a log that started over since - gets the events from now on.
func Since(id uint64) ([]Event, uint64) {
	mu.Lock()
	r, last := held, nextID-1
	mu.Unlock()
	id = min(id, last)
	var out []Event
	r.Each(func(e *Event) {
		if e.ID > id {
			out = append(out, *e)
		}
	})
	if len(out) == 0 {
		return nil, id
	}
	return out, out[len(out)-1].ID
}

// Changed returns a channel closed on the next Publish - see
// ring.Ring.Changed for how to use it without missing one.
func Changed() <-chan struct{} {
	mu.Lock()
	r := held
	mu.Unlock()
	return r.Changed()
}

// Persist keeps the log in dir (FileName) from now on, after loading
// what an earlier run kept there: its events come first, with their IDs,
// and the next ones go on after them. Events published before Persist
// follow, renumbered. Lines that can't be read - a torn last line after
// a power cut - are skipped. On an error the log stays in memory, as it
// was. Called once, before anything reads the log.
func Persist(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, FileName)
	kept, skipped, err := load(path)
	if err != nil {
		return err
	}
	if skipped > 0 {
		log.Printf("events: %d unreadable line(s) in %s skipped", skipped, path)
	}

	mu.Lock()
	defer mu.Unlock()
	if disk != nil {
		return fmt.Errorf("events: already kept in %s", disk.path)
	}
	all := kept
	next := uint64(1)
	if len(kept) > 0 {
		next = kept[len(kept)-1].ID + 1
	}
	held.Each(func(e *Event) {
		early := *e
		early.ID = next
		next++
		all = append(all, early)
	})
	if len(all) > Capacity {
		all = all[len(all)-Capacity:]
	}
	s := &store{path: path}
	if err := s.rewrite(all); err != nil {
		return err
	}
	r := ring.New[Event](Capacity)
	for _, e := range all {
		r.Append(e)
	}
	held, nextID, disk = r, next, s
	go s.syncLoop()
	return nil
}

// Flush forces what's been logged to the disk - before the machine goes
// down.
func Flush() {
	mu.Lock()
	defer mu.Unlock()
	if disk != nil {
		disk.sync()
	}
}

// load reads the events kept at path, the last Capacity of them; a
// missing file is an empty log.
func load(path string) (kept []Event, skipped int, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.ID == 0 || (len(kept) > 0 && l.ID <= kept[len(kept)-1].ID) {
			skipped++
			continue
		}
		kept = append(kept, l.event())
	}
	if len(kept) > Capacity {
		kept = kept[len(kept)-Capacity:]
	}
	return kept, skipped, nil
}

// line is an event as the file keeps it.
type line struct {
	ID      uint64          `json:"id"`
	Time    time.Time       `json:"time"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (l line) event() Event {
	e := Event{ID: l.ID, Time: l.Time, Type: l.Type}
	if len(l.Payload) > 0 {
		e.Payload = []byte(l.Payload)
	}
	return e
}

func encode(e Event) []byte {
	l := line{ID: e.ID, Time: e.Time, Type: e.Type}
	if len(e.Payload) > 0 && json.Valid(e.Payload) {
		l.Payload = e.Payload
	}
	data, _ := json.Marshal(l)
	return append(data, '\n')
}

// store is the log's file, appended to as events come and rewritten
// with the last Capacity once it holds twice that. Its methods run
// under mu.
type store struct {
	path  string
	f     *os.File
	lines int
	dirty bool
}

func (s *store) add(e Event) {
	if s.f == nil {
		return
	}
	if _, err := s.f.Write(encode(e)); err != nil {
		s.fail(err)
		return
	}
	s.lines++
	s.dirty = true
	if s.lines >= 2*Capacity {
		var all []Event
		held.Each(func(e *Event) { all = append(all, *e) })
		if err := s.rewrite(all); err != nil {
			s.fail(err)
		}
	}
}

// rewrite replaces the file with all - written aside, synced, renamed
// over it, the directory synced - and appends to it from then on.
func (s *store) rewrite(all []Event) error {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
	var buf bytes.Buffer
	for _, e := range all {
		buf.Write(encode(e))
	}
	tmp := s.path + ".tmp"
	if err := writeSynced(tmp, buf.Bytes()); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	s.f, s.lines, s.dirty = f, len(all), false
	return nil
}

func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s *store) sync() {
	if s.f == nil || !s.dirty {
		return
	}
	if err := s.f.Sync(); err != nil {
		s.fail(err)
		return
	}
	s.dirty = false
}

// fail stops keeping the log on the disk, logged once: it goes on in
// memory.
func (s *store) fail(err error) {
	log.Printf("events: %s: %v - the event log is kept in memory only from now on", s.path, err)
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

func (s *store) syncLoop() {
	for {
		time.Sleep(syncEvery)
		mu.Lock()
		if s != disk || s.f == nil {
			mu.Unlock()
			return
		}
		s.sync()
		mu.Unlock()
	}
}
