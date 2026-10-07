package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/updater/updaterapi"
)

const repo = "swenske/janus-controller"

// fakePlatform stands in for Docker Compose: "up" starts the image .env
// names - a version listed in broken never checks in, one in migrates
// rewrites the data first, like a new version migrating its files.
type fakePlatform struct {
	user     string // what imageUser answers
	t        *testing.T
	u        *updater
	wd       string
	dataDir  string
	problems []string
	pullErr  error
	broken   map[string]bool
	migrates map[string]bool

	mu      sync.Mutex
	calls   []string
	running string
	upAt    time.Time
}

func (f *fakePlatform) discover(context.Context) (*target, []string) {
	return &target{Project: "p", Service: "janus-controller", WorkingDir: f.wd, ConfigFiles: []string{filepath.Join(f.wd, "compose.yaml")}, Image: f.image()}, f.problems
}

func (f *fakePlatform) checkCompose(context.Context, *target) error { return nil }

func (f *fakePlatform) image() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakePlatform) compose(_ context.Context, _ *target, env []string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(append(append([]string(nil), env...), args...), " "))
	f.mu.Unlock()
	switch args[0] {
	case "pull":
		if f.pullErr != nil {
			return "Error response from daemon: manifest unknown", f.pullErr
		}
	case "up":
		image := repo + ":v2026.10.01"
		if data, err := os.ReadFile(filepath.Join(f.wd, ".env")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if v, ok := strings.CutPrefix(line, "JANUS_CONTROLLER_IMAGE="); ok {
					image = v
				}
			}
		}
		_, tag, _ := strings.Cut(strings.Split(image, "@")[0], ":")
		f.mu.Lock()
		f.running, f.upAt = image, time.Now()
		f.mu.Unlock()
		if f.migrates[tag] {
			if err := os.WriteFile(filepath.Join(f.dataDir, "migrated"), []byte(tag), 0o600); err != nil {
				f.t.Error(err)
			}
		}
		if !f.broken[tag] {
			go func() {
				time.Sleep(20 * time.Millisecond)
				f.u.checkins.add(tag)
			}()
		}
	}
	return "ok", nil
}

func (f *fakePlatform) imageUser(context.Context, string) (string, error) { return f.user, nil }

func (f *fakePlatform) containerState(context.Context, *target) (bool, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return true, f.upAt, nil
}

func (f *fakePlatform) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func setup(t *testing.T, env string) (*updater, *fakePlatform) {
	t.Helper()
	root := t.TempDir()
	f := &fakePlatform{
		t:        t,
		wd:       filepath.Join(root, "compose"),
		dataDir:  filepath.Join(root, "data"),
		broken:   map[string]bool{},
		migrates: map[string]bool{},
		running:  repo + ":v2026.10.01",
	}
	for _, d := range []string{f.wd, f.dataDir, filepath.Join(f.dataDir, "nodes", "n1")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(f.dataDir, "auth.json"), `{"hash":"x"}`, 0o600)
	mustWrite(t, filepath.Join(f.dataDir, "nodes", "n1", "meta.json"), `{"name":"lb1"}`, 0o644)
	if env != "" {
		mustWrite(t, filepath.Join(f.wd, ".env"), env, 0o640)
	}
	cfg := config{
		StateDir:     filepath.Join(root, "state"),
		DataDir:      f.dataDir,
		Service:      "janus-controller",
		Variable:     "JANUS_CONTROLLER_IMAGE",
		Repository:   repo,
		StartTimeout: 300 * time.Millisecond,
		Stable:       10 * time.Millisecond,
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	u := newUpdater(cfg, f)
	f.u = u
	return u, f
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// finished waits for the job to end.
func finished(t *testing.T, u *updater) *updaterapi.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		j := u.copyJob()
		u.mu.Unlock()
		if j != nil && j.State != updaterapi.JobRunning {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the job didn't finish")
	return nil
}

const digest = "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestUpdateSucceeds(t *testing.T) {
	u, f := setup(t, "# the Controller\nJANUS_CONTROLLER_ADDR=:443\n")
	f.migrates["v2026.10.02"] = true
	if _, err := u.start(updaterapi.UpdateRequest{Version: "v2026.10.02", Image: repo + ":v2026.10.02" + digest, FromVersion: "v2026.10.01"}); err != nil {
		t.Fatal(err)
	}
	j := finished(t, u)
	if j.State != updaterapi.JobDone || j.Message != "Updated to v2026.10.02" {
		t.Fatalf("job = %s: %s\n%s", j.State, j.Message, strings.Join(j.Log, "\n"))
	}
	if j.FromImage != repo+":v2026.10.01" {
		t.Errorf("FromImage = %q", j.FromImage)
	}
	want := "# the Controller\nJANUS_CONTROLLER_ADDR=:443\nJANUS_CONTROLLER_IMAGE=" + repo + ":v2026.10.02" + digest + "\n"
	if got := readFile(t, filepath.Join(f.wd, ".env")); got != want {
		t.Errorf(".env = %q, want %q", got, want)
	}
	if fi, _ := os.Stat(filepath.Join(f.wd, ".env")); fi.Mode().Perm() != 0o640 {
		t.Errorf(".env mode = %v, want kept at 0640", fi.Mode().Perm())
	}
	// The pull used the new image without touching .env; the old version
	// was stopped before the backup.
	if !f.called("JANUS_CONTROLLER_IMAGE=" + repo + ":v2026.10.02" + digest + " pull janus-controller") {
		t.Errorf("no pull of the new image: %q", f.calls)
	}
	backup := filepath.Join(u.cfg.StateDir, "backup")
	if got := readFile(t, filepath.Join(backup, "data", "nodes", "n1", "meta.json")); got != `{"name":"lb1"}` {
		t.Errorf("backed-up node = %q", got)
	}
	if _, err := os.Stat(filepath.Join(backup, "data", "migrated")); err == nil {
		t.Error("the backup holds the new version's data, not the old one's")
	}
	if got := readFile(t, filepath.Join(backup, "env")); got != "# the Controller\nJANUS_CONTROLLER_ADDR=:443\n" {
		t.Errorf("backed-up .env = %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(backup, "data", "auth.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("backed-up auth.json mode = %v", fi.Mode().Perm())
	}
}

func TestUpdateRollsBack(t *testing.T) {
	u, f := setup(t, "JANUS_CONTROLLER_IMAGE="+repo+":v2026.10.01\n")
	f.broken["v2026.10.02"] = true
	f.migrates["v2026.10.02"] = true
	if _, err := u.start(updaterapi.UpdateRequest{Version: "v2026.10.02", FromVersion: "v2026.10.01"}); err != nil {
		t.Fatal(err)
	}
	j := finished(t, u)
	if j.State != updaterapi.JobRolledBack || !strings.Contains(j.Message, "v2026.10.02 didn't start within") || !strings.Contains(j.Message, "v2026.10.01 is back") {
		t.Fatalf("job = %s: %s\n%s", j.State, j.Message, strings.Join(j.Log, "\n"))
	}
	if got := readFile(t, filepath.Join(f.wd, ".env")); got != "JANUS_CONTROLLER_IMAGE="+repo+":v2026.10.01\n" {
		t.Errorf(".env = %q, want the old one back", got)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "migrated")); err == nil {
		t.Error("the new version's data change survived the rollback")
	}
	if got := readFile(t, filepath.Join(f.dataDir, "auth.json")); got != `{"hash":"x"}` {
		t.Errorf("auth.json = %q", got)
	}
	if got := f.image(); got != repo+":v2026.10.01" {
		t.Errorf("running %q after the rollback", got)
	}
}

func TestRollbackRemovesANewEnvFile(t *testing.T) {
	u, f := setup(t, "")
	f.broken["v2026.10.02"] = true
	if _, err := u.start(updaterapi.UpdateRequest{Version: "v2026.10.02", FromVersion: "v2026.10.01"}); err != nil {
		t.Fatal(err)
	}
	if j := finished(t, u); j.State != updaterapi.JobRolledBack {
		t.Fatalf("job = %s: %s", j.State, j.Message)
	}
	if _, err := os.Stat(filepath.Join(f.wd, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".env should be gone again: %v", err)
	}
}

func TestRollbackFails(t *testing.T) {
	u, f := setup(t, "")
	f.broken["v2026.10.02"] = true
	f.broken["v2026.10.01"] = true
	if _, err := u.start(updaterapi.UpdateRequest{Version: "v2026.10.02", FromVersion: "v2026.10.01"}); err != nil {
		t.Fatal(err)
	}
	j := finished(t, u)
	if j.State != updaterapi.JobRollbackFailed || !strings.Contains(j.Message, "the previous version (v2026.10.01) didn't start again") {
		t.Fatalf("job = %s: %s", j.State, j.Message)
	}
}

func TestPullFailureChangesNothing(t *testing.T) {
	u, f := setup(t, "A=1\n")
	f.pullErr = errors.New("exit status 1")
	if _, err := u.start(updaterapi.UpdateRequest{Version: "v2026.10.02", FromVersion: "v2026.10.01"}); err != nil {
		t.Fatal(err)
	}
	j := finished(t, u)
	if j.State != updaterapi.JobFailed || !strings.Contains(j.Message, "manifest unknown") {
		t.Fatalf("job = %s: %s", j.State, j.Message)
	}
	if f.called("stop") || f.called("up") {
		t.Errorf("the Controller was touched: %q", f.calls)
	}
	if got := readFile(t, filepath.Join(f.wd, ".env")); got != "A=1\n" {
		t.Errorf(".env = %q", got)
	}
}

func TestIncompleteSetupIsRefused(t *testing.T) {
	u, f := setup(t, "")
	f.problems = []string{"the updater can't see /opt/janus/compose.yaml"}
	if _, err := u.start(updaterapi.UpdateRequest{Version: "v2026.10.02", FromVersion: "v2026.10.01"}); err != nil {
		t.Fatal(err)
	}
	j := finished(t, u)
	if j.State != updaterapi.JobFailed || !strings.Contains(j.Message, "can't see /opt/janus/compose.yaml") {
		t.Fatalf("job = %s: %s", j.State, j.Message)
	}
	if len(f.calls) != 0 {
		t.Errorf("compose ran: %q", f.calls)
	}
}

func TestStartChecksTheRequest(t *testing.T) {
	u, f := setup(t, "")
	f.broken["v2026.10.02"] = true
	for _, req := range []updaterapi.UpdateRequest{
		{Version: "latest", FromVersion: "v2026.10.01"},
		{Version: "v2026.10.02", Image: "attacker/image:v2026.10.02", FromVersion: "v2026.10.01"},
		{Version: "v2026.10.01", FromVersion: "v2026.10.01"},
	} {
		if _, err := u.start(req); err == nil {
			t.Errorf("start(%+v) accepted", req)
		}
	}
	if _, err := u.start(updaterapi.UpdateRequest{Version: "v2026.10.02", FromVersion: "v2026.10.01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.start(updaterapi.UpdateRequest{Version: "v2026.10.03", FromVersion: "v2026.10.01"}); !errors.Is(err, errBusy) {
		t.Errorf("a second update while one runs: %v", err)
	}
	finished(t, u)
}

func TestInterruptedJob(t *testing.T) {
	u, _ := setup(t, "")
	u.mu.Lock()
	u.job = &updaterapi.Job{ID: "1", State: updaterapi.JobRunning, Step: "wait"}
	u.save()
	u.mu.Unlock()

	again := newUpdater(u.cfg, u.p)
	if again.job.State != updaterapi.JobInterrupted || !strings.Contains(again.job.Message, `step "wait"`) {
		t.Fatalf("reloaded job = %+v", again.job)
	}
}

func TestCheckinsOnlyCountAfterSince(t *testing.T) {
	c := newCheckins()
	c.add("v2026.10.02")
	since := time.Now().Add(time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.wait(ctx, "v2026.10.02", since); err == nil {
		t.Fatal("an earlier check-in counted")
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		c.add("v2026.10.01")
		c.add("v2026.10.02")
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, err := c.wait(ctx2, "v2026.10.02", since); err != nil {
		t.Fatal(err)
	}
}

// TestOwnData: the data goes to the new image's numeric user - here the
// one this test runs as, so the walk changes nothing and chown isn't
// needed; an image that runs as root, or names its user, leaves the
// data alone; a bad user string is read as none.
func TestOwnData(t *testing.T) {
	u, f := setup(t, "")
	ctx := context.Background()
	for _, user := range []string{"", "nobody", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), fmt.Sprintf("%d", os.Getuid())} {
		f.user = user
		if err := u.ownData(ctx, "img"); err != nil {
			t.Fatalf("user %q: %v", user, err)
		}
	}
	for in, want := range map[string][3]int{"65532:65532": {65532, 65532, 1}, "65532": {65532, 65532, 1}, "1000:2000": {1000, 2000, 1}, "": {0, 0, 0}, "nobody": {0, 0, 0}, "-1": {0, 0, 0}, "5:x": {0, 0, 0}} {
		uid, gid, ok := parseUser(in)
		if uid != want[0] || gid != want[1] || ok != (want[2] == 1) {
			t.Errorf("parseUser(%q) = %d %d %v, want %v", in, uid, gid, ok, want)
		}
	}
}
