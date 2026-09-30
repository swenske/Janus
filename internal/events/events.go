// Package events is janusd's in-memory event log, streamed by
// SystemService.Events: what the node did and why (boot, config
// applied/rejected, HAProxy reloads, service state changes, upgrades,
// reboots...), for an operator who has no shell to look around with.
//
// It lives in memory only and restarts empty with janusd: persistent
// history isn't the goal, a live view is.
package events

import (
	"encoding/json"
	"time"

	"github.com/swenske/Janus/internal/ring"
)

// Capacity is how many events are kept for a newly connected reader.
const Capacity = 1000

type Event struct {
	// ID starts at 1 and increases by one per event within a janusd run.
	ID      uint64
	Time    time.Time
	Type    string
	Payload []byte // JSON
}

var log = ring.New[Event](Capacity)

// Publish records an event. payload is JSON-encoded; nil means none.
func Publish(typ string, payload any) {
	var data []byte
	if payload != nil {
		data, _ = json.Marshal(payload)
	}
	log.Append(Event{Time: time.Now(), Type: typ, Payload: data})
}

// Since returns the events with an ID greater than id still held, and
// the ID to pass next time.
func Since(id uint64) ([]Event, uint64) {
	evs, next := log.Since(id)
	first := next - uint64(len(evs))
	for i := range evs {
		evs[i].ID = first + uint64(i) + 1
	}
	return evs, next
}

// Changed returns a channel closed on the next Publish - see
// ring.Ring.Changed for how to use it without missing one.
func Changed() <-chan struct{} {
	return log.Changed()
}
