package logship

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/swenske/Janus/internal/logvol"
)

const (
	// batchRecords and batchBytes bound what one send carries.
	batchRecords = 500
	batchBytes   = 1 << 20
	// queueRecords and queueBytes bound what waits in memory for one
	// target; past that, the oldest is dropped.
	queueRecords = 20000
	queueBytes   = 16 << 20
	// failingAfter is how long a target fails before it's said to.
	failingAfter = 30 * time.Second
	maxBackoff   = time.Minute
	// saveEvery is how often moved cursors are saved.
	saveEvery = 5 * time.Second
)

// Meta is who sends: what every record carries besides itself.
type Meta struct {
	// Hostname is asked for each batch (a node's can change); os.Hostname
	// when nil.
	Hostname func() string
	// Agent is "janusd" or "dashboardd", AgentVersion its version.
	Agent, AgentVersion string
	// Slot and Schematic are the node's.
	Slot, Schematic string
}

// env is what a sink needs of its shipper.
type env struct {
	meta   *Meta
	labels func() map[string]string
	// nodeID identifies the node: its CA's SHA-256 (the console's "pki:
	// this node's CA" line). Empty on the Controller.
	nodeID func() string
}

func (m *Meta) hostname() string {
	if m.Hostname != nil {
		return m.Hostname()
	}
	h, _ := os.Hostname()
	return h
}

// Feed is a source kept on disk with an ID per item - the event log,
// the audit - that each target reads behind its own cursor.
type Feed struct {
	// Name is the source the feed's records belong to (SourceEvents,
	// SourceAudit).
	Name string
	// Since returns the items after id, oldest first, and the last ID
	// returned (id when none).
	Since func(id uint64) ([]FeedItem, uint64)
	// Latest is the newest item's ID: where a new target starts.
	Latest func() uint64
	// Changed returns a channel closed when an item is added.
	Changed func() <-chan struct{}
}

// FeedItem is one item of a Feed.
type FeedItem struct {
	ID     uint64
	Record Record
}

// Options are what a Shipper works from.
type Options struct {
	Meta Meta
	// Volume is the node's LOGS partition: the services' lines and the
	// kernel's are read from it behind each target's cursor. Nil: they
	// come through Offer, from memory.
	Volume *logvol.Volume
	// Feeds are the sources kept on disk besides the volume.
	Feeds []Feed
	// CursorFile keeps the feeds' cursors (a node's STATE); empty: in
	// memory only.
	CursorFile string
	// OnState is told when a target starts failing, or works again.
	OnState func(target, state, lastError string)
}

// Shipper sends records to the configured targets.
type Shipper struct {
	opt Options

	mu      sync.Mutex
	cfg     Config
	targets map[string]*target
	closed  bool
	// accessOnDisk is cfg.AccessLogsOnDisk == "always", and labels
	// cfg.Labels, read without mu; nodeID is SetNodeID's.
	accessOnDisk atomic.Bool
	labels       atomic.Pointer[map[string]string]
	nodeID       atomic.Pointer[string]

	cursorsMu    sync.Mutex
	cursors      cursorFile
	dirty        bool
	volForgotten map[string]bool // removed targets, to drop from the volume's slot
	stop         chan struct{}
	done         chan struct{}
}

// cursorFile is CursorFile's content: per feed, per target.
type cursorFile struct {
	Feeds map[string]map[string]uint64 `json:"feeds,omitempty"`
}

// volCursors is what's saved in the volume's cursor slot: per target.
type volCursors struct {
	Targets map[string]uint64 `json:"targets"`
}

// New makes a Shipper, with no target until Apply.
func New(opt Options) *Shipper {
	s := &Shipper{opt: opt, targets: map[string]*target{}, stop: make(chan struct{}), done: make(chan struct{})}
	if opt.CursorFile != "" {
		_ = os.MkdirAll(filepath.Dir(opt.CursorFile), 0o700)
		if b, err := os.ReadFile(opt.CursorFile); err == nil {
			_ = json.Unmarshal(b, &s.cursors)
		}
	}
	go s.saveLoop()
	return s
}

// Apply makes cfg (validated) the configuration: targets added start
// from now, targets removed stop, a target changed starts again where
// it was.
func (s *Shipper) Apply(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.cfg = cfg
	labels := cfg.Labels
	s.labels.Store(&labels)
	s.accessOnDisk.Store(cfg.AccessLogsOnDisk == "always")
	keep := map[string]bool{}
	for _, t := range cfg.Targets {
		keep[t.Name] = true
		var prev *target
		if old, ok := s.targets[t.Name]; ok {
			if reflect.DeepEqual(old.cfg, t) {
				continue
			}
			old.halt()
			delete(s.targets, t.Name)
			prev = old
		}
		if t.Disabled {
			if prev != nil {
				s.saveCursorsOf([]*target{prev})
			}
			continue
		}
		s.targets[t.Name] = s.start(t, prev)
	}
	for name, t := range s.targets {
		if !keep[name] {
			t.halt()
			delete(s.targets, name)
			s.forget(name)
		}
	}
}

// Config is the configuration applied.
func (s *Shipper) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// AccessOnDisk says whether HAProxy's access logs are to be kept on the
// LOGS partition (the configuration's access_logs_on_disk "always").
func (s *Shipper) AccessOnDisk() bool { return s.accessOnDisk.Load() }

// SetNodeID sets what identifies the node in what's sent: its CA's
// SHA-256, known once the PKI is loaded.
func (s *Shipper) SetNodeID(id string) { s.nodeID.Store(&id) }

// onDisk says whether source's records are read from the volume rather
// than offered from memory.
func (s *Shipper) onDisk(source string) bool {
	if s.opt.Volume == nil {
		return false
	}
	switch source {
	case SourceJanusd, SourceHAProxy, SourceExtensions, SourceKernel:
		return true
	case SourceAccess:
		return s.accessOnDisk.Load()
	}
	return false
}

// Offer queues r for the targets that take its source, unless they read
// it from disk. It never waits.
func (s *Shipper) Offer(r Record) {
	s.mu.Lock()
	if s.onDisk(r.Source) || len(s.targets) == 0 {
		s.mu.Unlock()
		return
	}
	var to []*target
	for _, t := range s.targets {
		if t.cfg.Takes(r.Source) {
			to = append(to, t)
		}
	}
	s.mu.Unlock()
	for _, t := range to {
		t.mem.push(r)
		t.poke()
	}
}

// Close stops every target and saves the cursors.
func (s *Shipper) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	targets := make([]*target, 0, len(s.targets))
	for _, t := range s.targets {
		t.halt()
		targets = append(targets, t)
	}
	s.mu.Unlock()
	close(s.stop)
	<-s.done
	s.saveCursorsOf(targets) // where each stopped
	s.mu.Lock()
	clear(s.targets)
	s.mu.Unlock()
}

// Flush saves the cursors now (before a reboot).
func (s *Shipper) Flush() { s.saveCursors() }

// start starts a target where prev (the same target, changed) stopped,
// else where its saved cursors say, else now.
func (s *Shipper) start(cfg Target, prev *target) *target {
	t := &target{
		s: s, cfg: cfg, mem: &queue{}, wake: make(chan struct{}, 1),
		stop: make(chan struct{}), done: make(chan struct{}),
		feedCursor: map[string]uint64{}, state: "starting",
	}
	if prev != nil {
		prev.cmu.Lock()
		t.volCursor, t.volMoved = prev.volCursor, true
		for k, v := range prev.feedCursor {
			t.feedCursor[k] = v
		}
		prev.cmu.Unlock()
		for _, f := range s.opt.Feeds {
			if _, ok := t.feedCursor[f.Name]; !ok {
				t.feedCursor[f.Name] = f.Latest()
			}
		}
		go t.run()
		return t
	}
	if v := s.opt.Volume; v != nil {
		var vc volCursors
		if b := v.LoadCursor(); b != nil {
			_ = json.Unmarshal(b, &vc)
		}
		if seq, ok := vc.Targets[cfg.Name]; ok && seq <= v.LastSeq() {
			t.volCursor = seq
		} else {
			t.volCursor = v.LastSeq()
		}
	}
	s.cursorsMu.Lock()
	for _, f := range s.opt.Feeds {
		if id, ok := s.cursors.Feeds[f.Name][cfg.Name]; ok && id <= f.Latest() {
			t.feedCursor[f.Name] = id
		} else {
			t.feedCursor[f.Name] = f.Latest()
		}
	}
	s.cursorsMu.Unlock()
	go t.run()
	return t
}

// forget drops a removed target's cursors.
func (s *Shipper) forget(name string) {
	s.cursorsMu.Lock()
	for _, m := range s.cursors.Feeds {
		delete(m, name)
	}
	s.dirty = true
	if s.volForgotten == nil {
		s.volForgotten = map[string]bool{}
	}
	s.volForgotten[name] = true
	s.cursorsMu.Unlock()
}

func (s *Shipper) saveLoop() {
	defer close(s.done)
	tick := time.NewTicker(saveEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			s.saveCursors()
		case <-s.stop:
			s.saveCursors()
			return
		}
	}
}

// saveCursors writes the running targets' cursors where they're kept,
// when they moved.
func (s *Shipper) saveCursors() {
	s.mu.Lock()
	targets := make([]*target, 0, len(s.targets))
	for _, t := range s.targets {
		targets = append(targets, t)
	}
	s.mu.Unlock()
	s.saveCursorsOf(targets)
}

func (s *Shipper) saveCursorsOf(targets []*target) {
	s.cursorsMu.Lock()
	none := len(targets) == 0 && len(s.volForgotten) == 0 && !s.dirty
	s.cursorsMu.Unlock()
	if none {
		return
	}

	vc := volCursors{Targets: map[string]uint64{}}
	moved := false
	s.cursorsMu.Lock()
	forgotten := s.volForgotten
	s.volForgotten = nil
	if s.cursors.Feeds == nil {
		s.cursors.Feeds = map[string]map[string]uint64{}
	}
	for _, t := range targets {
		t.cmu.Lock()
		vc.Targets[t.cfg.Name] = t.volCursor
		moved = moved || t.volMoved
		t.volMoved = false
		for feed, id := range t.feedCursor {
			m := s.cursors.Feeds[feed]
			if m == nil {
				m = map[string]uint64{}
				s.cursors.Feeds[feed] = m
			}
			if m[t.cfg.Name] != id {
				m[t.cfg.Name] = id
				s.dirty = true
			}
		}
		t.cmu.Unlock()
	}
	dirty := s.dirty
	s.dirty = false
	data, _ := json.Marshal(s.cursors)
	s.cursorsMu.Unlock()

	if v := s.opt.Volume; v != nil && (moved || len(forgotten) > 0) {
		// The slot holds every target's: the others' stay.
		var saved volCursors
		if b := v.LoadCursor(); b != nil {
			_ = json.Unmarshal(b, &saved)
		}
		for name, seq := range saved.Targets {
			if _, ok := vc.Targets[name]; !ok && !forgotten[name] {
				vc.Targets[name] = seq
			}
		}
		if b, err := json.Marshal(vc); err == nil && len(b) <= logvol.MaxCursor {
			if err := v.SaveCursor(b); err != nil {
				log.Printf("logship: save the cursors on the LOGS partition: %v", err)
			}
		}
	}
	if s.opt.CursorFile != "" && dirty {
		if err := writeFileAtomic(s.opt.CursorFile, data, 0o600); err != nil {
			log.Printf("logship: save the cursors: %v", err)
		}
	}
}

// writeFileAtomic writes data at path through a synced temporary file
// renamed over it, then syncs the directory.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path))
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// TargetStatus is how a target is doing.
type TargetStatus struct {
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	State         string    `json:"state"` // starting, ok, failing, disabled
	LastError     string    `json:"last_error,omitempty"`
	LastSent      time.Time `json:"last_sent,omitzero"`
	FailingSince  time.Time `json:"failing_since,omitzero"`
	Sent          uint64    `json:"sent"`
	Dropped       uint64    `json:"dropped"`
	Errors        uint64    `json:"errors"`
	QueuedRecords int       `json:"queued_records"`
}

// Status is every configured target's.
func (s *Shipper) Status() []TargetStatus {
	s.mu.Lock()
	cfg := s.cfg
	running := map[string]*target{}
	for k, v := range s.targets {
		running[k] = v
	}
	s.mu.Unlock()
	var out []TargetStatus
	for _, c := range cfg.Targets {
		st := TargetStatus{Name: c.Name, Kind: c.Kind(), State: "disabled"}
		if t, ok := running[c.Name]; ok {
			st = t.status()
		}
		out = append(out, st)
	}
	return out
}

// Test sends one record to t now - without the configured targets - and
// says what the server answered.
func (s *Shipper) Test(ctx context.Context, t Target) error {
	if err := t.validate(); err != nil {
		return err
	}
	sk, err := s.newSink(t)
	if err != nil {
		return err
	}
	defer sk.Close()
	host := s.opt.Meta.hostname()
	return sk.Send(ctx, []Record{{
		Time: time.Now(), Source: SourceEvents, Service: s.opt.Meta.Agent, Severity: SevNotice,
		Message: fmt.Sprintf("log forwarding test from %s to target %s", host, t.Name),
		Fields:  map[string]any{"event": map[string]any{"type": "logship.test"}},
	}})
}

func (s *Shipper) newSink(t Target) (sink, error) {
	e := &env{meta: &s.opt.Meta, labels: func() map[string]string {
		if p := s.labels.Load(); p != nil {
			return *p
		}
		return nil
	}, nodeID: func() string {
		if p := s.nodeID.Load(); p != nil {
			return *p
		}
		return ""
	}}
	if t.Elasticsearch != nil {
		return newElasticSink(t, e)
	}
	return newSyslogSink(t, e)
}

// target is one target at work.
type target struct {
	s    *Shipper
	cfg  Target
	mem  *queue
	wake chan struct{}
	stop chan struct{}
	done chan struct{}

	cmu        sync.Mutex
	volCursor  uint64
	volMoved   bool
	feedCursor map[string]uint64

	smu          sync.Mutex
	state        string
	lastErr      string
	lastSent     time.Time
	failingSince time.Time
	sent, errs   uint64
}

func (t *target) poke() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *target) halt() {
	close(t.stop)
	<-t.done
}

func (t *target) status() TargetStatus {
	t.smu.Lock()
	defer t.smu.Unlock()
	return TargetStatus{
		Name: t.cfg.Name, Kind: t.cfg.Kind(), State: t.state, LastError: t.lastErr, LastSent: t.lastSent,
		FailingSince: t.failingSince, Sent: t.sent, Dropped: t.mem.droppedCount(), Errors: t.errs,
		QueuedRecords: t.mem.len(),
	}
}

func (t *target) run() {
	defer close(t.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-t.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	sk, err := t.s.newSink(t.cfg)
	if err != nil {
		t.failed(err)
		<-t.stop
		return
	}
	defer sk.Close()
	backoff := time.Second
	for {
		var waitVol <-chan struct{}
		if v := t.s.opt.Volume; v != nil {
			waitVol = v.Written()
		}
		var waitFeeds []<-chan struct{}
		for _, f := range t.s.opt.Feeds {
			if t.cfg.Takes(f.Name) {
				waitFeeds = append(waitFeeds, f.Changed())
			}
		}
		b := t.collect()
		if len(b.recs) == 0 {
			if !t.wait(waitVol, waitFeeds) {
				return
			}
			continue
		}
		sendCtx, sendCancel := context.WithTimeout(ctx, 30*time.Second)
		err := sk.Send(sendCtx, b.recs)
		sendCancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			t.failed(err)
			select {
			case <-time.After(backoff):
			case <-t.stop:
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = time.Second
		b.commit()
		t.succeeded(len(b.recs))
	}
}

// wait waits for something new to send: the volume written, a feed
// changed, a record offered - or a minute, or the end.
func (t *target) wait(vol <-chan struct{}, feeds []<-chan struct{}) bool {
	woke := make(chan struct{}, 1)
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	for _, ch := range append(feeds, vol) {
		if ch == nil {
			continue
		}
		go func(ch <-chan struct{}) {
			select {
			case <-ch:
				select {
				case woke <- struct{}{}:
				default:
				}
			case <-stopWatch:
			}
		}(ch)
	}
	select {
	case <-woke:
	case <-t.wake:
	case <-time.After(time.Minute):
	case <-t.stop:
		return false
	}
	return true
}

func (t *target) failed(err error) {
	t.smu.Lock()
	t.errs++
	t.lastErr = err.Error()
	now := time.Now()
	if t.failingSince.IsZero() {
		t.failingSince = now
	}
	tell := t.state != "failing" && now.Sub(t.failingSince) >= failingAfter
	if tell {
		t.state = "failing"
	}
	msg := t.lastErr
	t.smu.Unlock()
	if tell {
		log.Printf("logship: target %s is failing: %s", t.cfg.Name, msg)
		if t.s.opt.OnState != nil {
			t.s.opt.OnState(t.cfg.Name, "failing", msg)
		}
	}
}

func (t *target) succeeded(n int) {
	t.smu.Lock()
	recovered := t.state == "failing"
	t.state, t.lastErr, t.failingSince = "ok", "", time.Time{}
	t.lastSent = time.Now()
	t.sent += uint64(n)
	t.smu.Unlock()
	if recovered {
		log.Printf("logship: target %s works again", t.cfg.Name)
		if t.s.opt.OnState != nil {
			t.s.opt.OnState(t.cfg.Name, "ok", "")
		}
	}
}

// batch is what one send carries, and how to move the cursors past it
// once it's sent.
type batch struct {
	recs   []Record
	commit func()
}

// collect gathers what the target hasn't sent: from the volume behind
// its cursor, from each feed behind its own, from memory - without
// moving anything until commit.
func (t *target) collect() batch {
	var recs []Record
	size := 0
	full := func() bool { return len(recs) >= batchRecords || size >= batchBytes }
	add := func(r Record) {
		recs = append(recs, r)
		size += len(r.Message) + 200
	}

	t.cmu.Lock()
	volFrom := t.volCursor
	feedFrom := map[string]uint64{}
	for k, v := range t.feedCursor {
		feedFrom[k] = v
	}
	t.cmu.Unlock()

	volTo := volFrom
	if v := t.s.opt.Volume; v != nil && t.takesDisk() {
		last, err := v.ReadFrom(volFrom, func(r logvol.Record) bool {
			if full() {
				return false
			}
			if rec, ok := t.s.fromDisk(r); ok && t.cfg.Takes(rec.Source) {
				add(rec)
			}
			return true
		})
		if err == nil {
			volTo = last
		}
	}
	feedTo := map[string]uint64{}
	for _, f := range t.s.opt.Feeds {
		if !t.cfg.Takes(f.Name) || full() {
			continue
		}
		items, _ := f.Since(feedFrom[f.Name])
		to := feedFrom[f.Name]
		for _, it := range items {
			if full() {
				break
			}
			add(it.Record)
			to = it.ID
		}
		feedTo[f.Name] = to
	}
	var memEnd uint64
	if !full() {
		var mem []Record
		mem, memEnd = t.mem.peek(batchRecords-len(recs), batchBytes-size)
		for _, r := range mem {
			add(r)
		}
	}
	commit := func() {
		t.cmu.Lock()
		if volTo != t.volCursor {
			t.volCursor, t.volMoved = volTo, true
		}
		for k, v := range feedTo {
			t.feedCursor[k] = v
		}
		t.cmu.Unlock()
		t.mem.pop(memEnd)
	}
	if len(recs) == 0 && volTo != volFrom {
		// Only records this target doesn't take: past them all the same.
		commit()
	}
	return batch{recs: recs, commit: commit}
}

func (t *target) takesDisk() bool {
	for _, src := range []string{SourceJanusd, SourceHAProxy, SourceAccess, SourceExtensions, SourceKernel} {
		if t.cfg.Takes(src) && t.s.onDisk(src) {
			return true
		}
	}
	return false
}

// fromDisk turns a LOGS record into one to send: classified, and with
// an ID unique to the volume.
func (s *Shipper) fromDisk(r logvol.Record) (Record, bool) {
	rec := Record{Time: r.Time, Service: r.Service}
	switch r.Service {
	case AccessService:
		rec.Source, rec.Service, rec.Severity, rec.Message = SourceAccess, "haproxy", priorityOr(r.Message, SevInfo), r.Message
	case "kernel":
		rec.Source, rec.Severity, rec.Message = SourceKernel, int(r.Severity), r.Message
	default:
		rec.Source, rec.Severity, rec.Message = Classify(r.Service, r.Message)
	}
	if !s.onDisk(rec.Source) {
		return Record{}, false
	}
	id := s.opt.Volume.ID()
	rec.ID = fmt.Sprintf("%x-%d", id[:8], r.Seq)
	return rec, true
}

// AccessService is the service name HAProxy's access logs are kept under
// on the LOGS partition (access_logs_on_disk "always"), apart from its
// own messages.
const AccessService = "haproxy-access"

// queue is a target's records waiting in memory, numbered so a batch
// taken from it can be removed once sent even if older ones were
// dropped meanwhile.
type queue struct {
	mu      sync.Mutex
	recs    []Record
	base    uint64 // the number of recs[0]
	bytes   int
	dropped uint64
}

func (q *queue) push(r Record) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.recs = append(q.recs, r)
	q.bytes += len(r.Message) + 200
	for len(q.recs) > queueRecords || q.bytes > queueBytes {
		q.bytes -= len(q.recs[0].Message) + 200
		q.recs[0] = Record{}
		q.recs = q.recs[1:]
		q.base++
		q.dropped++
	}
}

// peek returns up to n records (size bytes) from the oldest, and the
// number past the last one.
func (q *queue) peek(n, size int) ([]Record, uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Record
	b := 0
	for _, r := range q.recs {
		if len(out) >= n || b >= size {
			break
		}
		out = append(out, r)
		b += len(r.Message) + 200
	}
	return out, q.base + uint64(len(out))
}

// pop removes the records numbered below end.
func (q *queue) pop(end uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.base < end && len(q.recs) > 0 {
		q.bytes -= len(q.recs[0].Message) + 200
		q.recs[0] = Record{}
		q.recs = q.recs[1:]
		q.base++
	}
	if len(q.recs) == 0 {
		q.recs = nil
	}
}

func (q *queue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.recs)
}

func (q *queue) droppedCount() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// errNoElastic is until the Elasticsearch sink is there.
var errNoElastic = errors.New("elasticsearch isn't supported yet")

func newElasticSink(Target, *env) (sink, error) { return nil, errNoElastic }
