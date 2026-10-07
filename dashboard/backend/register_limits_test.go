package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/enroll"
	"github.com/swenske/Janus/dashboard/backend/internal/ratelimit"
)

// TestRegisterBounded: the registration port takes a burst of
// announcements from an address, then refuses with 429 and a
// Retry-After; the queue takes maxPendingRegistrations, then refuses
// with 503 - while a node admitted on an enrollment token still gets in.
func TestRegisterBounded(t *testing.T) {
	a, _ := newTestApp(t)
	withFleet(t, a)
	a.registerLimiter = ratelimit.New(2, time.Hour)

	for i := range 2 {
		if rec := call(t, a.handleRegister, "POST", "/register", "/register", keylessRegistration(t, "lb", "")); rec.Code != http.StatusCreated {
			t.Fatalf("announcement %d: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := call(t, a.handleRegister, "POST", "/register", "/register", keylessRegistration(t, "lb", ""))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("third announcement from the same address: %d %s (Retry-After %q)", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
	}
	if n := a.pending.Waiting(); n != 2 {
		t.Fatalf("%d waiting, want 2", n)
	}

	a.registerLimiter = nil
	for a.pending.Waiting() < maxPendingRegistrations {
		if rec := call(t, a.handleRegister, "POST", "/register", "/register", keylessRegistration(t, "lb", "")); rec.Code != http.StatusCreated {
			t.Fatalf("filling the queue: %d %s", rec.Code, rec.Body)
		}
	}
	rec = call(t, a.handleRegister, "POST", "/register", "/register", keylessRegistration(t, "lb", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("with the queue full: %d %s", rec.Code, rec.Body)
	}
	if n := a.pending.Waiting(); n != maxPendingRegistrations {
		t.Fatalf("%d waiting, want %d", n, maxPendingRegistrations)
	}

	// A token admits without queueing: the full queue doesn't stop it.
	es, err := enroll.Open(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	a.enroll = es
	tok, _, err := es.Create("rack", "root", 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec = call(t, a.handleRegister, "POST", "/register", "/register", keylessRegistration(t, "lb-tok", tok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("with a token and the queue full: %d %s", rec.Code, rec.Body)
	}
}
