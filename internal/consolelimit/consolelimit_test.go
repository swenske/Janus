package consolelimit

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// drain returns what's queued for the console.
func drain(b *Budget) string {
	var s strings.Builder
	for {
		select {
		case l := <-b.lines:
			s.Write(l)
		default:
			return s.String()
		}
	}
}

func TestLinesWithinTheBudget(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	b := newBudget(io.Discard, 10, 100, c.now)
	w := b.For("haproxy")
	// A line in two writes, two lines in one.
	_, _ = io.WriteString(w, "[NOTICE] st")
	_, _ = io.WriteString(w, "arting\nsecond\nthird\n")
	if got, want := drain(b), "[NOTICE] starting\nsecond\nthird\n"; got != want {
		t.Fatalf("console got %q, want %q", got, want)
	}
	if n := b.notes(); n != nil {
		t.Fatalf("notes = %q, want none", n)
	}
}

func TestOverTheBudgetIsLeftOutAndSaidSo(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	b := newBudget(io.Discard, 10, 20, c.now)
	w := b.For("haproxy")
	_, _ = io.WriteString(w, strings.Repeat("a", 14)+"\n") // 15 of 20
	_, _ = io.WriteString(w, "bbbbbbbbb\n")                // 10 > 5 left: out
	_, _ = io.WriteString(w, "cccc\n")                     // 5 of 5
	_, _ = io.WriteString(b.For("keepalived"), "d\n")      // nothing left: out
	if got, want := drain(b), strings.Repeat("a", 14)+"\ncccc\n"; got != want {
		t.Fatalf("console got %q, want %q", got, want)
	}
	notes := b.notes()
	want := []string{
		"janusd: 1 line of haproxy's output not shown on the console - janusctl system logs haproxy\n",
		"janusd: 1 line of keepalived's output not shown on the console - janusctl system logs keepalived\n",
	}
	if strings.Join(notes, "") != strings.Join(want, "") {
		t.Fatalf("notes = %q, want %q", notes, want)
	}
	if n := b.notes(); n != nil {
		t.Fatalf("notes said twice: %q", n)
	}

	// The budget fills back up with time, up to its burst.
	c.t = c.t.Add(time.Hour)
	_, _ = io.WriteString(w, strings.Repeat("e", 19)+"\n")
	if got := drain(b); got != strings.Repeat("e", 19)+"\n" {
		t.Fatalf("after an hour the console got %q", got)
	}
}

func TestALongLineIsCut(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	b := newBudget(io.Discard, 1<<20, 1<<20, c.now)
	w := b.For("consul")
	_, _ = io.WriteString(w, strings.Repeat("x", 3*maxLine))
	_, _ = io.WriteString(w, strings.Repeat("y", maxLine)+"\nnext\n")
	if got, want := drain(b), strings.Repeat("x", maxLine)+"\nnext\n"; got != want {
		t.Fatalf("console got %d bytes %q..., want %d", len(got), got[:20], len(want))
	}
}

func TestAFullQueueNeverWaits(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	b := newBudget(io.Discard, 1<<20, 1<<20, c.now)
	w := b.For("haproxy")
	done := make(chan struct{})
	go func() {
		for range queued + 10 {
			_, _ = io.WriteString(w, "line\n")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a write waited for the console")
	}
	if got := strings.Count(drain(b), "\n"); got != queued {
		t.Fatalf("%d lines queued, want %d", got, queued)
	}
	if n := b.notes(); len(n) != 1 || !strings.HasPrefix(n[0], "janusd: 10 lines of haproxy's") {
		t.Fatalf("notes = %q", n)
	}
}

// syncBuffer is a bytes.Buffer safe to read while run writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestRunWritesLinesThenNotes(t *testing.T) {
	out := &syncBuffer{}
	c := &clock{time.Unix(0, 0)}
	b := newBudget(out, 0, 6, c.now)
	tick := make(chan time.Time)
	go b.run(tick)
	w := b.For("bird")
	_, _ = io.WriteString(w, "hello\nworld\n")
	deadline := time.Now().Add(5 * time.Second)
	for out.String() != "hello\n" {
		if time.Now().After(deadline) {
			t.Fatalf("console = %q", out.String())
		}
		time.Sleep(time.Millisecond)
	}
	tick <- time.Time{}
	want := "hello\njanusd: 1 line of bird's output not shown on the console - janusctl system logs bird\n"
	for out.String() != want {
		if time.Now().After(deadline) {
			t.Fatalf("console = %q, want %q", out.String(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A budget long exhausted (the clock never moves): every line is left
// out, the common case under load.
func BenchmarkWriteDropped(b *testing.B) {
	c := &clock{time.Unix(0, 0)}
	bud := newBudget(io.Discard, 1, 1, c.now)
	w := bud.For("haproxy")
	line := []byte("10.200.40.7:40000 [15/Oct/2026:21:30:01.123] web web/<NOSRV> 0/-1/-1/-1/0 200 64 - - LR-- 1/1/0/0/0 0/0 \"GET / HTTP/1.1\"\n")
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = w.Write(line)
	}
}

// A lower budget takes hold at once: what's in hand is capped at the
// new burst; 0 lets nothing through and counts every line.
func TestSetAndLeftOut(t *testing.T) {
	c := &clock{time.Unix(0, 0)}
	b := newBudget(io.Discard, 100, 1000, c.now)
	w := b.For("haproxy")
	_, _ = io.WriteString(w, "one\n")
	if got := drain(b); got != "one\n" {
		t.Fatalf("before: %q", got)
	}
	b.Set(0, 0)
	_, _ = io.WriteString(w, "two\nthree\n")
	if got := drain(b); got != "" {
		t.Fatalf("at 0: console got %q", got)
	}
	if n := b.LeftOut()["haproxy"]; n != 2 {
		t.Fatalf("left out = %d, want 2", n)
	}
	if rate, burst := b.Settings(); rate != 0 || burst != 0 {
		t.Fatalf("settings = %d/%d", rate, burst)
	}
	b.Set(100, 1000)
	c.t = c.t.Add(time.Second)
	_, _ = io.WriteString(w, "four\n")
	if got := drain(b); got != "four\n" {
		t.Fatalf("back: %q", got)
	}
	if n := b.LeftOut()["haproxy"]; n != 2 {
		t.Fatalf("left out after = %d, want 2 still", n)
	}
}

func TestConfigFile(t *testing.T) {
	Dir = t.TempDir()
	cfg, isDefault, err := Load()
	if err != nil || !isDefault || cfg != DefaultConfig() {
		t.Fatalf("Load with nothing saved: %+v %v %v", cfg, isDefault, err)
	}
	if err := Save(Config{RateBytesPerSecond: 512, BurstBytes: 100}); err != nil {
		t.Fatal(err)
	}
	cfg, isDefault, err = Load()
	if err != nil || isDefault || cfg.RateBytesPerSecond != 512 || cfg.BurstBytes != 512 {
		t.Fatalf("Load after Save: %+v %v %v (a burst below the rate is raised to it)", cfg, isDefault, err)
	}
	for _, bad := range []Config{{RateBytesPerSecond: -1}, {RateBytesPerSecond: MaxRate + 1}, {BurstBytes: MaxBurst + 1}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	zero := Config{}
	if err := zero.Validate(); err != nil {
		t.Fatalf("0/0 refused: %v", err)
	}
	if err := Reset(); err != nil {
		t.Fatal(err)
	}
	if _, isDefault, _ = Load(); !isDefault {
		t.Fatal("Reset left a configuration")
	}
	if err := Reset(); err != nil {
		t.Fatalf("Reset twice: %v", err)
	}
}
