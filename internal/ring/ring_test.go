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
