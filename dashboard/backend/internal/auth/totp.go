package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238's default, what every authenticator app computes
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP (RFC 6238): HMAC-SHA1, 30-second steps, 6 digits - what every
// authenticator app (and Bitwarden, 1Password...) computes by default.
const (
	totpStep   = 30 * time.Second
	totpDigits = 6
	// totpSkew accepts the step before and after the current one: a
	// phone's clock a little off, a code typed as it changes.
	totpSkew = 1
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret is a random 160-bit secret (RFC 4226's recommended
// length), base32 as authenticator apps take it.
func NewTOTPSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return totpEncoding.EncodeToString(raw), nil
}

// TOTPURI is the otpauth:// URI an authenticator app reads from a QR code.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {fmt.Sprint(totpDigits)}, "period": {fmt.Sprint(int(totpStep.Seconds()))}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// totpCode is the code of secret for time step counter.
func totpCode(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000)
}

// checkTOTP returns the time step code matches for secret at now - within
// the skew, and after last, the step of the code used before: a code is
// good once.
func checkTOTP(secret, code string, now time.Time, last uint64) (uint64, bool) {
	key, err := totpEncoding.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return 0, false
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	now64 := uint64(now.Unix()) / uint64(totpStep.Seconds())
	for d := -totpSkew; d <= totpSkew; d++ {
		step := uint64(int64(now64) + int64(d))
		if step <= last {
			continue
		}
		if hmac.Equal([]byte(totpCode(key, step)), []byte(code)) {
			return step, true
		}
	}
	return 0, false
}

// TOTPCode is the code an authenticator app shows for secret at t - for
// tests and scripts driving a sign-in.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := totpEncoding.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", err
	}
	return totpCode(key, uint64(t.Unix())/uint64(totpStep.Seconds())), nil
}
