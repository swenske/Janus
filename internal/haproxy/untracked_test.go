package haproxy

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/events"
)

// An HAProxy started outside the Manager, listening, is reported - the
// Manager's own isn't, nor one that stopped listening.
func TestUntracked(t *testing.T) {
	r := newRestartRig(t, "", time.Second, time.Second, time.Minute)
	if _, err := r.m.Apply(r.cfg(`"janusd's\n"`)); err != nil {
		t.Fatal(err)
	}
	if found, err := r.m.Untracked(); err != nil || len(found) != 0 {
		t.Fatalf("the Manager's own HAProxy alone: %+v, %v", found, err)
	}

	// The stray: the same binary, another configuration and port.
	port := freePort(t)
	cfg := filepath.Join(t.TempDir(), "stray.cfg")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `defaults
    mode http
    timeout connect 1s
    timeout client 1s
    timeout server 1s
frontend stray
    bind 127.0.0.1:%d
    http-request return status 200 content-type text/plain string "stray\n"
`, port), 0o644); err != nil {
		t.Fatal(err)
	}
	stray := exec.Command(r.real, "-f", cfg)
	if err := stray.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stray.Process.Kill(); _ = stray.Wait() })
	want := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))
	var found []Untracked
	for range 100 {
		found, _ = r.m.Untracked()
		if len(found) == 1 && len(found[0].Listeners) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(found) != 1 || found[0].Pid != stray.Process.Pid || len(found[0].Listeners) != 1 || found[0].Listeners[0] != want ||
		len(found[0].Args) != 3 || found[0].Args[2] != cfg || found[0].Started.IsZero() || time.Since(found[0].Started) > time.Minute {
		t.Fatalf("got %+v, want pid %d listening on %s", found, stray.Process.Pid, want)
	}

	// Watched: reported once it's seen twice, counted while it lasts.
	_, last := events.Since(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.m.WatchUntracked(ctx, 50*time.Millisecond)
	var reported []events.Event
	for range 250 {
		var evs []events.Event
		evs, last = events.Since(last)
		for _, e := range evs {
			if e.Type == "haproxy.untracked" {
				reported = append(reported, e)
			}
		}
		if len(reported) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(reported) != 1 || !strings.Contains(string(reported[0].Payload), fmt.Sprintf(`"pid":%d`, stray.Process.Pid)) ||
		!strings.Contains(string(reported[0].Payload), want.String()) {
		t.Fatalf("haproxy.untracked events: %+v", reported)
	}
	if n := r.m.Counters().UntrackedListening.Load(); n != 1 {
		t.Errorf("counted %d, want 1", n)
	}
	_ = stray.Process.Kill()
	_ = stray.Wait()
	for range 100 {
		if r.m.Counters().UntrackedListening.Load() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("still counted once it's gone")
}
