package main

import (
	"bytes"
	"strings"
	"testing"
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
