package haproxy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryRecord(t *testing.T) {
	h := &History{Dir: filepath.Join(t.TempDir(), "history")}
	alice := Author{Name: "alice", Roles: []string{"os:operator"}, Via: "controller"}

	// The first apply keeps what it replaced, as found before the history.
	v, err := h.Record([]byte("old\n"), []byte("a\n"), alice, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != 2 || v.Author.Name != "alice" || v.Time.IsZero() {
		t.Fatalf("first version: %+v", v)
	}
	// The same file again (a reload) isn't a new version.
	if again, err := h.Record([]byte("a\n"), []byte("a\n"), Author{Name: "bob"}, 0); err != nil || again.ID != 2 || again.Author.Name != "alice" {
		t.Fatalf("a reload made a version: %+v, %v", again, err)
	}
	if _, err := h.Record([]byte("a\n"), []byte("b\n"), Author{Name: "bob"}, 0); err != nil {
		t.Fatal(err)
	}
	back, err := h.Record([]byte("b\n"), []byte("a\n"), Author{Name: "carol"}, 2)
	if err != nil || back.ID != 4 || back.RevertedFrom != 2 {
		t.Fatalf("going back: %+v, %v", back, err)
	}

	list, err := h.List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []uint64
	for _, v := range list {
		ids = append(ids, v.ID)
	}
	if len(ids) != 4 || ids[0] != 4 || ids[3] != 1 {
		t.Fatalf("ids newest first: %v", ids)
	}
	if !list[3].BeforeHistory || !list[3].Time.IsZero() || list[3].Author.Name != "" {
		t.Errorf("the version found before the history: %+v", list[3])
	}
	v1, cfg, err := h.Get(1)
	if err != nil || string(cfg) != "old\n" || !v1.BeforeHistory {
		t.Errorf("Get(1) = %+v %q %v", v1, cfg, err)
	}
	if _, _, err := h.Get(9); !errors.Is(err, ErrNoVersion) {
		t.Errorf("Get(9): %v, want ErrNoVersion", err)
	}
	// An empty history over an empty file: only the applied version.
	h2 := &History{Dir: filepath.Join(t.TempDir(), "history")}
	if v, err := h2.Record(nil, []byte("x\n"), alice, 0); err != nil || v.ID != 1 || v.BeforeHistory {
		t.Errorf("nothing before: %+v, %v", v, err)
	}
}

func TestHistoryBounds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	h := &History{Dir: dir, MaxVersions: 3, MaxBytes: 100}
	for i := range 6 {
		if _, err := h.Record(nil, []byte(strings.Repeat("x", 10)+string(rune('a'+i))+"\n"), Author{Name: "a"}, 0); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := h.List()
	if len(list) != 3 || list[0].ID != 6 || list[2].ID != 4 {
		t.Fatalf("by count: %+v", list)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 4 { // three versions and the index
		var names []string
		for _, f := range files {
			names = append(names, f.Name())
		}
		t.Errorf("files left: %v", names)
	}
	// By size: the newest is kept even alone over the bound.
	big := []byte(strings.Repeat("y", 150) + "\n")
	if _, err := h.Record(nil, big, Author{Name: "a"}, 0); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.List(); len(list) != 1 || list[0].ID != 7 {
		t.Fatalf("by size: %+v", list)
	}
	if _, err := h.Record(nil, []byte("small\n"), Author{Name: "a"}, 0); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.List(); len(list) != 1 || list[0].ID != 8 {
		t.Fatalf("by size, the big one dropped: %+v", list)
	}
}

// TestHistoryStrayFile: a version file a power cut left without its
// index entry is ignored, then removed by the next version - and its id
// is used again.
func TestHistoryStrayFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	h := &History{Dir: dir}
	if _, err := h.Record(nil, []byte("a\n"), Author{}, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "v2.cfg"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.List(); len(list) != 1 {
		t.Fatalf("the stray file is listed: %+v", list)
	}
	if _, err := h.Record(nil, []byte("b\n"), Author{}, 0); err != nil {
		t.Fatal(err)
	}
	if _, cfg, err := h.Get(2); err != nil || string(cfg) != "b\n" {
		t.Errorf("Get(2) = %q, %v", cfg, err)
	}
}

func TestLineChanges(t *testing.T) {
	for _, c := range []struct {
		a, b           string
		added, removed int
	}{
		{"", "", 0, 0},
		{"", "a\nb\n", 2, 0},
		{"a\nb\n", "", 0, 2},
		{"a\nb\nc\n", "a\nb\nc\n", 0, 0},
		{"a\nb\nc\n", "a\nx\nc\n", 1, 1},
		{"a\nb\nc\n", "c\nb\na\n", 2, 2},
		{"global\n  maxconn 10\ndefaults\n", "global\n  maxconn 20\n  nbthread 4\ndefaults\n", 2, 1},
	} {
		if a, r := LineChanges([]byte(c.a), []byte(c.b)); a != c.added || r != c.removed {
			t.Errorf("LineChanges(%q, %q) = +%d -%d, want +%d -%d", c.a, c.b, a, r, c.added, c.removed)
		}
	}
	// Past the edit bound: counted by content.
	var x, y strings.Builder
	for i := range 3000 {
		x.WriteString("x" + strings.Repeat("a", i%7) + string(rune('0'+i%10)) + "\n")
		y.WriteString("y" + string(rune('0'+i%10)) + "\n")
	}
	if a, r := LineChanges([]byte(x.String()), []byte(y.String())); a != 3000 || r != 3000 {
		t.Errorf("a whole rewrite: +%d -%d", a, r)
	}
}
