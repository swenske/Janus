package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestLoginLimiter(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewLoginLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < freeFailures; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("attempt %d refused within the free failures", i+1)
		}
		l.Fail("a")
	}
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("the free failures alone must not lock")
	}

	// Each failure past the free ones doubles the lockout.
	for _, want := range []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		l.Fail("a")
		ok, wait := l.Allow("a")
		if ok || wait != want {
			t.Fatalf("after a failure: allowed=%v wait=%v, want locked for %v", ok, wait, want)
		}
		if ok, _ := l.Allow("b"); !ok {
			t.Fatal("another address must not be locked out")
		}
		now = now.Add(want)
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("still locked once %v passed", want)
		}
	}

	// Capped.
	for i := 0; i < 40; i++ {
		l.Fail("a")
	}
	if _, wait := l.Allow("a"); wait != maxLockout {
		t.Fatalf("lockout = %v, want the %v cap", wait, maxLockout)
	}

	// A success forgets everything.
	now = now.Add(maxLockout)
	l.Succeed("a")
	l.Fail("a")
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a success must reset the failure count")
	}

	// Old failures are forgotten: an address that exhausted its free
	// failures long ago starts over.
	for i := 0; i < freeFailures; i++ {
		l.Fail("d")
	}
	now = now.Add(forgetAfter + time.Second)
	l.Fail("d")
	if ok, _ := l.Allow("d"); !ok {
		t.Fatal("failures older than forgetAfter should be forgotten")
	}
}

func TestLoginLimiterBounded(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewLoginLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i < maxTracked+50; i++ {
		now = now.Add(time.Millisecond)
		l.Fail(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	l.mu.Lock()
	n := len(l.addrs)
	_, newest := l.addrs[fmt.Sprintf("10.0.%d.%d", (maxTracked+49)/256, (maxTracked+49)%256)]
	l.mu.Unlock()
	if n > maxTracked || !newest {
		t.Fatalf("tracked %d addresses (max %d), newest kept: %v", n, maxTracked, newest)
	}
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMigration: the single admin password of an older Controller is the
// account "admin", an admin - its file left for a rollback.
func TestMigration(t *testing.T) {
	dir := t.TempDir()
	h, _ := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	raw, _ := json.Marshal(map[string][]byte{"password_hash": h})
	if err := os.WriteFile(filepath.Join(dir, legacyFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s := open(t, dir)
	if s.SetupRequired() {
		t.Fatal("setup required after migration")
	}
	u, err := s.Authenticate(LegacyAdmin, "old-password")
	if err != nil || u.Role != Admin || u.MustChangePassword {
		t.Fatalf("admin after migration: %+v, %v", u, err)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyFile)); err != nil {
		t.Errorf("the old file is gone: %v", err)
	}
	// Once accounts exist, the old file is never read again.
	if _, err := open(t, dir).UpdateUser(LegacyAdmin, Change{Password: ptr("new-password")}); err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, dir).Authenticate(LegacyAdmin, "new-password"); err != nil {
		t.Errorf("reopened: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestSetupAndSignIn(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if !s.SetupRequired() {
		t.Fatal("a new store needs its setup")
	}
	for _, bad := range [][2]string{{"Admin", "long-enough"}, {"", "long-enough"}, {"admin", "short"}, {"admin", strings.Repeat("x", 73)}} {
		if _, err := s.Setup(bad[0], bad[1]); err == nil {
			t.Errorf("Setup(%q, %d characters) accepted", bad[0], len(bad[1]))
		}
	}
	if _, err := s.Setup("root", "long-enough"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Setup("other", "long-enough"); err == nil {
		t.Error("a second setup")
	}
	if fi, _ := os.Stat(filepath.Join(dir, usersFile)); fi.Mode().Perm() != 0o600 {
		t.Errorf("users.json is %v", fi.Mode().Perm())
	}
	for _, bad := range [][2]string{{"root", "wrong-password"}, {"nobody", "long-enough"}} {
		if _, err := s.Authenticate(bad[0], bad[1]); !errors.Is(err, ErrInvalid) {
			t.Errorf("Authenticate(%q) = %v", bad[0], err)
		}
	}
	u, err := s.Authenticate("root", "long-enough")
	if err != nil || u.PasswordHash != nil || u.LastLoginAt == nil {
		t.Fatalf("Authenticate: %+v, %v", u, err)
	}
}

// TestSessionPolicy: a session ends idle or too old; only a request the
// user made keeps it alive.
func TestSessionPolicy(t *testing.T) {
	s := open(t, t.TempDir())
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	if _, err := s.Setup("root", "long-enough"); err != nil {
		t.Fatal(err)
	}
	tok, err := s.NewSession("root")
	if err != nil {
		t.Fatal(err)
	}
	live := func(touch bool) bool {
		_, _, ok := s.Session(tok, touch)
		return ok
	}
	now = now.Add(29 * time.Minute)
	if !live(false) {
		t.Fatal("ended before its idle time")
	}
	now = now.Add(2 * time.Minute)
	if live(false) {
		t.Fatal("a refresh the page made kept the session alive")
	}

	tok, _ = s.NewSession("root")
	for i := 0; i < 11*3; i++ { // used every 20 minutes for 11 hours
		now = now.Add(20 * time.Minute)
		if !live(true) {
			t.Fatalf("used, ended after %v", time.Duration(i+1)*20*time.Minute)
		}
	}
	now = now.Add(time.Hour)
	if live(true) {
		t.Fatal("outlived its 12 hours")
	}

	if err := s.SetSettings(Settings{SessionIdleMinutes: 2, SessionMaxHours: 1}); err == nil {
		t.Error("an idle time under 5 minutes")
	}
	if err := s.SetSettings(Settings{SessionIdleMinutes: 120, SessionMaxHours: 1}); err == nil {
		t.Error("idle longer than the session")
	}
	if err := s.SetSettings(Settings{SessionIdleMinutes: 5, SessionMaxHours: 24}); err != nil {
		t.Fatal(err)
	}
	tok, _ = s.NewSession("root")
	now = now.Add(6 * time.Minute)
	if live(false) {
		t.Error("the new policy doesn't apply")
	}
}

// TestAccounts: what an admin changes ends the sessions it should, and
// never leaves no enabled admin.
func TestAccounts(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := s.Setup("root", "long-enough"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("root", Reader, "long-enough"); err == nil {
		t.Error("a second account with the same name")
	}
	if _, err := s.CreateUser("bob", "boss", "long-enough"); err == nil {
		t.Error("an unknown role")
	}
	bob, err := s.CreateUser("bob", Operator, "given-by-admin")
	if err != nil || !bob.MustChangePassword {
		t.Fatalf("CreateUser: %+v, %v", bob, err)
	}
	tok, _ := s.NewSession("bob")

	if err := s.ChangePassword("bob", "wrong", "bobs-own-password"); err == nil {
		t.Error("changed with a wrong current password")
	}
	if err := s.ChangePassword("bob", "given-by-admin", "bobs-own-password"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Session(tok, true); ok {
		t.Error("a session from before the password change")
	}
	if u, _ := s.User("bob"); u.MustChangePassword {
		t.Error("still must change it")
	}

	tok, _ = s.NewSession("bob")
	if _, err := s.UpdateUser("bob", Change{Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Session(tok, true); ok {
		t.Error("a disabled account's session")
	}
	if _, err := s.Authenticate("bob", "bobs-own-password"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a disabled account signs in: %v", err)
	}

	// A role change applies to live sessions at once - and never takes
	// the last admin away.
	if _, err := s.UpdateUser("bob", Change{Disabled: ptr(false), Role: ptr(Admin)}); err != nil {
		t.Fatal(err)
	}
	tok, _ = s.NewSession("bob")
	if _, err := s.UpdateUser("root", Change{Role: ptr(Reader)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateUser("bob", Change{Role: ptr(Reader)}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demoting the last admin: %v", err)
	}
	if _, err := s.UpdateUser("bob", Change{Disabled: ptr(true)}); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("disabling the last admin: %v", err)
	}
	if err := s.DeleteUser("bob"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("deleting the last admin: %v", err)
	}
	if u, _, ok := s.Session(tok, true); !ok || u.Role != Admin {
		t.Errorf("bob's session: %+v %v", u, ok)
	}
	if err := s.DeleteUser("root"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.User("root"); ok {
		t.Error("root survived its deletion")
	}
	if got := open(t, dir).Users(); len(got) != 1 || got[0].Name != "bob" || got[0].PasswordHash != nil {
		t.Errorf("reopened: %+v", got)
	}
}

// TestResetFromHost: run while the Controller runs, the reset ends the
// account's sessions there and its new password works there.
func TestResetFromHost(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := s.Setup("root", "long-enough"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateUser("root", Change{Role: ptr(Admin)}); err != nil {
		t.Fatal(err)
	}
	tok, _ := s.NewSession("root")
	time.Sleep(10 * time.Millisecond) // a modification time of its own

	pw, err := ResetFromHost(dir, "root")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Session(tok, true); ok {
		t.Error("a session from before the reset")
	}
	u, err := s.Authenticate("root", pw)
	if err != nil || !u.MustChangePassword {
		t.Fatalf("the new password: %+v, %v", u, err)
	}

	// A name that doesn't exist becomes an admin.
	pw, err = ResetFromHost(dir, "rescue")
	if err != nil {
		t.Fatal(err)
	}
	if u, err := s.Authenticate("rescue", pw); err != nil || u.Role != Admin {
		t.Errorf("rescue: %+v, %v", u, err)
	}
}

func TestSessionsSweptOnCreate(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Setup("root", "long-enough"); err != nil {
		t.Fatal(err)
	}
	s.sessions["stale"] = &Session{User: "root", Created: time.Now().Add(-13 * time.Hour), LastSeen: time.Now().Add(-time.Hour)}
	live, err := s.NewSession("root")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.sessions["stale"]; ok {
		t.Error("an expired session survived NewSession")
	}
	if _, _, ok := s.Session(live, true); !ok {
		t.Error("the new session isn't valid")
	}
}
