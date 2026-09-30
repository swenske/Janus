package haproxy

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrInvalidArgument marks an argument refused before anything is sent
// to the stats socket - callers (internal/api) map it to
// codes.InvalidArgument.
var ErrInvalidArgument = errors.New("invalid argument")

// Every runtime API command is built by pasting caller-supplied values
// (map/ACL/backend/server/certificate names, keys, values) into one text
// line sent to a "level admin" socket. HAProxy's CLI runs each line as a
// command, splits a line into several commands on ";" and into arguments
// on spaces. Pasted raw, a value could therefore append any admin
// command of its own - including through MapGet, which os:reader may call
// (internal/api/authz.go) - and a map value with a space was silently cut
// at the first one. Every argument goes through cliToken or cliText,
// which refuse what can't be escaped (control characters, a newline
// above all) and escape the rest with the CLI's own backslash syntax.
// Confirmed against this project's HAProxy build: `add map M k a\;b`
// stores "a;b", `value\ with\ spaces` stores "value with spaces", `a\\b`
// stores "a\b".

// cliEscape backslash-escapes the characters HAProxy's CLI parser gives a
// meaning to: the escape character itself, the command separator and the
// argument separator.
func cliEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `;`, `\;`, ` `, `\ `).Replace(s)
}

// cliToken validates and escapes a name or key: non-empty, no whitespace
// and no control character. Whitespace is refused rather than escaped
// because HAProxy prints names and keys back unescaped ("show map"
// lists "<id> <key> <value>"), so one with a space couldn't be read back
// unambiguously.
func cliToken(what, s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%w: %s is empty", ErrInvalidArgument, what)
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("%w: %s %q contains %q, which a runtime API argument can't hold", ErrInvalidArgument, what, s, r)
		}
	}
	return cliEscape(s), nil
}

// cliText validates and escapes the free-text last argument of a command
// (a map value, an ACL pattern): spaces are allowed, but no control
// character. Empty is left for HAProxy itself to judge.
func cliText(what, s string) (string, error) {
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: %s %q contains control character %q", ErrInvalidArgument, what, s, r)
		}
	}
	return cliEscape(s), nil
}

// cliPayload turns a PEM bundle into the body of a "<command> <<"
// multi-line payload. HAProxy reads the payload up to the first empty
// line, so an empty line inside data would end it early and run whatever
// follows as commands. Blank lines mean nothing in PEM (a bundle made
// with `cat cert.pem key.pem` often has one between the blocks, which
// used to silently cut the upload short), so they are dropped rather
// than refused - whatever text follows then stays inside the payload.
// Returned with exactly one trailing newline: statsCommand's own "\n"
// then makes the terminating empty line (a bundle without a trailing
// newline used to leave HAProxy waiting until the socket deadline).
func cliPayload(what string, data []byte) (string, error) {
	var lines []string
	for i, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		for _, r := range line {
			if unicode.IsControl(r) && r != '\t' {
				return "", fmt.Errorf("%w: %s contains control character %q (line %d)", ErrInvalidArgument, what, r, i+1)
			}
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("%w: %s is empty", ErrInvalidArgument, what)
	}
	return strings.Join(lines, "\n") + "\n", nil
}
