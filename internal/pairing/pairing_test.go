package pairing

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCode(t *testing.T) {
	der := make([]byte, 300)
	_, _ = rand.Read(der)
	c, err := NewCode(der)
	if err != nil {
		t.Fatal(err)
	}
	s := c.String()
	if len(s) != Symbols+4 || strings.Count(s, "-") != 4 {
		t.Fatalf("code %q", s)
	}
	for _, typed := range []string{s, strings.ToLower(s), strings.ReplaceAll(s, "-", ""), " " + strings.ReplaceAll(s, "-", " ") + " "} {
		got, err := ParseCode(typed)
		if err != nil || got != c {
			t.Errorf("ParseCode(%q) = %v, %v; want %v", typed, got, err, c)
		}
	}
	if !c.Names(der) {
		t.Error("the code doesn't name its certificate")
	}
	other := append([]byte(nil), der...)
	other[0] ^= 1
	if c.Names(other) {
		t.Error("the code names another certificate")
	}
	if len(c.SecretBytes()) != 5 || c.Secret>>secretBits != 0 || c.Prefix>>prefixBits != 0 {
		t.Errorf("sizes: secret %d bytes, %x, prefix %x", len(c.SecretBytes()), c.Secret, c.Prefix)
	}

	// O and I/L read as 0 and 1.
	o := strings.NewReplacer("0", "O", "1", "L").Replace(s)
	if got, err := ParseCode(o); err != nil || got != c {
		t.Errorf("O/L for 0/1: %v %v", got, err)
	}
	// One symbol changed, two swapped: caught (31 in 32 - many tries).
	caught := 0
	for i := range 200 {
		b := []byte(strings.ReplaceAll(s, "-", ""))
		p := i % Symbols
		b[p] = alphabet[(strings.IndexByte(alphabet, b[p])+1+i/Symbols)%32]
		if _, err := ParseCode(string(b)); errors.Is(err, ErrMistyped) {
			caught++
		}
	}
	if caught < 180 {
		t.Errorf("only %d mistyped codes of 200 caught", caught)
	}
	for _, bad := range []string{"", s[:len(s)-1], s + "0", strings.Replace(s, s[:1], "U", 1)} {
		if _, err := ParseCode(bad); err == nil {
			t.Errorf("ParseCode(%q) took it", bad)
		}
	}
}

func TestWindow(t *testing.T) {
	secret := []byte{1, 2, 3, 4, 5}
	w := NewWindow(secret, 0)
	for i := 1; i < MaxFailures; i++ {
		var wrong *ErrWrong
		if err := w.Check([]byte{9, 9, 9, 9, 9}); !errors.As(err, &wrong) || wrong.Left != MaxFailures-i {
			t.Fatalf("wrong secret %d: %v", i, err)
		}
	}
	if err := w.Check(secret); err != nil {
		t.Fatalf("the right secret after %d wrong ones: %v", MaxFailures-1, err)
	}
	if err := w.Check([]byte{9}); err == nil || IsClosed(err) {
		t.Fatalf("the last try: %v", err)
	}
	if err := w.Check(secret); !IsClosed(err) || !strings.Contains(err.Error(), "5 wrong codes") {
		t.Errorf("after %d wrong ones: %v", MaxFailures, err)
	}
	select {
	case <-w.Done():
	default:
		t.Error("Done not closed")
	}

	w = NewWindow(secret, 0)
	w.Close("paired")
	w.Close("again")
	if w.Closed() != "paired" || !IsClosed(w.Check(secret)) {
		t.Errorf("closed: %q", w.Closed())
	}

	w = NewWindow(secret, time.Minute)
	w.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if err := w.Check(secret); !IsClosed(err) {
		t.Errorf("past its time: %v", err)
	}
}
