package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shortFetchWaits makes getRetrying's waits a test's.
func shortFetchWaits(t *testing.T) {
	t.Helper()
	prev := fetchWaits
	fetchWaits = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { fetchWaits = prev })
}

// TestFetchAskedAgain: GitHub answering 504 then the file (2026-11-09) -
// the file read, each new try said; a 404 is final at once.
func TestFetchAskedAgain(t *testing.T) {
	shortFetchWaits(t)
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/flaky/uki-b.efi":
			if asked.Add(1) <= 2 {
				http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
				return
			}
			_, _ = io.WriteString(w, "the UKI")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	var mu sync.Mutex
	var notices []string
	ctx := withFetchNotice(context.Background(), func(m string) {
		mu.Lock()
		notices = append(notices, m)
		mu.Unlock()
	})
	got, err := fetchBundleFile(ctx, srv.URL+"/flaky", "uki-b.efi", 1<<20)
	if err != nil || string(got) != "the UKI" || asked.Load() != 3 {
		t.Fatalf("%q, %v after %d asks", got, err, asked.Load())
	}
	if len(notices) != 2 || !strings.Contains(notices[0], "504 Gateway Timeout - trying again in 10ms (2/5)") || !strings.Contains(notices[1], "(3/5)") {
		t.Fatalf("notices: %q", notices)
	}

	notices = nil
	if _, err := fetchBundleFile(ctx, srv.URL+"/flaky", "rootfs.verity", 1<<20); !errors.Is(err, errBundleFileMissing) || len(notices) != 0 {
		t.Fatalf("a 404: %v, %q", err, notices)
	}
}

// TestFetchGivesUp: a server that never answers right - the last failure,
// after every try; and a Retry-After honoured up to maxRetryAfter.
func TestFetchGivesUp(t *testing.T) {
	shortFetchWaits(t)
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	_, err := fetchBundleFile(context.Background(), srv.URL, "rootfs.verity", 1<<20)
	if err == nil || !strings.Contains(err.Error(), "unexpected status 503 Service Unavailable - 5 tries") || asked.Load() != 5 {
		t.Fatalf("%v after %d asks", err, asked.Load())
	}

	// The stream ended while waiting: no more tries.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	t.Cleanup(slow.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = getRetrying(ctx, slow.URL+"/rootfs.squashfs")
	if err == nil || !strings.Contains(err.Error(), "429") || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("waiting on a Retry-After past the deadline: %v after %s", err, time.Since(start))
	}
}

// TestOpenBundleDataAskedAgain: the root filesystem's stream asked again
// too.
func TestOpenBundleDataAskedAgain(t *testing.T) {
	shortFetchWaits(t)
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if asked.Add(1) == 1 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		_, _ = io.WriteString(w, "squashfs bytes")
	}))
	t.Cleanup(srv.Close)
	d, err := openBundleData(context.Background(), srv.URL, "B", "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	b, _ := io.ReadAll(d)
	if string(b) != "squashfs bytes" || asked.Load() != 2 {
		t.Fatalf("%q after %d asks", b, asked.Load())
	}
}
