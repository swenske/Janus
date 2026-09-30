package events

import (
	"encoding/json"
	"testing"
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
