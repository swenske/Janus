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
//
// The package's functions are the event log's; NewLog makes another
// log kept the same way - the node's audit (internal/api).
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

// Log is a log of events: the last capacity of them in memory, and on a
// disk once Persist is called.
type Log struct {
	name     string // the file's, and the messages'
	capacity int

	mu     sync.Mutex
	held   *ring.Ring[Event]
	nextID uint64
	disk   *store // nil: in memory only
}

// NewLog makes a log named name (its file: name.jsonl) keeping capacity
// events.
func NewLog(name string, capacity int) *Log {
	return &Log{name: name, capacity: capacity, held: ring.New[Event](capacity), nextID: 1}
}

// std is the event log.
var std = NewLog("events", Capacity)

// Publish records an event in the event log.
func Publish(typ string, payload any) { std.Publish(typ, payload) }

// Since is the event log's Log.Since.
func Since(id uint64) ([]Event, uint64) { return std.Since(id) }

// Changed is the event log's Log.Changed.
func Changed() <-chan struct{} { return std.Changed() }

// Persist is the event log's Log.Persist.
func Persist(dir string) error { return std.Persist(dir) }

// Flush is the event log's Log.Flush.
func Flush() { std.Flush() }

// Publish records an event. payload is JSON-encoded; nil means none.
func (l *Log) Publish(typ string, payload any) {
	var data []byte
	if payload != nil {
		data, _ = json.Marshal(payload)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := Event{ID: l.nextID, Time: time.Now(), Type: typ, Payload: data}
	l.nextID++
	l.held.Append(e)
	if l.disk != nil {
		l.disk.add(e)
	}
}

// Since returns the events with an ID greater than id still held, and
// the ID to pass next time. A reader ahead of the log - it remembered an
// ID from a log that started over since - gets the events from now on.
func (l *Log) Since(id uint64) ([]Event, uint64) {
	l.mu.Lock()
	r, last := l.held, l.nextID-1
	l.mu.Unlock()
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
func (l *Log) Changed() <-chan struct{} {
	l.mu.Lock()
	r := l.held
	l.mu.Unlock()
	return r.Changed()
}

// Persist keeps the log in dir (FileName) from now on, after loading
// what an earlier run kept there: its events come first, with their IDs,
// and the next ones go on after them. Events published before Persist
// follow, renumbered. Lines that can't be read - a torn last line after
// a power cut - are skipped. On an error the log stays in memory, as it
// was. Called once, before anything reads the log.
func (l *Log) Persist(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, l.name+".jsonl")
	kept, skipped, err := load(path, l.capacity)
	if err != nil {
		return err
	}
	if skipped > 0 {
		log.Printf("%s: %d unreadable line(s) in %s skipped", l.name, skipped, path)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disk != nil {
		return fmt.Errorf("%s: already kept in %s", l.name, l.disk.path)
	}
	all := kept
	next := uint64(1)
	if len(kept) > 0 {
		next = kept[len(kept)-1].ID + 1
	}
	l.held.Each(func(e *Event) {
		early := *e
		early.ID = next
		next++
		all = append(all, early)
	})
	if len(all) > l.capacity {
		all = all[len(all)-l.capacity:]
	}
	s := &store{l: l, path: path}
	if err := s.rewrite(all); err != nil {
		return err
	}
	r := ring.New[Event](l.capacity)
	for _, e := range all {
		r.Append(e)
	}
	l.held, l.nextID, l.disk = r, next, s
	go s.syncLoop()
	return nil
}

// Flush forces what's been logged to the disk - before the machine goes
// down.
func (l *Log) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disk != nil {
		l.disk.sync()
	}
}

// load reads the events kept at path, the last capacity of them; a
// missing file is an empty log.
func load(path string, capacity int) (kept []Event, skipped int, err error) {
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
	if len(kept) > capacity {
		kept = kept[len(kept)-capacity:]
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

// store is a log's file, appended to as events come and rewritten
// with the last capacity once it holds twice that. Its methods run
// under the log's mu.
type store struct {
	l     *Log
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
	if s.lines >= 2*s.l.capacity {
		var all []Event
		s.l.held.Each(func(e *Event) { all = append(all, *e) })
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
	log.Printf("%s: %s: %v - kept in memory only from now on", s.l.name, s.path, err)
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

func (s *store) syncLoop() {
	for {
		time.Sleep(syncEvery)
		s.l.mu.Lock()
		if s != s.l.disk || s.f == nil {
			s.l.mu.Unlock()
			return
		}
		s.sync()
		s.l.mu.Unlock()
	}
}
