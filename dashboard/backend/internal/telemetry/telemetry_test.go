package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportCounts(t *testing.T) {
	r := NewReport("id")
	r.AddNode(&Node{Version: "v2026.10.29", Arch: "amd64", HAProxy: "3.4", Kernel: "stable", Console: "serial", Schematic: "a",
		Extensions: []string{"bird", "keepalived"}, VCPUs: 2, MemoryBytes: 1 << 30})
	r.AddNode(&Node{Version: "v2026.10.29", Arch: "amd64", HAProxy: "3.2", Kernel: "stable", Console: "screen", Schematic: "b",
		Extensions: []string{"bird"}, VCPUs: 8, MemoryBytes: 900 << 20})
	r.AddNode(nil)
	r.AddCluster(3)
	r.AddCluster(2)
	n := r.Nodes
	if n.Total != 3 || n.Unreachable != 1 || n.Versions["v2026.10.29"] != 2 || n.Extensions["bird"] != 2 || n.Extensions["keepalived"] != 1 ||
		n.HAProxy["3.2"] != 1 || n.Console["screen"] != 1 || n.Schematics["a"] != 1 || n.VCPUs["2"] != 1 || n.VCPUs["8+"] != 1 ||
		n.Memory["1-2GiB"] != 1 || n.Memory["<1GiB"] != 1 {
		t.Errorf("nodes %+v", n)
	}
	if r.Clusters.Count != 2 || len(r.Clusters.Sizes) != 2 || r.Clusters.Sizes[0] != 2 {
		t.Errorf("clusters %+v", r.Clusters)
	}
	for in, want := range map[int]string{1: "1", 2: "2", 3: "2", 4: "4", 7: "4", 8: "8+", 64: "8+"} {
		if got := VCPUBucket(in); got != want {
			t.Errorf("VCPUBucket(%d) = %s", in, got)
		}
	}
	for in, want := range map[uint64]string{512 << 20: "<1GiB", 1 << 30: "1-2GiB", 3 << 30: "2-4GiB", 16 << 30: "4GiB+"} {
		if got := MemoryBucket(in); got != want {
			t.Errorf("MemoryBucket(%d) = %s", in, got)
		}
	}
}

// The docs show a report (examples/telemetry/report.json): it is one,
// field for field.
func TestExampleReport(t *testing.T) {
	data, err := os.ReadFile("../../../../examples/telemetry/report.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var r Report
	if err := dec.Decode(&r); err != nil {
		t.Fatal(err)
	}
	again, _ := json.MarshalIndent(&r, "", "  ")
	var a, b any
	_ = json.Unmarshal(data, &a)
	_ = json.Unmarshal(again, &b)
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) || r.Schema != Schema {
		t.Errorf("the example isn't a whole report:\n%s\n%s", x, y)
	}
}

func TestSchedule(t *testing.T) {
	s, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	if s.Due(t0.Add(48 * time.Hour)) {
		t.Fatal("due while off")
	}
	if err := s.SetEnabled(true, t0); err != nil {
		t.Fatal(err)
	}
	first := s.Next()
	if first.Before(t0) || !first.Before(t0.Add(time.Hour)) {
		t.Fatalf("first report at %v, not within the hour", first)
	}
	_ = s.Recorded(first, nil)
	if next := s.Next(); !next.Equal(first.Add(24 * time.Hour)) {
		t.Errorf("after a report: %v", next)
	}
	day := first.Add(24 * time.Hour)
	_ = s.Recorded(day, io.EOF)
	if next := s.Next(); !next.Equal(day.Add(time.Hour)) {
		t.Errorf("after a failure: %v, want an hour later", next)
	}
	_ = s.Recorded(day.Add(time.Hour), io.EOF)
	if next := s.Next(); !next.Equal(day.Add(25 * time.Hour)) {
		t.Errorf("after two failures: %v, want the next day", next)
	}

	// Kept across a restart; the ID stays until made again.
	id := s.Get().InstallID
	again, err := Open(filepath.Dir(s.path), false)
	if err != nil || again.Get().InstallID != id || !again.Get().Enabled {
		t.Fatalf("reopened: %+v, %v", again.Get(), err)
	}
	old, err := again.Regenerate()
	if err != nil || old != id || again.Get().InstallID == id || len(again.Get().InstallID) != 32 {
		t.Errorf("regenerate: %q -> %q, %v", old, again.Get().InstallID, err)
	}
	if fi, _ := os.Stat(s.path); fi.Mode().Perm() != 0o600 {
		t.Errorf("telemetry.json is %v", fi.Mode().Perm())
	}

	locked, _ := Open(filepath.Dir(s.path), true)
	if locked.Get().Enabled || !locked.Next().IsZero() || locked.SetEnabled(true, t0) != ErrLocked {
		t.Error("the environment's lock doesn't hold")
	}
}

// Send posts the report; Forget deletes the ID; a redirect isn't
// followed.
func TestSendAndForget(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type")+" "+string(body))
		if r.URL.Path == "/moved/report" {
			http.Redirect(w, r, "/v1/report", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	r := NewReport("abc")
	if err := Send(context.Background(), srv.URL+"/v1/report", r); err != nil {
		t.Fatal(err)
	}
	if err := Forget(context.Background(), srv.URL+"/v1/report", "abc"); err != nil {
		t.Fatal(err)
	}
	if err := Send(context.Background(), srv.URL+"/moved/report", r); err == nil {
		t.Error("a redirect counted as sent")
	}
	if len(got) != 3 || !strings.HasPrefix(got[0], `POST /v1/report application/json {"schema":1,"install_id":"abc"`) || got[1] != "DELETE /v1/installs/abc  " {
		t.Errorf("requests %q", got)
	}
}
