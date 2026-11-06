package main

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/pairing"
)

const consoleSample = "janusd: listening on :9505\r\n" +
	"pki: ADMIN CERTIFICATE (printed once)\r\n" +
	"-----BEGIN CERTIFICATE-----\r\nMIIBszCCAVmgAwIBAgIQ\r\n-----END CERTIFICATE-----\r\n" +
	"-----BEGIN EC PRIVATE KEY-----\r\nMHcCAQEEIBsecretsecretsecret\r\nAwEHoUQDQgAE\r\n-----END EC PRIVATE KEY-----\r\n" +
	"\x1b[1mJanus\x1b[0m ready -----BEGIN not a marker at all, just text that goes on and on past any marker length-----\r\n" +
	"-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIsecret2\n-----END PRIVATE KEY-----\n" +
	"done\r\n"

const consoleWant = "janusd: listening on :9505\r\n" +
	"pki: ADMIN CERTIFICATE (printed once)\r\n" +
	"-----BEGIN CERTIFICATE-----\r\nMIIBszCCAVmgAwIBAgIQ\r\n-----END CERTIFICATE-----\r\n" +
	keyNotice + "\r\n" +
	"\x1b[1mJanus\x1b[0m ready -----BEGIN not a marker at all, just text that goes on and on past any marker length-----\r\n" +
	keyNotice + "\n" +
	"done\r\n"

// Every split of the output into two writes, then byte by byte: a
// marker cut anywhere is still recognized.
func TestKeyRedactor(t *testing.T) {
	run := func(chunks [][]byte) string {
		var out bytes.Buffer
		r := newKeyRedactor(&out)
		for _, c := range chunks {
			if _, err := r.Write(c); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Flush(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	in := []byte(consoleSample)
	for i := 0; i <= len(in); i++ {
		if got := run([][]byte{in[:i], in[i:]}); got != consoleWant {
			t.Fatalf("split at %d:\n got %q\nwant %q", i, got, consoleWant)
		}
	}
	var bytewise [][]byte
	for i := range in {
		bytewise = append(bytewise, in[i:i+1])
	}
	if got := run(bytewise); got != consoleWant {
		t.Fatalf("byte by byte:\n got %q\nwant %q", got, consoleWant)
	}
	if got := run([][]byte{in}); strings.Contains(got, "secret") {
		t.Fatalf("a key leaked: %q", got)
	}
}

// A console that ends in the middle of a key never lets its start out.
func TestKeyRedactorTruncatedKey(t *testing.T) {
	var out bytes.Buffer
	r := newKeyRedactor(&out)
	_, _ = r.Write([]byte("boot\n-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIBsecret"))
	_ = r.Flush()
	if got := out.String(); got != "boot\n" {
		t.Errorf("got %q", got)
	}
}

// A character cut across two writes arrives whole.
func TestUTF8Chunks(t *testing.T) {
	const text = "J A N U S ▄██▀ · ok"
	in := []byte(text)
	for i := 0; i <= len(in); i++ {
		var got strings.Builder
		u := &utf8Chunks{send: func(s string) error {
			if strings.ContainsRune(s, '\uFFFD') {
				t.Fatalf("split at %d: sent %q", i, s)
			}
			got.WriteString(s)
			return nil
		}}
		_, _ = u.Write(in[:i])
		_, _ = u.Write(in[i:])
		_ = u.Flush()
		if got.String() != text {
			t.Fatalf("split at %d: got %q", i, got.String())
		}
	}
}

// Tiny packets come out as few writes, in order, nothing lost.
func TestCoalescer(t *testing.T) {
	var mu sync.Mutex
	var writes []string
	c := newCoalescer(writerTo(func(p []byte) {
		mu.Lock()
		writes = append(writes, string(p))
		mu.Unlock()
	}), time.Hour)
	for _, b := range []byte("\x1b[38;5;166mJanus\x1b[0m") {
		_, _ = c.Write([]byte{b})
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || writes[0] != "\x1b[38;5;166mJanus\x1b[0m" {
		t.Errorf("writes = %q", writes)
	}
}

type writerTo func(p []byte)

func (f writerTo) Write(p []byte) (int, error) { f(p); return len(p), nil }

// codeSample is what janusd prints of a pairing or a rescue window (its
// sayCode line, the motd's bold banner line), among words that only look
// like a code.
func codeSample(t *testing.T) (in, want, code string) {
	t.Helper()
	c, err := pairing.NewCode([]byte("a node's certificate"))
	if err != nil {
		t.Fatal(err)
	}
	code = c.String()
	if len(code) != codeLen {
		t.Fatalf("a code is %d bytes (%q), the redactor expects %d", len(code), code, codeLen)
	}
	lines := []struct{ in, want string }{
		{"rescue: code " + code + " on :9506 until 10:30 UTC - janusctl access rescue -endpoint ADDRESS -code CODE DIR\r\n",
			"rescue: code " + codeNotice + " on :9506 until 10:30 UTC - janusctl access rescue -endpoint ADDRESS -code CODE DIR\r\n"},
		{"pairing: code " + code + " on :9506 - janusctl fleet adopt NAME -endpoint ADDRESS -code CODE, or the Controller's Add node\n",
			"pairing: code " + codeNotice + " on :9506 - janusctl fleet adopt NAME -endpoint ADDRESS -code CODE, or the Controller's Add node\n"},
		{"  \u2588\u2580\u2580  rescue    \x1b[1m" + code + "\x1b[0m\r\n", "  \u2588\u2580\u2580  rescue    \x1b[1m" + codeNotice + "\x1b[0m\r\n"},
		{"pairing   \x1b[1m" + strings.ToLower(code) + "\x1b[0m\n", "pairing   \x1b[1m" + codeNotice + "\x1b[0m\n"},
		// Not codes: part of a longer word, another shape.
		{"consul: node id 550e8400-e29b-41d4-a716-446655440000\n", "consul: node id 550e8400-e29b-41d4-a716-446655440000\n"},
		{"x" + code + " " + code + "x -" + code + " " + code + "-\n", "x" + code + " " + code + "x -" + code + " " + code + "-\n"},
		{"ABCD-EFGH-JKMN-PQRS ABCD-EFGH-JKMN-PQRS-TVWX-YZ01 ABCDE-FGH-JKMN-PQRS-TVWX\n", "ABCD-EFGH-JKMN-PQRS ABCD-EFGH-JKMN-PQRS-TVWX-YZ01 ABCDE-FGH-JKMN-PQRS-TVWX\n"},
		{"-----BEGIN CERTIFICATE-----\r\n", "-----BEGIN CERTIFICATE-----\r\n"},
		// The last word, the console ended right after it.
		{"rescue    " + code, "rescue    " + codeNotice},
	}
	for _, l := range lines {
		in += l.in
		want += l.want
	}
	return in, want, code
}

// Every split of the output into two writes, then byte by byte: a code
// cut anywhere, or an escape sequence, is still recognized - and nothing
// else changes.
func TestCodeRedactor(t *testing.T) {
	run := func(chunks [][]byte) string {
		var out bytes.Buffer
		r := newCodeRedactor(&out)
		for _, c := range chunks {
			if n, err := r.Write(c); err != nil || n != len(c) {
				t.Fatalf("write: %d, %v", n, err)
			}
		}
		if err := r.Flush(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	s, want, _ := codeSample(t)
	in := []byte(s)
	for i := 0; i <= len(in); i++ {
		if got := run([][]byte{in[:i], in[i:]}); got != want {
			t.Fatalf("split at %d:\n got %q\nwant %q", i, got, want)
		}
	}
	var bytewise [][]byte
	for i := range in {
		bytewise = append(bytewise, in[i:i+1])
	}
	got := run(bytewise)
	if got != want {
		t.Fatalf("byte by byte:\n got %q\nwant %q", got, want)
	}
	if strings.Count(got, codeNotice) != 5 {
		t.Errorf("%d codes hidden, want 5: %q", strings.Count(got, codeNotice), got)
	}
}

// The console as the Controller relays it: the keys and the codes
// hidden, both cut anywhere.
func TestConsoleRedactorsChained(t *testing.T) {
	codes, codesWant, _ := codeSample(t)
	in := []byte(consoleSample + codes)
	want := consoleWant + codesWant
	for i := 0; i <= len(in); i++ {
		var out bytes.Buffer
		c := newCodeRedactor(&out)
		k := newKeyRedactor(c)
		_, _ = k.Write(in[:i])
		_, _ = k.Write(in[i:])
		_ = k.Flush()
		_ = c.Flush()
		if got := out.String(); got != want {
			t.Fatalf("split at %d:\n got %q\nwant %q", i, got, want)
		}
		if strings.Contains(out.String(), "secret") {
			t.Fatalf("split at %d: a key leaked", i)
		}
	}
}

// TestRealConsoleCodeHidden runs a real node's console through the
// Controller's redactors, cut in chunks of every size up to 64:
// JANUS_CONSOLE_LOG is a console log, JANUS_CONSOLE_CODE the pairing or
// rescue code it shows (hack/qemu-rescue-test.sh boots a node with a
// rescue volume). Every occurrence of the code is hidden, nothing else
// changes - the code's lines as janusd prints them today.
func TestRealConsoleCodeHidden(t *testing.T) {
	path, code := os.Getenv("JANUS_CONSOLE_LOG"), os.Getenv("JANUS_CONSOLE_CODE")
	if path == "" || code == "" {
		t.Skip("JANUS_CONSOLE_LOG and JANUS_CONSOLE_CODE: a real console and its code (hack/qemu-rescue-test.sh)")
	}
	in, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := bytes.Count(in, []byte(code))
	if n < 2 {
		t.Fatalf("the console shows the code %d times - the rescue line and the banner expected", n)
	}
	want := bytes.ReplaceAll(in, []byte(code), []byte(codeNotice))
	for size := 1; size <= 64; size++ {
		var out bytes.Buffer
		c := newCodeRedactor(&out)
		k := newKeyRedactor(c)
		for rest := in; len(rest) > 0; {
			m := min(size, len(rest))
			if _, err := k.Write(rest[:m]); err != nil {
				t.Fatal(err)
			}
			rest = rest[m:]
		}
		_ = k.Flush()
		_ = c.Flush()
		if got := out.Bytes(); !bytes.Equal(got, want) {
			i := 0
			for i < len(got) && i < len(want) && got[i] == want[i] {
				i++
			}
			t.Fatalf("chunks of %d: differs at byte %d:\n got %q\nwant %q", size, i, got[max(0, i-80):min(len(got), i+80)], want[max(0, i-80):min(len(want), i+80)])
		}
	}
	t.Logf("the code hidden %d times in %d bytes of console", n, len(in))
}
