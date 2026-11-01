package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/ha"
	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// haActive is a Controller with accounts on a data directory of its own,
// its registration port served.
type haActive struct {
	app  *app
	dir  string
	addr string
}

func newHAActive(t *testing.T) *haActive {
	t.Helper()
	dir := t.TempDir()
	authStore, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authStore.Setup("root", "root-password", auth.MFANobody); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := secrets.LoadOrCreate("", dir)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := ha.Open(filepath.Join(dir, ha.Dir), key)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{auth: authStore, store: st, dataDir: dir, masterKeyPath: key.Path, ha: hs}
	srv := httptest.NewUnstartedServer(a.registrationMux())
	srv.TLS = a.registrationTLS()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &haActive{app: a, dir: dir, addr: strings.TrimPrefix(srv.URL, "https://")}
}

func noRestart(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := restartSelf
	restartSelf = func() { n.Add(1) }
	t.Cleanup(func() { restartSelf = prev })
	return &n
}

// archiveFiles lists a copy's files.
func archiveFiles(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		out[h.Name] = string(data)
	}
}

// TestHAPeerRoutesNeedTheToken: the registration port's peer routes let
// in the peers' secret only.
func TestHAPeerRoutesNeedTheToken(t *testing.T) {
	a := newHAActive(t)
	secret, _ := ha.NewSecret()
	if err := a.app.ha.SetPeer(ha.Peer{Name: "b", Address: "b:8443", CertSHA256: strings.Repeat("ab", 32)}, secret, ha.Active, 0); err != nil {
		t.Fatal(err)
	}
	mux := a.app.registrationMux()
	for _, bearer := range []string{"", "bm9wZQ", "Bearer"} {
		for _, r := range []struct{ method, path string }{{"GET", "/ha/state"}, {"GET", "/ha/replica"}, {"POST", "/ha/stepdown"}, {"POST", "/ha/promote"}} {
			req := httptest.NewRequest(r.method, r.path, nil)
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %q: %d", r.method, r.path, bearer, rec.Code)
			}
		}
	}
	if a.app.ha.State().Role != ha.Active || a.app.restored.Load() {
		t.Fatal("a refused request changed the active")
	}
}

// TestStandby: a standby pairs with the active by its token, copies it -
// its data directory and master key, never the HA files -, and a
// switchover hands it a last copy: it is the active one epoch above, on
// the active's accounts and master key; the active is its standby. The
// old active, coming back as the active, finds itself fenced.
func TestStandby(t *testing.T) {
	restarts := noRestart(t)
	active := newHAActive(t)
	token, _, err := active.app.ha.NewPairing(time.Now())
	if err != nil {
		t.Fatal(err)
	}

	sdir := t.TempDir()
	skey, err := secrets.LoadOrCreate("", sdir)
	if err != nil {
		t.Fatal(err)
	}
	shs, err := ha.Open(filepath.Join(sdir, ha.Dir), skey)
	if err != nil {
		t.Fatal(err)
	}
	// The standby's registration port, served once it is promoted.
	var standbyApp atomic.Pointer[app]
	ssrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := standbyApp.Load(); a != nil {
			a.registrationMux().ServeHTTP(w, r)
			return
		}
		http.Error(w, "standby", http.StatusServiceUnavailable)
	}))
	ssrv.TLS = (&app{ha: shs}).registrationTLS()
	ssrv.StartTLS()
	defer ssrv.Close()

	ctx := context.Background()
	if _, err := pairStandby(ctx, shs, active.addr, "janus-standby_"+strings.Repeat("00", 32)+"_x", "b", strings.TrimPrefix(ssrv.URL, "https://")); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("another active's token: %v", err)
	}
	resp, err := pairStandby(ctx, shs, active.addr, token, "b", strings.TrimPrefix(ssrv.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	if st := shs.State(); st.Role != ha.Standby || st.Peer == nil || st.Peer.Address != active.addr || resp.Epoch != 0 {
		t.Fatalf("the standby: %+v", st)
	}
	if st := active.app.ha.State(); st.Peer == nil || st.Peer.Name != "b" {
		t.Fatalf("the active: %+v", st)
	}
	if _, err := pairStandby(ctx, mustOpenHA(t, t.TempDir()), active.addr, token, "c", "c:8443"); err == nil {
		t.Fatal("the token paired twice")
	}

	sb := &standby{ha: shs, dataDir: sdir, masterKeyPath: skey.Path}
	if err := sb.pull(ctx); err != nil {
		t.Fatal(err)
	}
	archive, rep, err := shs.LastReplica()
	if err != nil {
		t.Fatal(err)
	}
	files := archiveFiles(t, archive)
	if files["controller/data/users.json"] == "" || files["controller/master.key"] == "" {
		t.Errorf("the copy lacks the accounts or the master key: %d files", len(files))
	}
	for name := range files {
		if strings.HasPrefix(name, "controller/data/ha/") || strings.HasPrefix(name, "nodes/") || strings.HasSuffix(name, dataLockFile) {
			t.Errorf("the copy holds %s", name)
		}
	}
	if rep.Version != version || active.app.ha.Seen().At.IsZero() || active.app.ha.Seen().Version != version {
		t.Errorf("the copy's version %q, the active saw it go: %v", rep.Version, active.app.ha.Seen())
	}

	// A change after the last copy: the switchover's own copy has it.
	if _, err := active.app.auth.CreateUser("late", auth.Reader, "late-password-1", nil); err != nil {
		t.Fatal(err)
	}
	msg, err := sb.promote(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "epoch 1") || !strings.Contains(msg, "handed over") {
		t.Errorf("promote: %s", msg)
	}
	for i := 0; i < 100 && restarts.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if st := active.app.ha.State(); st.Role != ha.Standby || st.Epoch != 0 || !active.app.restored.Load() || restarts.Load() != 1 {
		t.Fatalf("the old active: %+v, answers %v, restarts %d", st, !active.app.restored.Load(), restarts.Load())
	}
	users, err := os.ReadFile(filepath.Join(sdir, "users.json"))
	if err != nil || !strings.Contains(string(users), `"late"`) {
		t.Fatalf("the promoted standby lacks the last change: %v", err)
	}
	activeKey, _ := os.ReadFile(filepath.Join(active.dir, "master.key"))
	if key, _ := os.ReadFile(filepath.Join(sdir, "master.key")); !bytes.Equal(key, activeKey) {
		t.Fatal("the promoted standby hasn't the active's master key")
	}
	nkey, err := secrets.LoadOrCreate("", sdir)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := ha.Open(filepath.Join(sdir, ha.Dir), nkey)
	if err != nil {
		t.Fatalf("the HA files after the promotion: %v", err)
	}
	if st := promoted.State(); st.Role != ha.Active || st.Epoch != 1 {
		t.Fatalf("promoted: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(sdir, restoredFile)); err != nil {
		t.Errorf("no note for the first start: %v", err)
	}

	// The old active comes back as the active (a promotion it missed):
	// the promoted peer answers above it, it is fenced.
	standbyApp.Store(&app{ha: promoted})
	if err := active.app.ha.SetRole(ha.Active, 0); err != nil {
		t.Fatal(err)
	}
	if peer, fenced := fencedAtStart(active.app.ha); !fenced || peer.Epoch != 1 {
		t.Errorf("not fenced: %+v", peer)
	}
}

func mustOpenHA(t *testing.T, dir string) *ha.Store {
	t.Helper()
	key, err := secrets.LoadOrCreate("", dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ha.Open(filepath.Join(dir, ha.Dir), key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSetAside: a Controller turned standby keeps its HA files, its lock
// and its master key; the rest goes aside.
func TestSetAside(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"users.json", "master.key", dataLockFile, "ha/state.json", "store/nodes.json"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dest, err := setAside(dir, filepath.Join(dir, "master.key"), time.Now())
	if err != nil || dest == "" {
		t.Fatal(err)
	}
	for _, f := range []string{"master.key", dataLockFile, "ha/state.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s set aside", f)
		}
	}
	for _, f := range []string{"users.json", "store/nodes.json"} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("%s not set aside: %v", f, err)
		}
	}
	if again, err := setAside(dir, filepath.Join(dir, "master.key"), time.Now()); err != nil || again != "" {
		t.Errorf("nothing left to set aside: %q %v", again, err)
	}
}
