package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/swenske/Janus/internal/ring"
)

func TestPublishAndSince(t *testing.T) {
	_, start := Since(^uint64(0) - 1) // skip whatever other tests published
	Publish("test.one", map[string]string{"k": "v"})
	Publish("test.two", nil)

	evs, next := Since(start)
	if len(evs) != 2 || next != start+2 {
		t.Fatalf("Since(%d) = %d events, next %d", start, len(evs), next)
	}
	if evs[0].ID != start+1 || evs[1].ID != start+2 || evs[0].Type != "test.one" || evs[1].Type != "test.two" {
		t.Errorf("events = %+v", evs)
	}
	var payload map[string]string
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil || payload["k"] != "v" {
		t.Errorf("payload = %s, %v", evs[0].Payload, err)
	}
	if evs[1].Payload != nil {
		t.Errorf("nil payload encoded as %s", evs[1].Payload)
	}
	// Resuming from the last ID seen returns only newer events.
	if more, _ := Since(evs[1].ID); len(more) != 0 {
		t.Errorf("Since(last id) = %+v, want none", more)
	}
}

// restart drops what's in memory, as a janusd that stops would: what
// Persist kept is still on the disk.
func restart(t *testing.T) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if disk != nil && disk.f != nil {
		disk.f.Close()
	}
	held, nextID, disk = ring.New[Event](Capacity), 1, nil
}

func types(evs []Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, fmt.Sprintf("%d:%s", e.ID, e.Type))
	}
	return out
}

// A node's log survives janusd's restarts and the node's reboots: what
// was kept comes back with its IDs, the next ones go on after them.
func TestPersistAcrossRestarts(t *testing.T) {
	restart(t)
	t.Cleanup(func() { restart(t) })
	dir := filepath.Join(t.TempDir(), "events")

	Publish("early.one", nil) // before Persist: kept too, after what's on the disk
	if err := Persist(dir); err != nil {
		t.Fatal(err)
	}
	Publish("boot.one", map[string]string{"slot": "A"})
	Publish("lifecycle.upgrade", map[string]string{"to": "B"})
	Flush()

	restart(t)
	Publish("janusd.started", map[string]string{"slot": "B"})
	if err := Persist(dir); err != nil {
		t.Fatal(err)
	}
	evs, next := Since(0)
	want := []string{"1:early.one", "2:boot.one", "3:lifecycle.upgrade", "4:janusd.started"}
	if got := types(evs); !slices.Equal(got, want) || next != 4 {
		t.Fatalf("after a restart: %v, next %d; want %v", got, next, want)
	}
	var p map[string]string
	if err := json.Unmarshal(evs[2].Payload, &p); err != nil || p["to"] != "B" || evs[0].Payload != nil {
		t.Errorf("payloads: %s / %s", evs[2].Payload, evs[0].Payload)
	}
	Publish("after", nil)
	if more, _ := Since(4); len(more) != 1 || more[0].ID != 5 {
		t.Errorf("Since(4) = %v", types(more))
	}

	// A power cut mid-line: that line is skipped, the rest kept.
	Flush()
	f, err := os.OpenFile(filepath.Join(dir, FileName), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"id":6,"time":"2026-10-13T16:24`)
	f.Close()
	restart(t)
	if err := Persist(dir); err != nil {
		t.Fatal(err)
	}
	if evs, next := Since(0); len(evs) != 5 || next != 5 {
		t.Errorf("after a torn line: %v, next %d", types(evs), next)
	}
	Publish("next", nil)
	if evs, _ := Since(5); len(evs) != 1 || evs[0].ID != 6 {
		t.Errorf("the next event after a torn line: %v", types(evs))
	}
}

// The file holds at most twice Capacity lines, and what's loaded is the
// last Capacity events.
func TestPersistKeepsCapacity(t *testing.T) {
	restart(t)
	t.Cleanup(func() { restart(t) })
	dir := t.TempDir()
	if err := Persist(dir); err != nil {
		t.Fatal(err)
	}
	total := 2*Capacity + 5
	for i := range total {
		Publish("n", map[string]int{"i": i})
	}
	Flush()
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(data, []byte("\n")); lines >= 2*Capacity || lines < Capacity {
		t.Errorf("the file holds %d lines", lines)
	}
	restart(t)
	if err := Persist(dir); err != nil {
		t.Fatal(err)
	}
	evs, next := Since(0)
	if len(evs) != Capacity || next != uint64(total) || evs[0].ID != uint64(total-Capacity+1) {
		t.Errorf("loaded %d events, %d..%d, next %d", len(evs), evs[0].ID, evs[len(evs)-1].ID, next)
	}
}

// A reader ahead of the log - an ID from before a log that started over
// (a reset node, an older janusd) - gets what comes from now on.
func TestSinceAheadOfTheLog(t *testing.T) {
	restart(t)
	t.Cleanup(func() { restart(t) })
	Publish("a", nil)
	evs, next := Since(900)
	if evs != nil || next != 1 {
		t.Fatalf("Since(900) = %v, next %d", types(evs), next)
	}
	Publish("b", nil)
	if evs, _ := Since(next); len(evs) != 1 || evs[0].Type != "b" {
		t.Errorf("then %v", types(evs))
	}
}
