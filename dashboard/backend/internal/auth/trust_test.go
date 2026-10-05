package auth

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// trustSetup: an admin with an authenticator app, at a fixed time; it
// returns a sign-in that just gave its code.
func trustSetup(t *testing.T) (*Store, string, *time.Time, func() string) {
	t.Helper()
	dir := t.TempDir()
	s := open(t, dir)
	s.SetSealer(xorSealer{})
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	if _, err := s.Setup("root", "long-enough", ""); err != nil {
		t.Fatal(err)
	}
	tok, _ := s.NewSession("root", false)
	secret, err := s.StartTOTP(tok)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := TOTPCode(secret, now)
	if _, err := s.EnableTOTP(tok, code); err != nil {
		t.Fatal(err)
	}
	// Each sign-in gives the next step's code: a code is good once.
	signIn := func() string {
		now = now.Add(30 * time.Second)
		tok, _ := s.NewSession("root", false)
		code, _ := TOTPCode(secret, now)
		if err := s.VerifyTOTP(tok, code); err != nil {
			t.Fatal(err)
		}
		return tok
	}
	return s, dir, &now, signIn
}

func TestTrustedBrowser(t *testing.T) {
	s, dir, now, signIn := trustSetup(t)

	cookie, expires, err := s.TrustBrowser(signIn(), "Firefox on Linux", "192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(DefaultTrustBrowserHours * time.Hour); !expires.Equal(want) {
		t.Errorf("trusted until %v, want %v (the default)", expires, want)
	}
	if raw, _ := readFile(t, dir); bytes.Contains(raw, []byte(cookie[17:])) {
		t.Error("the browser's secret is written in the clear")
	}
	if !s.TrustedSignIn("root", cookie) {
		t.Fatal("the trusted browser isn't recognized")
	}
	// The session it opens counts as having given the second factor,
	// marked as trusted.
	tok, _ := s.NewTrustedSession("root")
	u, ss, _ := s.Session(tok, true)
	if got := s.Needs(u, ss); len(got) != 0 || !ss.MFA || !ss.Trusted {
		t.Errorf("a trusted sign-in needs %v (MFA %v, trusted %v)", got, ss.MFA, ss.Trusted)
	}
	// Such a sign-in can't trust a browser again (it gave no factor),
	// until it confirms its second factor.
	if _, _, err := s.TrustBrowser(tok, "x", "y"); err == nil {
		t.Error("a trusted sign-in trusted a browser without a second factor")
	}

	for _, bad := range []string{"", "nope", cookie[:16] + "_wrong", "deadbeefdeadbeef_" + cookie[17:], cookie + "x"} {
		if s.TrustedSignIn("root", bad) {
			t.Errorf("TrustedSignIn(%q) = true", bad)
		}
	}
	if _, err := s.CreateUser("olga", Operator, "long-enough-too", nil); err != nil {
		t.Fatal(err)
	}
	if s.TrustedSignIn("olga", cookie) {
		t.Error("root's browser stood for another account's second factor")
	}
	if list, _ := s.TrustedBrowsers("root"); len(list) != 1 || list[0].Label != "Firefox on Linux" || list[0].Client != "192.0.2.7" || list[0].LastUsedAt == nil {
		t.Errorf("TrustedBrowsers = %+v", list)
	}

	// It runs out.
	*now = now.Add(DefaultTrustBrowserHours * time.Hour)
	if s.TrustedSignIn("root", cookie) {
		t.Error("a browser is still trusted after its hours")
	}
	if list, _ := s.TrustedBrowsers("root"); len(list) != 0 {
		t.Errorf("an expired browser is listed: %+v", list)
	}
}

// TestTrustForgotten: everything that should end a browser's trust does.
func TestTrustForgotten(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(s *Store, dir string) error
	}{
		{"password change", func(s *Store, _ string) error { return s.ChangePassword("root", "long-enough", "another-long-one") }},
		{"second factors reset", func(s *Store, _ string) error { return s.ResetMFA("root") }},
		{"reset from the host", func(_ *Store, dir string) error { _, err := ResetFromHost(dir, "root"); return err }},
		{"policy set to never", func(s *Store, _ string) error {
			p := s.Settings()
			p.TrustBrowserHours = new(int)
			return s.SetSettings(p)
		}},
		{"policy shortened", func(s *Store, _ string) error {
			p := s.Settings()
			p.TrustBrowserHours = ptr(1)
			s.now = func() time.Time { return time.Unix(1_800_000_000, 0).Add(2 * time.Hour) }
			return s.SetSettings(p)
		}},
		{"forgotten by id", func(s *Store, _ string) error {
			list, _ := s.TrustedBrowsers("root")
			return s.ForgetBrowser("root", list[0].ID)
		}},
		{"all forgotten", func(s *Store, _ string) error { return s.ForgetBrowser("root", "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir, _, signIn := trustSetup(t)
			cookie, _, err := s.TrustBrowser(signIn(), "Chrome on Windows", "192.0.2.8")
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.end(s, dir); err != nil {
				t.Fatal(err)
			}
			if s.TrustedSignIn("root", cookie) {
				t.Errorf("still trusted after: %s", tc.name)
			}
		})
	}
}

func TestTrustPolicy(t *testing.T) {
	s, _, now, signIn := trustSetup(t)
	p := s.Settings()
	p.TrustBrowserHours = new(int)
	if err := s.SetSettings(p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TrustBrowser(signIn(), "x", "y"); !errors.Is(err, ErrTrustOff) {
		t.Errorf("trusted a browser with the policy at 0: %v", err)
	}
	p.TrustBrowserHours = ptr(721)
	if err := s.SetSettings(p); err == nil {
		t.Error("721 hours accepted")
	}
	p.TrustBrowserHours = ptr(720)
	if err := s.SetSettings(p); err != nil {
		t.Fatal(err)
	}
	_, expires, err := s.TrustBrowser(signIn(), "x", "y")
	if err != nil || !expires.Equal(now.Add(720*time.Hour)) {
		t.Errorf("trusted until %v (%v), want 30 days", expires, err)
	}
	// An older users.json, without the setting: the default.
	if (Settings{}).TrustHours() != DefaultTrustBrowserHours {
		t.Error("no setting isn't the default")
	}

	// The list keeps the newest few.
	for range maxTrustedBrowsers + 5 {
		if _, _, err := s.TrustBrowser(signIn(), "x", "y"); err != nil {
			t.Fatal(err)
		}
	}
	if list, _ := s.TrustedBrowsers("root"); len(list) != maxTrustedBrowsers {
		t.Errorf("%d browsers kept, want %d", len(list), maxTrustedBrowsers)
	}
}
