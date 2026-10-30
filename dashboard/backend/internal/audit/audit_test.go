package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLog(t *testing.T) {
	dir := t.TempDir()
	old := MaxSize
	MaxSize = 2000
	t.Cleanup(func() { MaxSize = old })

	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		if err := l.Append(Entry{Time: time.Unix(int64(i), 0), User: fmt.Sprint("u", i), Method: "POST", Path: "/api/x", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, "audit.jsonl")); fi.Size() > MaxSize || fi.Mode().Perm() != 0o600 {
		t.Errorf("audit.jsonl: %d bytes, %v", fi.Size(), fi.Mode().Perm())
	}
	p, err := l.Query(Filter{}, "", 10)
	got := p.Entries
	if err != nil || len(got) != 10 || got[0].User != "u59" || got[9].User != "u50" {
		t.Fatalf("Query(10) = %v, %v", got, err)
	}
	// Across the rotated file, newest first, bounded.
	p, _ = l.Query(Filter{}, "", 1000)
	all := p.Entries
	if len(all) < 20 || all[0].User != "u59" || p.Total != len(all) || p.Next != "" {
		t.Fatalf("Query(all): %d entries, first %+v", len(all), all[0])
	}
	for i := 1; i < len(all); i++ {
		if !all[i].Time.Before(all[i-1].Time) {
			t.Fatalf("not newest first at %d: %v after %v", i, all[i].Time, all[i-1].Time)
		}
	}
	// Kept across a reopen, still rotated by size.
	l2, _ := Open(dir)
	if err := l2.Append(Entry{User: "again", Method: "DELETE", Path: "/api/y"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := l2.Query(Filter{Method: "POST"}, "", 1); len(p.Entries) != 1 || p.Entries[0].User != "u59" || p.Next != "60" {
		t.Errorf("filtered: %+v", p)
	}
}

// IDs go on across a reopen; Since reads behind a cursor, across the
// rotated file; Changed says when there's more.
func TestSinceAndChanged(t *testing.T) {
	dir := t.TempDir()
	old := MaxSize
	MaxSize = 1000
	t.Cleanup(func() { MaxSize = old })
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ch := l.Changed()
	for i := range 20 {
		if err := l.Append(Entry{User: fmt.Sprint("u", i), Method: "POST", Path: "/api/x", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-ch:
	default:
		t.Fatal("Changed wasn't closed by an append")
	}
	if l.Latest() != 20 {
		t.Fatalf("Latest = %d", l.Latest())
	}
	got, last := l.Since(15, 100)
	if len(got) != 5 || got[0].ID != 16 || got[0].User != "u15" || last != 20 {
		t.Fatalf("Since(15) = %d entries from %+v, last %d", len(got), got, last)
	}
	l2, _ := Open(dir)
	_ = l2.Append(Entry{User: "after a restart", Method: "PUT", Path: "/api/y"})
	if got, _ := l2.Since(20, 10); len(got) != 1 || got[0].ID != 21 {
		t.Fatalf("after a reopen: %+v", got)
	}
}

// Nothing newer than the last entry - what the log shipper asks at
// least twice a flush.
func BenchmarkSinceNothingNew(b *testing.B) {
	dir := b.TempDir()
	l, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		if err := l.Append(Entry{Time: time.Unix(int64(i), 0), User: "admin", Via: "session", Method: "POST", Path: "/api/nodes/node-01/haproxy/config", Status: 200, Client: "10.1.2.3:51234"}); err != nil {
			b.Fatal(err)
		}
	}
	last := l.Latest()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got, _ := l.Since(last, 500); len(got) != 0 {
			b.Fatal("entries after the last one")
		}
	}
}
