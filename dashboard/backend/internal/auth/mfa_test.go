package auth

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// xorSealer stands in for the master key: reversible, bound to purpose.
type xorSealer struct{}

func (xorSealer) Seal(p []byte, purpose string) ([]byte, error) {
	return append([]byte(purpose+"|"), p...), nil
}

func (xorSealer) Open(s []byte, purpose string) ([]byte, error) {
	out, ok := bytes.CutPrefix(s, []byte(purpose+"|"))
	if !ok {
		return nil, errors.New("sealed for another purpose")
	}
	return out, nil
}

// TestTOTPFactor: set up, then every sign-in gives a code - once each,
// five tries, five minutes; recovery codes once each.
func TestTOTPFactor(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.SetSealer(xorSealer{})
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	if _, err := s.Setup("root", "long-enough", ""); err != nil {
		t.Fatal(err)
	}
	u, _ := s.User("root")
	tok, _ := s.NewSession("root", false)
	_, ss, _ := s.Session(tok, true)
	if got := s.Needs(u, ss); len(got) != 1 || got[0] != "mfa_enroll" {
		t.Fatalf("an admin without a factor needs %v", got)
	}

	secret, err := s.StartTOTP(tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnableTOTP(tok, "000000"); !errors.Is(err, ErrSecondFactor) {
		t.Errorf("enabled with a wrong code: %v", err)
	}
	code, _ := TOTPCode(secret, now)
	codes, err := s.EnableTOTP(tok, code)
	if err != nil || len(codes) != recoveryCodes {
		t.Fatalf("EnableTOTP: %d codes, %v", len(codes), err)
	}
	u, _ = s.User("root")
	_, ss, _ = s.Session(tok, true)
	if got := s.Needs(u, ss); len(got) != 0 {
		t.Errorf("after setting it up: needs %v", got)
	}
	if raw, _ := readFile(t, dir); bytes.Contains(raw, []byte(secret)) {
		t.Error("the secret is written in the clear")
	}

	// A new sign-in waits for a code, not the one used already.
	tok, _ = s.NewSession("root", false)
	_, ss, _ = s.Session(tok, true)
	if got := s.Needs(u, ss); len(got) != 1 || got[0] != "mfa" {
		t.Fatalf("a new sign-in needs %v", got)
	}
	if err := s.VerifyTOTP(tok, code); !errors.Is(err, ErrSecondFactor) {
		t.Errorf("a code used already: %v", err)
	}
	now = now.Add(30 * time.Second)
	code, _ = TOTPCode(secret, now)
	if err := s.VerifyTOTP(tok, code); err != nil {
		t.Fatalf("the next code: %v", err)
	}

	// Recovery codes, once each, any case, without dashes.
	tok, _ = s.NewSession("root", false)
	if err := s.UseRecoveryCode(tok, codes[0][:4]+codes[0][5:]); err != nil {
		t.Fatalf("a recovery code: %v", err)
	}
	tok, _ = s.NewSession("root", false)
	if err := s.UseRecoveryCode(tok, codes[0]); !errors.Is(err, ErrSecondFactor) {
		t.Errorf("a used recovery code: %v", err)
	}
	if u, _ := s.User("root"); len(u.MFA.RecoveryCodes) != recoveryCodes-1 || u.MFA.RecoveryCodes[0] != "" || u.MFA.TOTP != nil {
		t.Error("the view shows secrets, or the count is wrong")
	}

	// Five wrong codes end the sign-in.
	tok, _ = s.NewSession("root", false)
	for i := 1; i < mfaTries; i++ {
		if err := s.VerifyTOTP(tok, "000000"); !errors.Is(err, ErrSecondFactor) {
			t.Fatalf("try %d: %v", i, err)
		}
	}
	if err := s.VerifyTOTP(tok, "000000"); !errors.Is(err, ErrTooManyTries) {
		t.Errorf("the last try: %v", err)
	}
	if _, _, ok := s.Session(tok, true); ok {
		t.Error("the sign-in survived its tries")
	}

	// A sign-in waits five minutes for its factor.
	tok, _ = s.NewSession("root", false)
	now = now.Add(mfaPendingFor + time.Second)
	if _, _, ok := s.Session(tok, true); ok {
		t.Error("a sign-in waited for its second factor too long")
	}

	// The last factor stays while the policy needs it; an admin's reset
	// takes them all, and the sessions.
	if err := s.RemoveTOTP("root", "long-enough"); !errors.Is(err, ErrFactorRequired) {
		t.Errorf("removing the last required factor: %v", err)
	}
	if fresh, err := s.NewRecoveryCodes("root", "long-enough"); err != nil || len(fresh) != recoveryCodes {
		t.Errorf("new recovery codes: %v", err)
	}
	live, _ := s.NewSession("root", true)
	if err := s.ResetMFA("root"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Session(live, true); ok {
		t.Error("a session survived the reset")
	}
	if u, _ := s.User("root"); u.MFA.Enabled() || len(u.MFA.RecoveryCodes) != 0 {
		t.Error("factors survived the reset")
	}
}

// TestMFAPolicy: who must have a second factor.
func TestMFAPolicy(t *testing.T) {
	for policy, want := range map[string][3]bool{"": {false, false, true}, MFAAdmins: {false, false, true}, MFAEveryone: {true, true, true}, MFANobody: {false, false, false}} {
		p := Settings{SessionIdleMinutes: 30, SessionMaxHours: 12, MFARequired: policy}
		if err := p.Check(); err != nil {
			t.Fatal(err)
		}
		if got := [3]bool{p.Requires(Reader), p.Requires(Operator), p.Requires(Admin)}; got != want {
			t.Errorf("%q: %v, want %v", policy, got, want)
		}
	}
	if (Settings{SessionIdleMinutes: 30, SessionMaxHours: 12, MFARequired: "some"}).Check() == nil {
		t.Error("an unknown policy")
	}
	s := open(t, t.TempDir())
	if _, err := s.Setup("root", "long-enough", "sometimes"); err == nil {
		t.Error("setup with an unknown policy")
	}
	if _, err := s.Setup("root", "long-enough", MFANobody); err != nil || s.Settings().MFAPolicy() != MFANobody {
		t.Errorf("setup with no second factor: %v", err)
	}
}
