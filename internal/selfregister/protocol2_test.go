package selfregister

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/swenske/Janus/internal/pki"
)

// TestAnnounceNoKey: the keyless announcement carries the node's CA and
// a secret - never a key - and comes back admitted with the fleet's
// trust for a token, or as an enrollment to poll.
func TestAnnounceNoKey(t *testing.T) {
	var body string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		var req announceRequest
		_ = json.Unmarshal(b, &req)
		w.WriteHeader(http.StatusCreated)
		if req.RegistrationToken == "tok" {
			_ = json.NewEncoder(w).Encode(Answer{ID: "n1", Admitted: true, Trust: &Trust{RootCert: "ROOT", Bundle: []byte(`{"payload":""}`)}})
			return
		}
		_ = json.NewEncoder(w).Encode(Answer{ID: "p1"})
	}))
	defer srv.Close()
	ca, err := pki.NewCA("node CA")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Address: srv.Listener.Addr().String(), CACertPEM: certPEM(t, srv), Token: "tok"}

	a, err := Announce(cfg, ca.CertPEM, "lb1", "10.0.0.5:9505", "s3cret")
	if err != nil || !a.Admitted || a.Trust == nil || a.Trust.RootCert != "ROOT" {
		t.Fatalf("with a token: %+v, %v", a, err)
	}
	if strings.Contains(body, "PRIVATE KEY") || !strings.Contains(body, `"protocol":2`) || !strings.Contains(body, `"poll_secret":"s3cret"`) {
		t.Errorf("announcement: %s", body)
	}
	cfg.Token = ""
	if a, err := Announce(cfg, ca.CertPEM, "lb1", "10.0.0.5:9505", "s3cret"); err != nil || a.Admitted || a.ID != "p1" {
		t.Errorf("without a token: %+v, %v", a, err)
	}
}

// TestAnnounceFallback: a Controller without a fleet (409), or older than
// protocol 2 (400 asking for the service credential), makes the node
// fall back; another refusal doesn't.
func TestAnnounceFallback(t *testing.T) {
	for _, c := range []struct {
		code     int
		body     string
		fallback bool
	}{
		{http.StatusConflict, "this Controller's fleet isn't set up", true},
		{http.StatusBadRequest, "name, address, ca_cert_pem, service_cert_pem, and service_key_pem are all required", true},
		{http.StatusBadRequest, "decode request: junk", false},
		{http.StatusInternalServerError, "boom", false},
	} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, c.body, c.code)
		}))
		cfg := &Config{Address: srv.Listener.Addr().String(), CACertPEM: certPEM(t, srv)}
		_, err := Announce(cfg, []byte("CA"), "lb1", "10.0.0.5:9505", "s")
		if errors.Is(err, ErrFallback) != c.fallback || err == nil {
			t.Errorf("%d %q: %v, want fallback %v", c.code, c.body, err, c.fallback)
		}
		srv.Close()
	}
}

// TestPoll: the enrollment's secret goes along; waiting, approved and
// unknown are told apart.
func TestPoll(t *testing.T) {
	var state atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/register/p1" || r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		switch state.Load() {
		case 0:
			w.WriteHeader(http.StatusAccepted)
		case 1:
			_ = json.NewEncoder(w).Encode(Trust{RootCert: "ROOT", Bundle: []byte(`{}`)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := &Config{Address: srv.Listener.Addr().String(), CACertPEM: certPEM(t, srv)}
	if _, err := Poll(cfg, "p1", "s3cret"); !errors.Is(err, ErrPending) {
		t.Errorf("waiting: %v", err)
	}
	state.Store(1)
	if tr, err := Poll(cfg, "p1", "s3cret"); err != nil || tr.RootCert != "ROOT" {
		t.Errorf("approved: %+v, %v", tr, err)
	}
	state.Store(2)
	if _, err := Poll(cfg, "p1", "s3cret"); !errors.Is(err, ErrUnknown) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := Poll(cfg, "p1", "wrong"); err == nil || errors.Is(err, ErrPending) || errors.Is(err, ErrUnknown) {
		t.Errorf("a wrong secret: %v", err)
	}
}

func TestEnrollment(t *testing.T) {
	dir := t.TempDir()
	if e, err := ReadEnrollment(dir); e != nil || err != nil {
		t.Fatalf("none: %+v, %v", e, err)
	}
	if err := SaveEnrollment(dir, Enrollment{ID: "p1", Secret: "s"}); err != nil {
		t.Fatal(err)
	}
	if e, err := ReadEnrollment(dir); err != nil || e.ID != "p1" || e.Secret != "s" {
		t.Fatalf("saved: %+v, %v", e, err)
	}
	if err := RemoveEnrollment(dir); err != nil {
		t.Fatal(err)
	}
	if e, _ := ReadEnrollment(dir); e != nil {
		t.Error("still there after RemoveEnrollment")
	}
	if s, err := NewSecret(); err != nil || len(s) != 64 {
		t.Errorf("secret %q, %v", s, err)
	}
}
