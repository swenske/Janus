package ha

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
)

func newKey(t *testing.T) *secrets.Key {
	t.Helper()
	k, err := secrets.LoadOrCreate(filepath.Join(t.TempDir(), "master.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func open(t *testing.T, dir string, key Sealer) *Store {
	t.Helper()
	s, err := Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestStateKept: a Controller on its own is active at epoch 0; its peer,
// role and epoch - and its HA certificate - outlive a restart.
func TestStateKept(t *testing.T) {
	dir, key := t.TempDir(), newKey(t)
	s := open(t, dir, key)
	if st := s.State(); st.Role != Active || st.Epoch != 0 || st.Peer != nil {
		t.Fatalf("a new store: %+v", st)
	}
	if s.Authorized("") || s.Client() != nil {
		t.Fatal("unpaired, yet a peer lets in")
	}
	secret, _ := NewSecret()
	p := Peer{Name: "b", Address: "192.0.2.2:8443", CertSHA256: strings.Repeat("ab", 32), PairedAt: time.Now().UTC()}
	if err := s.SetPeer(p, secret, Standby, 3); err != nil {
		t.Fatal(err)
	}
	again := open(t, dir, key)
	st := again.State()
	if st.Role != Standby || st.Epoch != 3 || st.Peer == nil || st.Peer.Address != p.Address {
		t.Fatalf("reopened: %+v", st)
	}
	if again.CertSHA256() != s.CertSHA256() || !again.Authorized(s.Bearer()) || again.Authorized("bm9wZQ") {
		t.Fatal("reopened: another certificate, or the secret lost")
	}
	if err := again.SetRole(Active, 4); err != nil {
		t.Fatal(err)
	}
	if st := open(t, dir, key).State(); st.Role != Active || st.Epoch != 4 || st.Peer == nil {
		t.Fatalf("after SetRole: %+v", st)
	}
	if err := again.Unpair(); err != nil {
		t.Fatal(err)
	}
	if st := open(t, dir, key).State(); st.Peer != nil || st.Epoch != 4 || again.Authorized(s.Bearer()) {
		t.Fatalf("unpaired: %+v", st)
	}
	if _, err := Open(dir, newKey(t)); err == nil {
		t.Error("opened with another master key")
	}
}

// TestPairing: a token pins the active's certificate, is good once and
// for an hour - the one before it no longer.
func TestPairing(t *testing.T) {
	s := open(t, t.TempDir(), newKey(t))
	now := time.Now()
	first, _, err := s.NewPairing(now)
	if err != nil {
		t.Fatal(err)
	}
	token, exp, err := s.NewPairing(now)
	if err != nil || !exp.Equal(now.Add(TokenLifetime)) {
		t.Fatal(err)
	}
	fp, secret, err := ParseToken(token)
	if err != nil || fp != s.CertSHA256() {
		t.Fatalf("ParseToken: %v, %s", err, fp)
	}
	_, firstSecret, _ := ParseToken(first)
	if err := s.UsePairing(firstSecret, now); !errors.Is(err, ErrPairing) {
		t.Errorf("the token before: %v", err)
	}
	if err := s.UsePairing(secret, now.Add(TokenLifetime)); !errors.Is(err, ErrPairing) {
		t.Errorf("expired: %v", err)
	}
	if err := s.UsePairing(secret, now.Add(time.Minute)); err != nil {
		t.Fatalf("good: %v", err)
	}
	if err := s.UsePairing(secret, now.Add(time.Minute)); !errors.Is(err, ErrPairing) {
		t.Errorf("used twice: %v", err)
	}
	for _, bad := range []string{"", "janus-standby_", "janus-standby_abc_def", "janus_x_y", "janus-standby_" + strings.Repeat("zz", 32) + "_s"} {
		if _, _, err := ParseToken(bad); err == nil {
			t.Errorf("ParseToken(%q) took it", bad)
		}
	}
}

func TestDecisions(t *testing.T) {
	for _, c := range []struct {
		own   State
		role  string
		epoch uint64
		want  bool
	}{
		{State{Role: Active, Epoch: 1}, Active, 2, true},
		{State{Role: Active, Epoch: 2}, Active, 2, false},
		{State{Role: Active, Epoch: 2}, Active, 1, false},
		{State{Role: Active, Epoch: 1}, Standby, 5, false},
		{State{Role: Standby, Epoch: 1}, Active, 5, false},
	} {
		if got := StepDown(c.own, c.role, c.epoch); got != c.want {
			t.Errorf("StepDown(%+v, %s, %d) = %v", c.own, c.role, c.epoch, got)
		}
	}
	now := time.Now()
	if MayPromote(now.Add(-time.Minute), now, false) == nil {
		t.Error("promoted while the active answered a minute ago")
	}
	if MayPromote(now.Add(-time.Minute), now, true) != nil || MayPromote(now.Add(-QuietFor), now, false) != nil || MayPromote(time.Time{}, now, false) != nil {
		t.Error("refused a forced promotion, or one after the active went quiet")
	}
}

// TestReplica: the standby's last copy, sealed; a master key put back by
// the promotion seals what's kept again - the copy, spent, goes.
func TestReplica(t *testing.T) {
	dir, key := t.TempDir(), newKey(t)
	s := open(t, dir, key)
	if _, _, err := s.LastReplica(); !errors.Is(err, ErrNoReplica) {
		t.Fatalf("no copy: %v", err)
	}
	at := time.Now().UTC()
	if err := s.SaveReplica([]byte("the archive"), Replica{At: at, Epoch: 2, Version: "v2026.11.01"}); err != nil {
		t.Fatal(err)
	}
	if err := s.NotePull(at.Add(time.Minute), true, 3, errors.New("refused")); err != nil {
		t.Fatal(err)
	}
	data, r, err := s.LastReplica()
	if err != nil || string(data) != "the archive" || r.Epoch != 2 || r.Size != len("the archive") {
		t.Fatalf("LastReplica: %v %q %+v", err, data, r)
	}
	p := s.Pulls()
	if p.PeerEpoch != 3 || p.Error != "refused" || !p.LastContact.Equal(at.Add(time.Minute)) {
		t.Fatalf("pulls: %+v", p)
	}
	if _, rp, err := ReadPulls(dir); err != nil || rp.Last == nil || rp.Last.Version != "v2026.11.01" {
		t.Fatalf("ReadPulls: %v %+v", err, rp)
	}

	secret, _ := NewSecret()
	if err := s.SetPeer(Peer{Name: "a", Address: "a:8443", CertSHA256: strings.Repeat("cd", 32)}, secret, Standby, 3); err != nil {
		t.Fatal(err)
	}
	next := newKey(t)
	if err := s.Reseal(next); err != nil {
		t.Fatal(err)
	}
	again := open(t, dir, next)
	if !again.Authorized(s.Bearer()) || again.CertSHA256() != s.CertSHA256() {
		t.Fatal("resealed: the secret or the certificate lost")
	}
	if _, _, err := again.LastReplica(); !errors.Is(err, ErrNoReplica) {
		t.Errorf("the spent copy kept: %v", err)
	}
}

// TestStepDownForgetsOldCopies: a Controller stepping down again has no
// copy of its earlier time as a standby to show - or promote from - only
// copies of its new active (v2026.11.02-2's lab: "3h36m ago, epoch 5").
func TestStepDownForgetsOldCopies(t *testing.T) {
	s := open(t, t.TempDir(), newKey(t))
	if err := s.SetRole(Standby, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveReplica([]byte("epoch 5's"), Replica{At: time.Now().UTC(), Epoch: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(Active, 6); err != nil {
		t.Fatal(err)
	}
	if p := s.Pulls(); p.Last == nil {
		t.Fatal("the copy's notes went at the promotion already")
	}
	if err := s.SetRole(Standby, 7); err != nil {
		t.Fatal(err)
	}
	if p := s.Pulls(); p.Last != nil || !p.LastContact.IsZero() || p.PeerEpoch != 0 {
		t.Errorf("stepped down, the old pulls kept: %+v", p)
	}
	if _, _, err := s.LastReplica(); !errors.Is(err, ErrNoReplica) {
		t.Errorf("stepped down, the old copy kept: %v", err)
	}
	if err := s.SaveReplica([]byte("epoch 7's"), Replica{At: time.Now().UTC(), Epoch: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(Standby, 7); err != nil { // standby to standby: its copies stay
		t.Fatal(err)
	}
	if data, r, err := s.LastReplica(); err != nil || string(data) != "epoch 7's" || r.Epoch != 7 {
		t.Errorf("a standby's own copy: %q %+v %v", data, r, err)
	}
}

// TestClientPins: the client reaches a peer serving the certificate it
// pinned, carrying the secret; another certificate is refused before
// anything is sent.
func TestClientPins(t *testing.T) {
	active := open(t, t.TempDir(), newKey(t))
	secret, _ := NewSecret()
	if err := active.SetPeer(Peer{Name: "b", Address: "b:8443", CertSHA256: strings.Repeat("ef", 32)}, secret, Active, 7); err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.TLS.ServerName
		if !active.Authorized(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(PeerState{Role: Active, Epoch: 7})
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*active.Certificate()}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")

	st, err := NewClient(addr, active.CertSHA256(), active.Bearer()).State(context.Background())
	if err != nil || st.Epoch != 7 || <-seen != ServerName {
		t.Fatalf("State: %v %+v", err, st)
	}
	if _, err := NewClient(addr, active.CertSHA256(), "").State(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("without the secret: %v", err)
	}
	<-seen
	if _, err := NewClient(addr, strings.Repeat("00", 32), active.Bearer()).State(context.Background()); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Errorf("another certificate: %v", err)
	}
}
