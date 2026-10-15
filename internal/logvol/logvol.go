// Package logvol keeps a node's logs on its LOGS partition, across
// reboots: what each service printed and the kernel's messages, boot by
// boot, until newer ones take the room.
//
// The partition is raw - no filesystem, so nothing to format, check or
// mount on a node that has no mkfs - and the file API can't read it:
// SystemService.Logs and Dmesg serve it. Its layout, all integers little
// endian:
//
//   - The first MiB is the volume's own: a superblock (block 0: magic,
//     version, sizes, the volume's UUID, a CRC32C) and two cursor slots
//     (blocks 1 and 2, written in turn) for whoever reads it in order -
//     the log shipper.
//   - The rest is a ring of 1 MiB segments. A segment holds one boot's
//     records: a header block (magic, the volume's UUID, a sequence
//     number that only grows, the boot's number and the kernel's
//     boot_id, its first record's sequence number, a CRC32C), then
//     records back to back until the next wouldn't fit. A boot starts a new segment, so the headers alone
//     say which boots the volume holds.
//   - A record: total length, a CRC32C over the segment's sequence
//     number and the rest of the record, its own sequence number, time,
//     severity, service and message. A length of 0 ends a segment; a
//     record whose CRC doesn't match - a write the power cut, or one
//     left from the segment's previous turn, whose sequence number was
//     another - ends it too.
//
// Records are queued in memory and written by one goroutine every
// FlushEvery (or once FlushBytes are waiting), each write followed by
// a sync: a power cut loses at most the last few seconds. Append
// never waits for the disk: past MaxQueued bytes waiting, a record is
// dropped and counted.
package logvol

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	BlockSize   = 4096
	SegmentSize = 1 << 20

	// FlushEvery and FlushBytes say when queued records are written.
	FlushEvery = 2 * time.Second
	FlushBytes = 64 << 10
	// MaxQueued bytes may wait for the disk; past that, records are
	// dropped.
	MaxQueued = 4 << 20

	// MaxService and MaxMessage cut what a record keeps.
	MaxService = 64
	MaxMessage = 16 << 10

	// minSegments is the smallest ring: a boot's records and the one
	// before it.
	minSegments = 2

	superMagic   = "JANUSLV1"
	slotMagic    = "JLVSLOT1"
	segmentMagic = "JLVSEG01"
	version      = 1

	recordHeader = 28
)

// SeverityUnknown is a record's severity when the source didn't say.
const SeverityUnknown = 0xff

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Record is one line of a service's output or one kernel message.
type Record struct {
	Seq      uint64
	Time     time.Time
	Severity uint8 // syslog's 0 (emergency) to 7 (debug), or SeverityUnknown
	Service  string
	Message  string
}

// Boot is one boot the volume holds records of.
type Boot struct {
	Number   uint32
	BootID   [16]byte  // the kernel's boot_id
	Started  time.Time // its first segment's
	Segments int
}

// Stats are a volume's counters.
type Stats struct {
	SizeBytes    int64
	WrittenBytes uint64
	Records      uint64
	Dropped      uint64
	Errors       uint64
}

// Options are what Open needs besides the device.
type Options struct {
	// BootID is the kernel's boot_id (/proc/sys/kernel/random/boot_id).
	BootID [16]byte
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

type superblock struct {
	segments uint32
	uuid     [16]byte
	created  int64
}

type segHeader struct {
	valid    bool
	seq      uint64
	boot     uint32
	bootID   [16]byte
	created  int64
	firstRec uint64 // its first record's sequence number
}

// Volume is an open LOGS partition.
type Volume struct {
	f    *os.File
	size int64
	sb   superblock
	now  func() time.Time

	boot   uint32
	bootID [16]byte

	mu      sync.Mutex
	queue   []Record
	queued  int
	nextSeq uint64
	closed  bool
	kick    chan struct{}
	done    chan struct{}
	// flushed is closed (and replaced) after each write that reached
	// the disk: Written.
	flushed chan struct{}
	// headers is the index of every segment, kept current as segments
	// are written.
	headers []segHeader

	// The writer's state, owned by flush (under wmu).
	wmu       sync.Mutex
	seg       int    // current segment index, -1 before the first
	segSeq    uint64 // its sequence number
	segBuf    []byte // its image
	off       int    // where the next record goes in it
	dirtyFrom int    // first byte not on disk yet

	written atomic.Uint64
	records atomic.Uint64
	dropped atomic.Uint64
	errs    atomic.Uint64
}

// Open opens the LOGS partition at path for a new boot - formatting it
// first when it holds no volume (a blank partition, another volume's
// leftovers) - and starts its writer.
func Open(path string, opt Options) (*Volume, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	v, err := open(f, opt)
	if err != nil {
		f.Close()
		return nil, err
	}
	go v.run()
	return v, nil
}

func open(f *os.File, opt Options) (*Volume, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("size: %w", err)
	}
	segments := size/SegmentSize - 1
	if segments < minSegments {
		return nil, fmt.Errorf("%d bytes is too small a LOGS volume (%d MiB at least)", size, (minSegments+1)*SegmentSize>>20)
	}
	v := &Volume{
		f: f, size: size, now: opt.Now, bootID: opt.BootID, seg: -1,
		kick: make(chan struct{}, 1), done: make(chan struct{}),
	}
	if v.now == nil {
		v.now = time.Now
	}
	sb, ok := v.readSuper()
	if !ok || sb.segments != uint32(segments) {
		if sb, err = v.format(uint32(segments)); err != nil {
			return nil, err
		}
	}
	v.sb = sb
	v.headers = make([]segHeader, segments)
	var last uint64
	var newest segHeader
	for i := range v.headers {
		h := v.readHeader(i)
		v.headers[i] = h
		if h.valid {
			v.boot = max(v.boot, h.boot)
			last = max(last, h.seq)
			if h.seq == last {
				newest = h
			}
		}
	}
	// janusd restarted, not the machine: the same boot goes on.
	if !newest.valid || newest.bootID != opt.BootID || opt.BootID == ([16]byte{}) {
		v.boot++
	}
	v.segSeq = last
	v.nextSeq = v.lastRecordSeq() + 1
	return v, nil
}

// format writes a new superblock - a new UUID, so nothing written under
// the previous one counts any more - and empty cursor slots.
func (v *Volume) format(segments uint32) (superblock, error) {
	sb, err := formatAt(v.f, 0, segments, v.now())
	if err != nil {
		return sb, fmt.Errorf("format: %w", err)
	}
	if err := v.f.Sync(); err != nil {
		return sb, fmt.Errorf("format: %w", err)
	}
	return sb, nil
}

// Format makes a new, empty volume of size bytes at offset off of w -
// what LifecycleService.Install does to the LOGS partition of a disk it
// lays out, so a reused disk's old logs never show.
func Format(w io.WriterAt, off, size int64) error {
	segments := size/SegmentSize - 1
	if segments < minSegments {
		return fmt.Errorf("%d bytes is too small a LOGS volume", size)
	}
	_, err := formatAt(w, off, uint32(segments), time.Now())
	return err
}

func formatAt(w io.WriterAt, off int64, segments uint32, now time.Time) (superblock, error) {
	sb := superblock{segments: segments, created: now.UnixNano()}
	if _, err := rand.Read(sb.uuid[:]); err != nil {
		return sb, err
	}
	meta := make([]byte, 3*BlockSize)
	b := meta[:BlockSize]
	copy(b, superMagic)
	binary.LittleEndian.PutUint32(b[8:], version)
	binary.LittleEndian.PutUint32(b[12:], BlockSize)
	binary.LittleEndian.PutUint32(b[16:], SegmentSize)
	binary.LittleEndian.PutUint32(b[20:], segments)
	copy(b[24:40], sb.uuid[:])
	binary.LittleEndian.PutUint64(b[40:], uint64(sb.created))
	sealBlock(b)
	_, err := w.WriteAt(meta, off)
	return sb, err
}

// sealBlock puts a block's CRC32C in its last 4 bytes.
func sealBlock(b []byte) {
	binary.LittleEndian.PutUint32(b[BlockSize-4:], crc32.Checksum(b[:BlockSize-4], castagnoli))
}

func sealed(b []byte) bool {
	return len(b) == BlockSize && binary.LittleEndian.Uint32(b[BlockSize-4:]) == crc32.Checksum(b[:BlockSize-4], castagnoli)
}

func (v *Volume) readSuper() (superblock, bool) {
	b := make([]byte, BlockSize)
	if _, err := v.f.ReadAt(b, 0); err != nil || !sealed(b) || string(b[:8]) != superMagic ||
		binary.LittleEndian.Uint32(b[8:]) != version || binary.LittleEndian.Uint32(b[12:]) != BlockSize ||
		binary.LittleEndian.Uint32(b[16:]) != SegmentSize {
		return superblock{}, false
	}
	var sb superblock
	sb.segments = binary.LittleEndian.Uint32(b[20:])
	copy(sb.uuid[:], b[24:40])
	sb.created = int64(binary.LittleEndian.Uint64(b[40:]))
	return sb, true
}

func segOffset(i int) int64 { return int64(i+1) * SegmentSize }

func (v *Volume) readHeader(i int) segHeader {
	b := make([]byte, BlockSize)
	if _, err := v.f.ReadAt(b, segOffset(i)); err != nil {
		return segHeader{}
	}
	return parseHeader(b, v.sb.uuid)
}

func parseHeader(b []byte, uuid [16]byte) segHeader {
	if !sealed(b) || string(b[:8]) != segmentMagic || !bytes.Equal(b[8:24], uuid[:]) {
		return segHeader{}
	}
	h := segHeader{valid: true}
	h.seq = binary.LittleEndian.Uint64(b[24:])
	h.boot = binary.LittleEndian.Uint32(b[32:])
	copy(h.bootID[:], b[36:52])
	h.created = int64(binary.LittleEndian.Uint64(b[52:]))
	h.firstRec = binary.LittleEndian.Uint64(b[60:])
	return h
}

func (v *Volume) header(h segHeader) []byte {
	b := make([]byte, BlockSize)
	copy(b, segmentMagic)
	copy(b[8:24], v.sb.uuid[:])
	binary.LittleEndian.PutUint64(b[24:], h.seq)
	binary.LittleEndian.PutUint32(b[32:], h.boot)
	copy(b[36:52], h.bootID[:])
	binary.LittleEndian.PutUint64(b[52:], uint64(h.created))
	binary.LittleEndian.PutUint64(b[60:], h.firstRec)
	sealBlock(b)
	return b
}

// lastRecordSeq is the newest record's sequence number on the volume
// (0 when it holds none), so the numbers go on across boots.
func (v *Volume) lastRecordSeq() uint64 {
	newest := -1
	for i, h := range v.headers {
		if h.valid && (newest < 0 || h.seq > v.headers[newest].seq) {
			newest = i
		}
	}
	var last uint64
	for newest >= 0 {
		seg := make([]byte, SegmentSize)
		if _, err := v.f.ReadAt(seg, segOffset(newest)); err != nil {
			return 0
		}
		parseRecords(seg, v.headers[newest].seq, func(r Record) bool { last = r.Seq; return true })
		if last > 0 {
			return last
		}
		// An empty newest segment (a boot that wrote its header and
		// nothing more): the one before it.
		prev := -1
		for i, h := range v.headers {
			if h.valid && h.seq < v.headers[newest].seq && (prev < 0 || h.seq > v.headers[prev].seq) {
				prev = i
			}
		}
		newest = prev
	}
	return last
}

// encodeRecord appends r to dst as it's laid on disk in a segment of
// sequence number segSeq.
func encodeRecord(dst []byte, r Record, segSeq uint64) []byte {
	service := r.Service
	if len(service) > MaxService {
		service = service[:MaxService]
	}
	message := r.Message
	if len(message) > MaxMessage {
		message = message[:MaxMessage]
	}
	n := recordHeader + len(service) + len(message)
	start := len(dst)
	dst = append(dst, make([]byte, recordHeader)...)
	b := dst[start:]
	binary.LittleEndian.PutUint32(b[0:], uint32(n))
	binary.LittleEndian.PutUint64(b[8:], r.Seq)
	binary.LittleEndian.PutUint64(b[16:], uint64(r.Time.UnixNano()))
	b[24] = r.Severity
	b[25] = uint8(len(service))
	binary.LittleEndian.PutUint16(b[26:], uint16(len(message)))
	dst = append(dst, service...)
	dst = append(dst, message...)
	binary.LittleEndian.PutUint32(dst[start+4:], recordCRC(dst[start+8:start+n], segSeq))
	return dst
}

func recordCRC(body []byte, segSeq uint64) uint32 {
	var s [8]byte
	binary.LittleEndian.PutUint64(s[:], segSeq)
	return crc32.Update(crc32.Checksum(s[:], castagnoli), castagnoli, body)
}

// parseRecords calls fn for each valid record of a segment image, in
// order, until the segment ends, a record doesn't check out, or fn
// returns false.
func parseRecords(seg []byte, segSeq uint64, fn func(Record) bool) {
	off := BlockSize
	for off+recordHeader <= len(seg) {
		b := seg[off:]
		n := int(binary.LittleEndian.Uint32(b))
		if n < recordHeader || n > len(b) {
			return
		}
		if binary.LittleEndian.Uint32(b[4:]) != recordCRC(b[8:n], segSeq) {
			return
		}
		sl, ml := int(b[25]), int(binary.LittleEndian.Uint16(b[26:]))
		if recordHeader+sl+ml != n {
			return
		}
		r := Record{
			Seq:      binary.LittleEndian.Uint64(b[8:]),
			Time:     time.Unix(0, int64(binary.LittleEndian.Uint64(b[16:]))),
			Severity: b[24],
			Service:  string(b[recordHeader : recordHeader+sl]),
			Message:  string(b[recordHeader+sl : n]),
		}
		if !fn(r) {
			return
		}
		off += n
	}
}

// Append queues a record of this boot; Seq is set here, Time too when
// zero. It never waits for the disk.
func (v *Volume) Append(r Record) {
	if r.Time.IsZero() {
		r.Time = v.now()
	}
	size := recordHeader + min(len(r.Service), MaxService) + min(len(r.Message), MaxMessage)
	v.mu.Lock()
	if v.closed || v.queued+size > MaxQueued {
		v.mu.Unlock()
		v.dropped.Add(1)
		return
	}
	r.Seq = v.nextSeq
	v.nextSeq++
	v.queue = append(v.queue, r)
	v.queued += size
	kick := v.queued >= FlushBytes
	v.mu.Unlock()
	if kick {
		select {
		case v.kick <- struct{}{}:
		default:
		}
	}
}

// Line returns a func appending each line it's given as a record of
// service, for ring.LineWriter's Tee.
func (v *Volume) Line(service string) func(string) {
	return func(line string) {
		v.Append(Record{Service: service, Severity: SeverityUnknown, Message: line})
	}
}

func (v *Volume) run() {
	t := time.NewTicker(FlushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-v.kick:
		case <-v.done:
			return
		}
		if err := v.Flush(); err != nil {
			v.errs.Add(1)
		}
	}
}

// Flush writes every queued record now, and syncs.
func (v *Volume) Flush() error {
	v.wmu.Lock()
	defer v.wmu.Unlock()
	v.mu.Lock()
	queue := v.queue
	v.queue, v.queued = nil, 0
	closed := v.closed
	v.mu.Unlock()
	if closed || len(queue) == 0 {
		return nil
	}
	for _, r := range queue {
		if v.seg < 0 {
			v.startSegment(r.Seq)
		}
		rec := encodeRecord(nil, r, v.segSeq)
		if v.off+len(rec) > SegmentSize {
			if err := v.writeDirty(); err != nil {
				return err
			}
			v.startSegment(r.Seq)
			rec = encodeRecord(rec[:0], r, v.segSeq) // its CRC covers the segment's number
		}
		copy(v.segBuf[v.off:], rec)
		v.off += len(rec)
		v.records.Add(1)
	}
	return v.writeDirty()
}

// startSegment moves to the next segment of the ring - over the oldest
// one - with this boot's header; firstRec is the record that goes first.
func (v *Volume) startSegment(firstRec uint64) {
	if v.seg < 0 {
		// After the newest segment on the volume.
		newest, seq := -1, uint64(0)
		for i, h := range v.headers {
			if h.valid && h.seq >= seq {
				newest, seq = i, h.seq
			}
		}
		v.seg = newest
	}
	v.seg = (v.seg + 1) % len(v.headers)
	v.segSeq++
	h := segHeader{valid: true, seq: v.segSeq, boot: v.boot, bootID: v.bootID, created: v.now().UnixNano(), firstRec: firstRec}
	if v.segBuf == nil {
		v.segBuf = make([]byte, SegmentSize)
	} else {
		clear(v.segBuf)
	}
	copy(v.segBuf, v.header(h))
	v.off, v.dirtyFrom = BlockSize, 0
	v.mu.Lock()
	v.headers[v.seg] = h
	v.mu.Unlock()
}

// writeDirty writes the current segment's blocks that changed since the
// last write, and syncs.
func (v *Volume) writeDirty() error {
	if v.seg < 0 || v.dirtyFrom >= v.off {
		return nil
	}
	from := v.dirtyFrom / BlockSize * BlockSize
	to := min(SegmentSize, (v.off+BlockSize-1)/BlockSize*BlockSize)
	if _, err := v.f.WriteAt(v.segBuf[from:to], segOffset(v.seg)+int64(from)); err != nil {
		return err
	}
	if err := v.f.Sync(); err != nil {
		return err
	}
	v.written.Add(uint64(to - from))
	v.dirtyFrom = v.off
	v.mu.Lock()
	if v.flushed != nil {
		close(v.flushed)
		v.flushed = nil
	}
	v.mu.Unlock()
	return nil
}

// Close writes what's queued and stops the writer.
func (v *Volume) Close() error {
	err := v.Flush()
	v.mu.Lock()
	if !v.closed {
		v.closed = true
		close(v.done)
	}
	v.mu.Unlock()
	return errors.Join(err, v.f.Close())
}

// Wipe forgets everything the volume holds - a new UUID - and stops
// writing: what this boot appends from now on is dropped (Reset reboots
// right after).
func (v *Volume) Wipe() error {
	v.wmu.Lock()
	defer v.wmu.Unlock()
	v.mu.Lock()
	v.closed = true
	v.queue, v.queued = nil, 0
	for i := range v.headers {
		v.headers[i] = segHeader{}
	}
	v.mu.Unlock()
	sb, err := v.format(v.sb.segments)
	if err == nil {
		v.sb = sb
	}
	return err
}

// Boots lists the boots the volume holds records of, newest first -
// this one included once its first records are written.
func (v *Volume) Boots() []Boot {
	v.mu.Lock()
	defer v.mu.Unlock()
	byNum := map[uint32]*Boot{}
	for _, h := range v.headers {
		if !h.valid {
			continue
		}
		b, ok := byNum[h.boot]
		if !ok {
			b = &Boot{Number: h.boot, BootID: h.bootID, Started: time.Unix(0, h.created)}
			byNum[h.boot] = b
		}
		b.Segments++
		if t := time.Unix(0, h.created); t.Before(b.Started) {
			b.Started = t
		}
	}
	out := make([]Boot, 0, len(byNum))
	for _, b := range byNum {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
	return out
}

// Current is this boot's number.
func (v *Volume) Current() uint32 { return v.boot }

// LastSeq is the newest record's sequence number, written or still
// queued: a reader starting now starts after it.
func (v *Volume) LastSeq() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.nextSeq - 1
}

// ID is the volume's UUID: record sequence numbers are unique within it
// (a Reset or a reinstall makes a new one).
func (v *Volume) ID() [16]byte {
	v.wmu.Lock()
	defer v.wmu.Unlock()
	return v.sb.uuid
}

// ReadFrom calls fn with every record written to the volume after
// sequence number after, oldest first and across boots, until fn
// returns false - what a reader that keeps its place (the log shipper)
// hasn't seen yet. This boot's records not written yet aren't read.
// It returns the last sequence number it gave fn (after when none).
func (v *Volume) ReadFrom(after uint64, fn func(Record) bool) (uint64, error) {
	v.mu.Lock()
	var segs []segHeader
	var index []int
	for i, h := range v.headers {
		if h.valid {
			segs = append(segs, h)
			index = append(index, i)
		}
	}
	v.mu.Unlock()
	order := make([]int, len(segs))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return segs[order[a]].seq < segs[order[b]].seq })
	last := after
	seg := make([]byte, SegmentSize)
	for k, o := range order {
		// A segment whose successor starts at or before after holds
		// nothing new.
		if k+1 < len(order) && segs[order[k+1]].firstRec != 0 && segs[order[k+1]].firstRec <= after+1 {
			continue
		}
		h := segs[o]
		if _, err := v.f.ReadAt(seg, segOffset(index[o])); err != nil {
			return last, err
		}
		if now := parseHeader(seg[:BlockSize], v.sb.uuid); !now.valid || now.seq != h.seq {
			continue // reused since
		}
		more := true
		parseRecords(seg, h.seq, func(r Record) bool {
			if r.Seq <= after {
				return true
			}
			more = fn(r)
			if more {
				last = r.Seq
			}
			return more
		})
		if !more {
			break
		}
	}
	return last, nil
}

// Written returns a channel closed the next time records reach the disk.
func (v *Volume) Written() <-chan struct{} {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.flushed == nil {
		v.flushed = make(chan struct{})
	}
	return v.flushed
}

// Read calls fn with every record of boot, oldest first, until fn
// returns false. This boot's records not written yet aren't read.
func (v *Volume) Read(boot uint32, fn func(Record) bool) error {
	segs := v.segmentsOf(boot)
	seg := make([]byte, SegmentSize)
	for _, s := range segs {
		if _, err := v.f.ReadAt(seg, segOffset(s.index)); err != nil {
			return err
		}
		// The segment may have been reused since the index was read.
		if h := parseHeader(seg[:BlockSize], v.sb.uuid); !h.valid || h.seq != s.seq {
			continue
		}
		more := true
		parseRecords(seg, s.seq, func(r Record) bool { more = fn(r); return more })
		if !more {
			return nil
		}
	}
	return nil
}

// Tail returns the last n records of boot that keep accepts, oldest
// first - read from its newest segment back.
func (v *Volume) Tail(boot uint32, n int, keep func(Record) bool) ([]Record, error) {
	segs := v.segmentsOf(boot)
	seg := make([]byte, SegmentSize)
	var out []Record
	for i := len(segs) - 1; i >= 0 && (n <= 0 || len(out) < n); i-- {
		s := segs[i]
		if _, err := v.f.ReadAt(seg, segOffset(s.index)); err != nil {
			return nil, err
		}
		if h := parseHeader(seg[:BlockSize], v.sb.uuid); !h.valid || h.seq != s.seq {
			continue
		}
		var these []Record
		parseRecords(seg, s.seq, func(r Record) bool {
			if keep == nil || keep(r) {
				these = append(these, r)
			}
			return true
		})
		out = append(these, out...)
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

type segRef struct {
	index int
	seq   uint64
}

func (v *Volume) segmentsOf(boot uint32) []segRef {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []segRef
	for i, h := range v.headers {
		if h.valid && h.boot == boot {
			out = append(out, segRef{i, h.seq})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// Stats returns the volume's counters.
func (v *Volume) Stats() Stats {
	return Stats{
		SizeBytes: v.size, WrittenBytes: v.written.Load(), Records: v.records.Load(),
		Dropped: v.dropped.Load(), Errors: v.errs.Load(),
	}
}

// LoadCursor returns the newest cursor saved with SaveCursor, nil when
// none.
func (v *Volume) LoadCursor() []byte {
	var best []byte
	var bestGen uint64
	for i := 1; i <= 2; i++ {
		if gen, data, ok := v.readSlot(i); ok && (best == nil || gen > bestGen) {
			best, bestGen = data, gen
		}
	}
	return best
}

// MaxCursor is the most SaveCursor keeps.
const MaxCursor = BlockSize - 40

// SaveCursor saves data over the older of the two slots, so a power cut
// during the write leaves the previous cursor.
func (v *Volume) SaveCursor(data []byte) error {
	if len(data) > MaxCursor {
		return fmt.Errorf("cursor of %d bytes: %d at most", len(data), MaxCursor)
	}
	g1, _, ok1 := v.readSlot(1)
	g2, _, ok2 := v.readSlot(2)
	slot, gen := 1, uint64(1)
	switch {
	case ok1 && ok2 && g1 <= g2:
		slot, gen = 1, g2+1
	case ok1 && ok2:
		slot, gen = 2, g1+1
	case ok1:
		slot, gen = 2, g1+1
	case ok2:
		slot, gen = 1, g2+1
	}
	b := make([]byte, BlockSize)
	copy(b, slotMagic)
	copy(b[8:24], v.sb.uuid[:])
	binary.LittleEndian.PutUint64(b[24:], gen)
	binary.LittleEndian.PutUint32(b[32:], uint32(len(data)))
	copy(b[36:], data)
	sealBlock(b)
	if _, err := v.f.WriteAt(b, int64(slot)*BlockSize); err != nil {
		return err
	}
	return v.f.Sync()
}

func (v *Volume) readSlot(i int) (uint64, []byte, bool) {
	b := make([]byte, BlockSize)
	if _, err := v.f.ReadAt(b, int64(i)*BlockSize); err != nil || !sealed(b) ||
		string(b[:8]) != slotMagic || !bytes.Equal(b[8:24], v.sb.uuid[:]) {
		return 0, nil, false
	}
	n := int(binary.LittleEndian.Uint32(b[32:]))
	if n > MaxCursor {
		return 0, nil, false
	}
	return binary.LittleEndian.Uint64(b[24:]), append([]byte(nil), b[36:36+n]...), true
}
