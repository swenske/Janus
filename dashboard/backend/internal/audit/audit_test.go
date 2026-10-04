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
	got, err := l.Recent(10, nil)
	if err != nil || len(got) != 10 || got[0].User != "u59" || got[9].User != "u50" {
		t.Fatalf("Recent(10) = %v, %v", got, err)
	}
	// Across the rotated file, newest first, bounded.
	all, _ := l.Recent(1000, nil)
	if len(all) < 20 || all[0].User != "u59" {
		t.Fatalf("Recent(all): %d entries, first %+v", len(all), all[0])
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
	if got, _ := l2.Recent(1, func(e Entry) bool { return e.Method == "POST" }); len(got) != 1 || got[0].User != "u59" {
		t.Errorf("filtered: %+v", got)
	}
}
