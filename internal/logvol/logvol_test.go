package logvol

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// volumeFile makes a blank volume of mib MiB.
func volumeFile(t *testing.T, mib int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "logs.img")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, int64(mib)<<20); err != nil {
		t.Fatal(err)
	}
	return path
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Millisecond); return c.t }

// boot opens path for one more boot, without the background writer.
func boot(t *testing.T, path string, c *clock, id byte) *Volume {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	v, err := open(f, Options{BootID: [16]byte{id}, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return v
}

func messages(t *testing.T, v *Volume, boot uint32) []string {
	t.Helper()
	var out []string
	if err := v.Read(boot, func(r Record) bool { out = append(out, r.Service+": "+r.Message); return true }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBootsAcrossReopens(t *testing.T) {
	path := volumeFile(t, 8)
	c := &clock{time.Unix(1_800_000_000, 0)}

	v := boot(t, path, c, 1)
	if v.Current() != 1 || len(v.Boots()) != 0 {
		t.Fatalf("blank volume: boot %d, boots %v", v.Current(), v.Boots())
	}
	v.Append(Record{Service: "janusd", Severity: SeverityUnknown, Message: "first boot"})
	v.Line("haproxy")("[NOTICE] started")
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}

	v = boot(t, path, c, 2)
	if v.Current() != 2 {
		t.Fatalf("second boot is %d", v.Current())
	}
	v.Append(Record{Service: "kernel", Severity: 6, Message: "[    0.000000] Linux version"})
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}

	v = boot(t, path, c, 3)
	boots := v.Boots()
	if len(boots) != 2 || boots[0].Number != 2 || boots[1].Number != 1 || boots[0].BootID[0] != 2 || boots[1].BootID[0] != 1 {
		t.Fatalf("boots = %+v", boots)
	}
	if got := messages(t, v, 1); strings.Join(got, "|") != "janusd: first boot|haproxy: [NOTICE] started" {
		t.Fatalf("boot 1 = %q", got)
	}
	var recs []Record
	_ = v.Read(2, func(r Record) bool { recs = append(recs, r); return true })
	if len(recs) != 1 || recs[0].Severity != 6 || recs[0].Seq != 3 {
		t.Fatalf("boot 2 = %+v (sequence numbers go on across boots)", recs)
	}
}

func TestTheRingWrapsOverTheOldest(t *testing.T) {
	path := volumeFile(t, 4) // 3 segments
	c := &clock{time.Unix(1_800_000_000, 0)}
	msg := strings.Repeat("x", 1000)
	// Five boots of about half a segment each: the ring keeps the last
	// three.
	for b := 1; b <= 5; b++ {
		v := boot(t, path, c, byte(b))
		for i := range 500 {
			v.Append(Record{Service: "janusd", Message: fmt.Sprintf("boot %d line %d %s", b, i, msg)})
		}
		if err := v.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	v := boot(t, path, c, 6)
	var nums []uint32
	for _, b := range v.Boots() {
		nums = append(nums, b.Number)
	}
	if fmt.Sprint(nums) != "[5 4 3]" {
		t.Fatalf("boots kept = %v, want [5 4 3]", nums)
	}
	got := messages(t, v, 5)
	if len(got) != 500 || !strings.HasPrefix(got[0], "janusd: boot 5 line 0 ") || !strings.HasPrefix(got[499], "janusd: boot 5 line 499 ") {
		t.Fatalf("boot 5: %d records, first %.40q", len(got), got[0])
	}
}

func TestABootLongerThanASegment(t *testing.T) {
	path := volumeFile(t, 6)
	c := &clock{time.Unix(1_800_000_000, 0)}
	v := boot(t, path, c, 1)
	msg := strings.Repeat("y", 4000)
	for i := range 700 { // ~2.7 MiB: three segments
		v.Append(Record{Service: "haproxy", Message: fmt.Sprintf("%d %s", i, msg)})
		if i%100 == 0 {
			if err := v.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}
	v = boot(t, path, c, 2)
	if b := v.Boots(); len(b) != 1 || b[0].Segments != 3 {
		t.Fatalf("boots = %+v", b)
	}
	got := messages(t, v, 1)
	if len(got) != 700 {
		t.Fatalf("%d records, want 700", len(got))
	}
	for i, m := range got {
		if !strings.HasPrefix(m, fmt.Sprintf("haproxy: %d ", i)) {
			t.Fatalf("record %d = %.30q", i, m)
		}
	}
	tail, err := v.Tail(1, 3, nil)
	if err != nil || len(tail) != 3 || !strings.HasPrefix(tail[2].Message, "699 ") || !strings.HasPrefix(tail[0].Message, "697 ") {
		t.Fatalf("tail = %d records, err %v", len(tail), err)
	}
}

func TestATornWriteEndsTheSegment(t *testing.T) {
	path := volumeFile(t, 4)
	c := &clock{time.Unix(1_800_000_000, 0)}
	v := boot(t, path, c, 1)
	for i := range 10 {
		v.Append(Record{Service: "janusd", Message: fmt.Sprintf("line %d", i)})
	}
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}
	// The power cut half way through the 6th record.
	f, _ := os.OpenFile(path, os.O_RDWR, 0)
	off := int64(SegmentSize + BlockSize)
	for range 5 {
		off += int64(recordHeader + len("janusd") + len("line 0"))
	}
	if _, err := f.WriteAt([]byte("garbage"), off+10); err != nil {
		t.Fatal(err)
	}
	f.Close()

	v = boot(t, path, c, 2)
	if got := messages(t, v, 1); len(got) != 5 || got[4] != "janusd: line 4" {
		t.Fatalf("after a torn write: %q", got)
	}
	v.Append(Record{Service: "janusd", Message: "next boot"})
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}
	v = boot(t, path, c, 3)
	if got := messages(t, v, 2); len(got) != 1 {
		t.Fatalf("boot 2: %q", got)
	}
}

func TestLeftoversOfASegmentsPreviousTurnAreNotRead(t *testing.T) {
	path := volumeFile(t, 3) // 2 segments
	c := &clock{time.Unix(1_800_000_000, 0)}
	long := strings.Repeat("z", 2000)
	// Boots 1 and 2 fill both segments with many records...
	for b := 1; b <= 2; b++ {
		v := boot(t, path, c, byte(b))
		for i := range 400 {
			v.Append(Record{Service: "janusd", Message: fmt.Sprintf("old %d %s", i, long)})
		}
		if err := v.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	// ...boot 3 takes segment 0 back, with one record.
	v := boot(t, path, c, 3)
	v.Append(Record{Service: "janusd", Message: "new"})
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}
	v = boot(t, path, c, 4)
	if got := messages(t, v, 3); len(got) != 1 || got[0] != "janusd: new" {
		t.Fatalf("boot 3 read %d records (the segment's previous turn's leftovers?)", len(got))
	}
	if b := v.Boots(); len(b) != 2 || b[0].Number != 3 || b[1].Number != 2 {
		t.Fatalf("boots = %+v", b)
	}
}

func TestAnotherVolumeIsFormattedOver(t *testing.T) {
	path := volumeFile(t, 4)
	c := &clock{time.Unix(1_800_000_000, 0)}
	v := boot(t, path, c, 1)
	v.Append(Record{Service: "janusd", Message: "before"})
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}
	// The superblock damaged: a new volume - nothing before counts.
	f, _ := os.OpenFile(path, os.O_RDWR, 0)
	_, _ = f.WriteAt([]byte("x"), 100)
	f.Close()
	v = boot(t, path, c, 2)
	if v.Current() != 1 || len(v.Boots()) != 0 {
		t.Fatalf("after a damaged superblock: boot %d, boots %+v", v.Current(), v.Boots())
	}
}

func TestWipe(t *testing.T) {
	path := volumeFile(t, 4)
	c := &clock{time.Unix(1_800_000_000, 0)}
	v := boot(t, path, c, 1)
	v.Append(Record{Service: "janusd", Message: "secret-ish"})
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := v.SaveCursor([]byte("cursor")); err != nil {
		t.Fatal(err)
	}
	if err := v.Wipe(); err != nil {
		t.Fatal(err)
	}
	v.Append(Record{Service: "janusd", Message: "after"}) // dropped
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}
	if v.Stats().Dropped != 1 {
		t.Fatalf("stats = %+v", v.Stats())
	}
	v = boot(t, path, c, 2)
	if v.Current() != 1 || len(v.Boots()) != 0 || v.LoadCursor() != nil {
		t.Fatalf("after a wipe: boot %d, boots %+v, cursor %q", v.Current(), v.Boots(), v.LoadCursor())
	}
}

func TestCursorSlots(t *testing.T) {
	path := volumeFile(t, 4)
	c := &clock{time.Unix(1_800_000_000, 0)}
	v := boot(t, path, c, 1)
	if v.LoadCursor() != nil {
		t.Fatal("a blank volume has a cursor")
	}
	for i := range 5 {
		if err := v.SaveCursor(fmt.Appendf(nil, "c%d", i)); err != nil {
			t.Fatal(err)
		}
		if got := string(v.LoadCursor()); got != fmt.Sprintf("c%d", i) {
			t.Fatalf("cursor = %q after saving c%d", got, i)
		}
	}
	// The newest slot torn: the previous cursor.
	g1, _, _ := v.readSlot(1)
	g2, _, _ := v.readSlot(2)
	newest := 1
	if g2 > g1 {
		newest = 2
	}
	f, _ := os.OpenFile(path, os.O_RDWR, 0)
	_, _ = f.WriteAt([]byte("torn"), int64(newest)*BlockSize+40)
	f.Close()
	v = boot(t, path, c, 2)
	if got := string(v.LoadCursor()); got != "c3" {
		t.Fatalf("cursor = %q with the newest slot torn, want c3", got)
	}
	if err := v.SaveCursor(make([]byte, MaxCursor+1)); err == nil {
		t.Fatal("an oversized cursor was saved")
	}
}

func TestAppendNeverWaits(t *testing.T) {
	path := volumeFile(t, 4)
	c := &clock{time.Unix(1_800_000_000, 0)}
	v := boot(t, path, c, 1)
	msg := strings.Repeat("q", MaxMessage)
	n := MaxQueued/(MaxMessage+recordHeader+len("haproxy")) + 10
	for range n {
		v.Append(Record{Service: "haproxy", Message: msg})
	}
	if d := v.Stats().Dropped; d < 10 {
		t.Fatalf("dropped %d with the queue full", d)
	}
	// A message past MaxMessage is cut.
	v2 := boot(t, volumeFile(t, 4), c, 1)
	v2.Append(Record{Service: strings.Repeat("s", 100), Message: strings.Repeat("m", MaxMessage+5)})
	if err := v2.Flush(); err != nil {
		t.Fatal(err)
	}
	var r Record
	_ = v2.Read(1, func(x Record) bool { r = x; return false })
	if len(r.Service) != MaxService || len(r.Message) != MaxMessage {
		t.Fatalf("kept %d/%d bytes", len(r.Service), len(r.Message))
	}
}

func TestTooSmall(t *testing.T) {
	if _, err := Open(volumeFile(t, 2), Options{}); err == nil {
		t.Fatal("a 2 MiB volume opened")
	}
}

func TestOpenWritesInTheBackground(t *testing.T) {
	path := volumeFile(t, 4)
	v, err := Open(path, Options{BootID: [16]byte{9}})
	if err != nil {
		t.Fatal(err)
	}
	v.Append(Record{Service: "janusd", Message: "hello"})
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	c := &clock{time.Now()}
	v = boot(t, path, c, 10)
	if got := messages(t, v, 1); len(got) != 1 || got[0] != "janusd: hello" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatForgetsWhatWasThere(t *testing.T) {
	path := volumeFile(t, 4)
	c := &clock{time.Unix(1_800_000_000, 0)}
	v := boot(t, path, c, 1)
	v.Append(Record{Service: "janusd", Message: "an older install's"})
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(path, os.O_RDWR, 0)
	if err := Format(f, 0, 4<<20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	v = boot(t, path, c, 2)
	if v.Current() != 1 || len(v.Boots()) != 0 {
		t.Fatalf("after Format: boot %d, boots %+v", v.Current(), v.Boots())
	}
}

func TestJanusdRestartedGoesOnWithTheSameBoot(t *testing.T) {
	path := volumeFile(t, 4)
	c := &clock{time.Unix(1_800_000_000, 0)}
	v := boot(t, path, c, 7)
	v.Append(Record{Service: "janusd", Message: "before the restart"})
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}
	v = boot(t, path, c, 7) // same kernel boot_id
	if v.Current() != 1 {
		t.Fatalf("a restarted janusd is on boot %d", v.Current())
	}
	v.Append(Record{Service: "janusd", Message: "after"})
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}
	v = boot(t, path, c, 8)
	if v.Current() != 2 {
		t.Fatalf("a reboot is boot %d", v.Current())
	}
	if got := messages(t, v, 1); strings.Join(got, "|") != "janusd: before the restart|janusd: after" {
		t.Fatalf("boot 1 = %q", got)
	}
}

func TestReadFromAcrossBoots(t *testing.T) {
	path := volumeFile(t, 8)
	c := &clock{time.Unix(1_800_000_000, 0)}
	msg := strings.Repeat("r", 3000)
	for b := 1; b <= 3; b++ {
		v := boot(t, path, c, byte(b))
		for i := range 400 { // more than a segment a boot
			v.Append(Record{Service: "janusd", Message: fmt.Sprintf("b%d-%d %s", b, i, msg)})
		}
		if err := v.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	v := boot(t, path, c, 4)
	var seqs []uint64
	last, err := v.ReadFrom(0, func(r Record) bool { seqs = append(seqs, r.Seq); return true })
	if err != nil || len(seqs) != 1200 || last != 1200 {
		t.Fatalf("ReadFrom(0): %d records, last %d, err %v", len(seqs), last, err)
	}
	for i, s := range seqs {
		if s != uint64(i+1) {
			t.Fatalf("record %d has seq %d", i, s)
		}
	}
	// From the middle of the second boot: the rest, in order.
	var got []Record
	last, _ = v.ReadFrom(650, func(r Record) bool { got = append(got, r); return true })
	if len(got) != 550 || got[0].Seq != 651 || !strings.HasPrefix(got[0].Message, "b2-250 ") || last != 1200 {
		t.Fatalf("ReadFrom(650): %d records from %d (%.8q), last %d", len(got), got[0].Seq, got[0].Message, last)
	}
	// Stopping early says where it stopped.
	n := 0
	last, _ = v.ReadFrom(1000, func(r Record) bool { n++; return n < 3 })
	if last != 1002 {
		t.Fatalf("stopped after 2 records, last = %d", last)
	}
	// Nothing new.
	if last, _ := v.ReadFrom(1200, func(Record) bool { t.Fatal("a record past the last"); return true }); last != 1200 {
		t.Fatalf("ReadFrom(1200) = %d", last)
	}
	if v.ID() == ([16]byte{}) {
		t.Fatal("no volume ID")
	}
}

func TestWrittenSignals(t *testing.T) {
	path := volumeFile(t, 4)
	c := &clock{time.Unix(1_800_000_000, 0)}
	v := boot(t, path, c, 1)
	ch := v.Written()
	v.Append(Record{Service: "janusd", Message: "x"})
	if err := v.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("Written wasn't closed by a write")
	}
}

// A reader that keeps its place (the log shipper) asking for what's
// after the last record, twice a flush, on a full current segment.
func BenchmarkReadFromFollow(b *testing.B) {
	path := filepath.Join(b.TempDir(), "logs.img")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		b.Fatal(err)
	}
	if err := os.Truncate(path, 4<<20); err != nil {
		b.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	c := &clock{time.Unix(1_800_000_000, 0)}
	v, err := open(f, Options{BootID: [16]byte{1}, Now: c.now})
	if err != nil {
		b.Fatal(err)
	}
	line := `10.200.40.7:40000 [15/Oct/2026:21:30:01.123] web web/srv1 0/0/1/2/3 200 1234 "GET /api/x HTTP/1.1"`
	for i := 0; i < 7000; i++ { // 7000 × (28 + 7 + 96) bytes: one segment, nearly full
		v.Append(Record{Service: "haproxy", Severity: SeverityUnknown, Message: line})
	}
	if err := v.Flush(); err != nil {
		b.Fatal(err)
	}
	last := v.LastSeq()
	if v.seg != 0 || v.off < 900<<10 {
		b.Fatalf("the records should nearly fill the first segment: segment %d, %d bytes", v.seg, v.off)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		got, err := v.ReadFrom(last-1, func(Record) bool { n++; return true })
		if err != nil || n != 1 || got != last {
			b.Fatalf("ReadFrom: %d records, last %d, err %v", n, got, err)
		}
	}
}
