package ring

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRingSinceAndEviction(t *testing.T) {
	r := New[int](3)
	for i := 0; i < 5; i++ {
		if seq := r.Append(i * 10); seq != uint64(i) {
			t.Fatalf("Append #%d returned seq %d", i, seq)
		}
	}
	// Only the last 3 (seq 2..4) remain.
	got, next := r.Since(0)
	if fmt.Sprint(got) != "[20 30 40]" || next != 5 {
		t.Errorf("Since(0) = %v, %d", got, next)
	}
	got, next = r.Since(3)
	if fmt.Sprint(got) != "[30 40]" || next != 5 {
		t.Errorf("Since(3) = %v, %d", got, next)
	}
	got, next = r.Since(5)
	if got != nil || next != 5 {
		t.Errorf("Since(5) = %v, %d", got, next)
	}
	got, _ = r.Last(2)
	if fmt.Sprint(got) != "[30 40]" {
		t.Errorf("Last(2) = %v", got)
	}
	got, _ = r.Last(0)
	if fmt.Sprint(got) != "[20 30 40]" {
		t.Errorf("Last(0) = %v", got)
	}
}

func TestRingEach(t *testing.T) {
	r := New[int](3)
	var got []int
	r.Each(func(v *int) { got = append(got, *v) })
	if got != nil {
		t.Errorf("an empty ring: %v", got)
	}
	for i := 0; i < 5; i++ {
		r.Append(i)
	}
	r.Each(func(v *int) { got = append(got, *v) })
	if fmt.Sprint(got) != "[2 3 4]" {
		t.Errorf("Each = %v, want the items held, oldest first", got)
	}
}

func TestRingChanged(t *testing.T) {
	r := New[string](10)
	ch := r.Changed()
	select {
	case <-ch:
		t.Fatal("Changed fired before any Append")
	default:
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		r.Append("x")
	}()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("Changed never fired after Append")
	}
}

// The Ring against the obvious model - a slice that keeps everything -
// across many wraps: Since from every sequence number, Last, Each.
func TestRingMatchesModel(t *testing.T) {
	for _, capacity := range []int{1, 2, 7} {
		r := New[int](capacity)
		var all []int
		for v := 0; v < 5*capacity+3; v++ {
			if seq := r.Append(v); seq != uint64(v) {
				t.Fatalf("cap %d: Append(%d) = %d", capacity, v, seq)
			}
			all = append(all, v)
			held := all[max(0, len(all)-capacity):]
			first := len(all) - len(held)
			for seq := 0; seq <= len(all)+1; seq++ {
				got, next := r.Since(uint64(seq))
				want := held[min(len(held), max(0, seq-first)):]
				if fmt.Sprint(got) != fmt.Sprint(want) && len(got)+len(want) > 0 || next != uint64(len(all)) {
					t.Fatalf("cap %d after %d: Since(%d) = %v, %d; want %v, %d", capacity, v, seq, got, next, want, len(all))
				}
			}
			if got, _ := r.Last(2); fmt.Sprint(got) != fmt.Sprint(held[max(0, len(held)-2):]) {
				t.Fatalf("cap %d after %d: Last(2) = %v", capacity, v, got)
			}
			var each []int
			r.Each(func(p *int) { each = append(each, *p) })
			if fmt.Sprint(each) != fmt.Sprint(held) {
				t.Fatalf("cap %d after %d: Each = %v, want %v", capacity, v, each, held)
			}
		}
	}
}

// A full Ring takes an item without allocating: a log ring takes one a
// request (it once copied all its items each time - 27 µs a line).
func TestAppendFullAllocatesNothing(t *testing.T) {
	r := New[int](5000)
	for i := 0; i < 5000; i++ {
		r.Append(i)
	}
	if n := testing.AllocsPerRun(1000, func() { r.Append(1) }); n != 0 {
		t.Errorf("Append on a full Ring: %v allocations", n)
	}
}

func BenchmarkLineWriterFull(b *testing.B) {
	w := &LineWriter{Ring: New[string](5000)}
	line := []byte("10.200.40.7:40000 [15/Oct/2026:21:30:01.123] web web/<NOSRV> 0/-1/-1/-1/0 200 64 - - LR-- 1/1/0/0/0 0/0 \"GET / HTTP/1.1\"\n")
	for i := 0; i < 5000; i++ {
		_, _ = w.Write(line)
	}
	b.SetBytes(int64(len(line)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = w.Write(line)
	}
}

func TestLineWriter(t *testing.T) {
	r := New[string](10)
	w := &LineWriter{Ring: r}
	fmt.Fprint(w, "first line\nsecond ")
	fmt.Fprint(w, "line\n\nthird")
	got, _ := r.Since(0)
	if strings.Join(got, "|") != "first line|second line|" {
		t.Errorf("lines = %q", got)
	}
	// A line with no newline at all is flushed once it hits maxLine.
	fmt.Fprint(w, strings.Repeat("a", maxLine))
	got, _ = r.Since(3)
	if len(got) != 1 || len(got[0]) != maxLine || !strings.HasPrefix(got[0], "third") {
		t.Errorf("oversized line: %d lines, first %d bytes", len(got), len(got[0]))
	}
}

func TestLineWriterTee(t *testing.T) {
	var teed []string
	w := &LineWriter{Ring: New[string](10), Tee: func(l string) { teed = append(teed, l) }}
	fmt.Fprint(w, "one\ntw")
	fmt.Fprint(w, "o\n")
	if strings.Join(teed, "|") != "one|two" {
		t.Errorf("teed = %q", teed)
	}
}
