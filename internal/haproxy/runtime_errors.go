package haproxy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// "show errors [<proxy>] [request|response]": the last invalid request
// and response HAProxy captured for each proxy - what made it answer 400
// or 502 "protocol error" - with the bytes it read (testdata/
// show-errors.txt, the same on this project's HAProxy 3.0, 3.2 and 3.4
// builds):
//
//	Total events captured on [<date>] : <n>
//	[<date>] frontend web (#2): invalid request
//	  backend <NONE> (#-1), server <NONE> (#-1), event #0, src 127.0.0.1:34094
//	  buffer starts at 0 (including 0 out), 16268 free,
//	  len 116, wraps at 16344, error at position 102
//	  H1 ... (the parser's state)
//	  00000  GET /x HTTP/1.1\r\n
//	  00099  Bad Header: x\r\n
//
// A buffer line is its offset, then " " - or "+" when it goes on the
// line above, cut for length - and the bytes, escaped ("\r", "\xHH").
// They are what a client or a server sent: credentials too, whose
// values are masked here.

// CapturedError is one invalid request or response HAProxy kept.
type CapturedError struct {
	When       string `json:"when"` // as HAProxy writes it: "18/Oct/2026:07:38:35.666"
	WhenUnixMS int64  `json:"when_unix_ms"`
	ProxyKind  string `json:"proxy_kind"` // frontend, backend
	Proxy      string `json:"proxy"`
	Direction  string `json:"direction"` // request, response
	Frontend   string `json:"frontend,omitempty"`
	Backend    string `json:"backend,omitempty"`
	Server     string `json:"server,omitempty"`
	Source     string `json:"source,omitempty"` // the client's address
	Event      uint64 `json:"event"`
	Position   int    `json:"position"` // of the byte HAProxy refused, in the buffer
	// Details are HAProxy's lines about its buffer and parser state.
	Details []string    `json:"details"`
	Lines   []ErrorLine `json:"lines"`
}

// ErrorLine is one line of a captured buffer.
type ErrorLine struct {
	Offset    int    `json:"offset"`
	Continued bool   `json:"continued,omitempty"` // goes on the line above
	Text      string `json:"text"`                // HAProxy's escaping kept
	// ErrorAt is where in Text the refused byte is, -1 when elsewhere.
	ErrorAt int `json:"error_at"`
}

var (
	errorsTotalRE  = regexp.MustCompile(`^Total events captured on \[[^\]]*\] : (\d+)`)
	errorHeadRE    = regexp.MustCompile(`^\[([^\]]+)\] (frontend|backend) (.+) \(#-?\d+\): invalid (request|response)`)
	errorPeersRE   = regexp.MustCompile(`^\s+(frontend|backend) (.+) \(#-?\d+\), server (.+) \(#-?\d+\), event #(\d+), src (\S+)`)
	errorPosRE     = regexp.MustCompile(`error at position (\d+)`)
	errorLineRE    = regexp.MustCompile(`^  (\d{5,})([ +]) (.*)$`)
	credentialLine = regexp.MustCompile(`(?i)^(authorization|proxy-authorization|cookie|set-cookie|x-api-key|x-auth-token)\s*:`)
)

// ErrorsShow runs "show errors": total is how many HAProxy captured since
// it started; errors are the last of each proxy and direction it keeps -
// only proxy's when set, only direction's ("request", "response") when
// set.
func (m *Manager) ErrorsShow(proxy, direction string) (total uint64, errors []CapturedError, err error) {
	cmd := "show errors"
	if proxy != "" {
		p, err := cliToken("proxy", proxy)
		if err != nil {
			return 0, nil, err
		}
		cmd += " " + p
	} else if direction != "" {
		cmd += " -1" // every proxy, then the direction
	}
	switch direction {
	case "":
	case "request", "response":
		cmd += " " + direction
	default:
		return 0, nil, fmt.Errorf("%w: direction %q: request or response", ErrInvalidArgument, direction)
	}
	out, err := m.statsCommand(cmd)
	if err != nil {
		return 0, nil, err
	}
	return parseErrors(string(out))
}

func parseErrors(out string) (uint64, []CapturedError, error) {
	var (
		total  uint64
		errors []CapturedError
		cur    *CapturedError
		header bool
	)
	for _, line := range strings.Split(out, "\n") {
		if m := errorsTotalRE.FindStringSubmatch(line); m != nil {
			total, _ = strconv.ParseUint(m[1], 10, 64)
			header = true
			continue
		}
		if m := errorHeadRE.FindStringSubmatch(line); m != nil {
			errors = append(errors, CapturedError{When: m[1], WhenUnixMS: haproxyTime(m[1]), ProxyKind: m[2], Proxy: m[3], Direction: m[4], Position: -1})
			cur = &errors[len(errors)-1]
			continue
		}
		if cur == nil {
			continue
		}
		if m := errorLineRE.FindStringSubmatch(line); m != nil {
			offset, _ := strconv.Atoi(m[1])
			cur.Lines = append(cur.Lines, ErrorLine{Offset: offset, Continued: m[2] == "+", Text: m[3], ErrorAt: -1})
			continue
		}
		if m := errorPeersRE.FindStringSubmatch(line); m != nil {
			if m[1] == "frontend" {
				cur.Frontend = m[2]
			} else {
				cur.Backend = none(m[2])
			}
			cur.Server, cur.Event, cur.Source = none(m[3]), atou(m[4]), m[5]
			if cur.ProxyKind == "frontend" {
				cur.Frontend = cur.Proxy
			} else {
				cur.Backend = cur.Proxy
			}
			continue
		}
		if t := strings.TrimSpace(line); t != "" && len(cur.Lines) == 0 {
			if m := errorPosRE.FindStringSubmatch(t); m != nil {
				cur.Position, _ = strconv.Atoi(m[1])
			}
			cur.Details = append(cur.Details, t)
		}
	}
	if !header {
		if answer := strings.TrimSpace(out); strings.HasPrefix(answer, "No such") {
			return 0, nil, fmt.Errorf("%w: %s", ErrNotFound, answer)
		}
		return 0, nil, fmt.Errorf("show errors: %s", strings.TrimSpace(out))
	}
	for i := range errors {
		locateError(&errors[i])
		maskCredentials(errors[i].Lines)
	}
	return total, errors, nil
}

// locateError finds the refused byte - its offset in the buffer - in the
// escaped text of the line holding it.
func locateError(e *CapturedError) {
	for i := range e.Lines {
		l := &e.Lines[i]
		if e.Position < l.Offset || i+1 < len(e.Lines) && e.Position >= e.Lines[i+1].Offset {
			continue
		}
		at, raw := 0, l.Offset
		for at < len(l.Text) && raw < e.Position {
			at += escapedWidth(l.Text[at:])
			raw++
		}
		if at < len(l.Text) {
			l.ErrorAt = at
		}
		return
	}
}

// escapedWidth is how many characters of HAProxy's escaping one byte
// takes at the start of s: "\xHH" four, "\r" two, a printable one.
func escapedWidth(s string) int {
	if s[0] != '\\' || len(s) == 1 {
		return 1
	}
	if s[1] == 'x' && len(s) >= 4 {
		return 4
	}
	return 2
}

// maskCredentials replaces the value of a credential header - on its
// line and on the lines continuing it - by Redacted, keeping the
// escaped line end.
func maskCredentials(lines []ErrorLine) {
	masking := false
	for i := range lines {
		l := &lines[i]
		start := 0
		if !l.Continued {
			m := credentialLine.FindStringIndex(l.Text)
			masking = m != nil
			if !masking {
				continue
			}
			start = m[1]
		} else if !masking {
			continue
		}
		end := len(l.Text)
		if strings.HasSuffix(l.Text, `\r\n`) {
			end -= 4
		}
		if end <= start {
			continue
		}
		value := l.Text[start:end]
		lead := len(value) - len(strings.TrimLeft(value, " "))
		l.Text = l.Text[:start+lead] + Redacted + l.Text[end:]
		if l.ErrorAt > start+lead {
			l.ErrorAt = start + lead
		}
	}
}

// haproxyTime reads HAProxy's "18/Oct/2026:07:38:35.666" - local time,
// UTC on a node - as Unix milliseconds; 0 for what it can't read.
func haproxyTime(s string) int64 {
	t, err := time.ParseInLocation("02/Jan/2006:15:04:05.000", s, time.Local)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func atou(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}
