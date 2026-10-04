package auth

import (
	"strings"
	"testing"
	"time"
)

// TestTOTPVectors: RFC 6238's appendix B, SHA-1, last six digits.
func TestTOTPVectors(t *testing.T) {
	key := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037", 20000000000: "353130"} {
		if got := totpCode(key, uint64(unix/30)); got != want {
			t.Errorf("at %d: %s, want %s", unix, got, want)
		}
	}
}

func TestCheckTOTP(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 32 {
		t.Fatalf("secret %q: want 32 base32 characters", secret)
	}
	key, _ := totpEncoding.DecodeString(secret)
	now := time.Unix(1_800_000_015, 0)
	step := uint64(now.Unix() / 30)
	for name, tc := range map[string]struct {
		code string
		last uint64
		ok   bool
	}{
		"now":                  {totpCode(key, step), 0, true},
		"with spaces":          {totpCode(key, step)[:3] + " " + totpCode(key, step)[3:], 0, true},
		"the step before":      {totpCode(key, step-1), 0, true},
		"the step after":       {totpCode(key, step+1), 0, true},
		"two steps before":     {totpCode(key, step-2), 0, false},
		"used already":         {totpCode(key, step), step, false},
		"after one used later": {totpCode(key, step-1), step, false},
		"short":                {"12345", 0, false},
	} {
		if _, ok := checkTOTP(strings.ToLower(secret), tc.code, now, tc.last); ok != tc.ok {
			t.Errorf("%s: %v, want %v", name, ok, tc.ok)
		}
	}
	if uri := TOTPURI("Janus Controller", "alice", secret); !strings.HasPrefix(uri, "otpauth://totp/Janus%20Controller:alice?") || !strings.Contains(uri, "secret="+secret) {
		t.Errorf("URI %s", uri)
	}
}
