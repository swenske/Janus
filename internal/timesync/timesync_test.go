package timesync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/beevik/ntp"
)

// fake answers queries per server: an offset, or an error.
type fake struct {
	answers map[string]time.Duration
	calls   []string
}

func (f *fake) query(addr string) (*ntp.Response, error) {
	f.calls = append(f.calls, addr)
	off, ok := f.answers[addr]
	if !ok {
		return nil, errors.New("i/o timeout")
	}
	// A response Validate accepts: NTPv4, stratum 2, a reference time
	// shortly before the transmit time, a small root distance.
	ref := time.Now()
	return &ntp.Response{Version: 4, ClockOffset: off, RTT: 4 * time.Millisecond, Stratum: 2, Leap: ntp.LeapNoWarning,
		ReferenceTime: ref, Time: ref.Add(time.Millisecond), RootDelay: time.Millisecond, RootDispersion: time.Millisecond}, nil
}

func TestSyncFallsThroughServersAndAdjusts(t *testing.T) {
	f := &fake{answers: map[string]time.Duration{"b.example:123": 300 * time.Millisecond}}
	var adjusted []time.Duration
	s := New(Options{
		Manage:  true,
		Servers: func() ([]string, string) { return []string{"a.example:123", "b.example:123"}, "configured" },
		query:   f.query,
		adjust:  func(off, _ time.Duration) error { adjusted = append(adjusted, off); return nil },
	})
	if err := s.syncOnce(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.calls, ",") != "a.example:123,b.example:123" {
		t.Errorf("queried %v", f.calls)
	}
	if len(adjusted) != 1 || adjusted[0] != 300*time.Millisecond {
		t.Errorf("adjusted %v", adjusted)
	}
	if !s.WaitSynced(0) {
		t.Error("not marked synced after a success")
	}
	st := s.Status()
	if st.GetLastServer() != "b.example:123" || st.GetOffsetNs() != int64(300*time.Millisecond) || st.GetSource() != "configured" || st.GetError() != "" || st.GetStratum() != 2 {
		t.Errorf("status %v", st)
	}
}

func TestSyncReportsFailuresAndDoesntAdjustUnmanaged(t *testing.T) {
	adjusted := 0
	s := New(Options{
		Servers: func() ([]string, string) { return []string{"a.example"}, "default" },
		query:   (&fake{}).query,
		adjust:  func(time.Duration, time.Duration) error { adjusted++; return nil },
	})
	if err := s.syncOnce(); err == nil {
		t.Fatal("no server answered, yet no error")
	}
	if st := s.Status(); !strings.Contains(st.GetError(), "a.example: i/o timeout") {
		t.Errorf("status error %q", st.GetError())
	}
	if s.WaitSynced(10 * time.Millisecond) {
		t.Error("synced without an answer")
	}

	s.opts.query = (&fake{answers: map[string]time.Duration{"a.example": time.Second}}).query
	if err := s.syncOnce(); err != nil || adjusted != 0 {
		t.Errorf("unmanaged: err=%v, clock adjusted %d times", err, adjusted)
	}
}

func TestRunStopsAndKicks(t *testing.T) {
	queries := make(chan string, 10)
	s := New(Options{
		Servers: func() ([]string, string) { return []string{"a.example"}, "dhcp" },
		query:   func(addr string) (*ntp.Response, error) { queries <- addr; return nil, errors.New("down") },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	<-queries
	s.Kick()
	select {
	case <-queries:
	case <-time.After(2 * time.Second):
		t.Fatal("Kick didn't trigger a new query")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't stop")
	}
}
