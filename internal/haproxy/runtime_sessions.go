package haproxy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Streams through the runtime API: "show sess" lists those HAProxy is
// processing, one line each (testdata/show-sess.txt, from this
// project's HAProxy 3.0, 3.2 and 3.4 builds: the same fields), "shutdown
// session <ptr>" closes one - an empty answer when it did. A connection
// shows up once its request is whole: an idle keep-alive connection or
// a request still arriving has no stream yet.

// Session is a stream HAProxy is processing.
type Session struct {
	ID         string `json:"id"`    // what SessionShutdown takes: "0x7702b96d66a0"
	Proto      string `json:"proto"` // tcpv4, tcpv6, unix_stream...
	Source     string `json:"source"`
	Frontend   string `json:"frontend"`
	Backend    string `json:"backend,omitempty"`
	Server     string `json:"server,omitempty"`
	Age        string `json:"age"` // as HAProxy writes it: "1m30s"
	AgeSeconds uint64 `json:"age_seconds"`
	Calls      uint64 `json:"calls"`
	Expires    string `json:"expires,omitempty"` // the next timeout: "29s"
	Raw        string `json:"raw"`               // the whole line, for its internals
}

// SessionFilter keeps the streams of a frontend, a backend, a client
// address - each when set.
type SessionFilter struct {
	Frontend, Backend, Source string
}

var sessionIDRE = regexp.MustCompile(`^0x[0-9a-fA-F]{1,16}$`)

// SessionList runs "show sess": the streams matching f, limit at most
// (truncated: more matched). The runtime API's own - janusd asking,
// frontend GLOBAL - aren't listed.
func (m *Manager) SessionList(f SessionFilter, limit int) ([]Session, bool, error) {
	var (
		sessions []Session
		answer   []string
		more     bool
	)
	truncated, err := m.statsStream("show sess", 64<<20, func(line string) bool {
		s, ok := parseSession(line)
		if !ok {
			if strings.TrimSpace(line) != "" {
				answer = append(answer, strings.TrimSpace(line))
			}
			return true
		}
		if s.Frontend == "GLOBAL" || !f.matches(s) {
			return true
		}
		if len(sessions) == limit {
			more = true
			return false
		}
		sessions = append(sessions, s)
		return true
	})
	if err != nil {
		return nil, false, err
	}
	if len(answer) > 0 && len(sessions) == 0 {
		return nil, false, fmt.Errorf("show sess: %s", strings.Join(answer, " "))
	}
	return sessions, more || truncated, nil
}

func (f SessionFilter) matches(s Session) bool {
	if f.Frontend != "" && s.Frontend != f.Frontend || f.Backend != "" && s.Backend != f.Backend {
		return false
	}
	// An address alone matches whatever port: "10.0.0.1" matches
	// "10.0.0.1:51234" (and an IPv6 address the same way).
	return f.Source == "" || s.Source == f.Source || strings.HasPrefix(s.Source, f.Source+":")
}

// SessionShutdown runs "shutdown session <id>": the stream ends, its
// client and server connections closed.
func (m *Manager) SessionShutdown(id string) error {
	if !sessionIDRE.MatchString(id) {
		return fmt.Errorf("%w: %q isn't a stream ID (0x and hexadecimal digits, as SessionList gives it)", ErrInvalidArgument, id)
	}
	err := mustEmpty(m.statsCommand("shutdown session " + id))
	if err != nil && strings.HasPrefix(err.Error(), "No such session") {
		return fmt.Errorf("%w: %s", ErrNotFound, err)
	}
	return err
}

// parseSession reads "<ptr>: proto=<p> src=<addr> fe=<name> be=<name>
// srv=<name> ts=.. epoch=.. age=<t> calls=<n> ... exp=<t> ...".
func parseSession(line string) (Session, bool) {
	ptr, rest, ok := strings.Cut(line, ": ")
	if !ok || !sessionIDRE.MatchString(ptr) {
		return Session{}, false
	}
	s := Session{ID: ptr, Raw: line}
	for _, field := range strings.Fields(rest) {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch name {
		case "proto":
			s.Proto = value
		case "src":
			s.Source = value
		case "fe":
			s.Frontend = value
		case "be":
			s.Backend = none(value)
		case "srv":
			s.Server = none(value)
		case "age":
			s.Age, s.AgeSeconds = value, humanSeconds(value)
		case "calls":
			s.Calls, _ = strconv.ParseUint(value, 10, 64)
		case "exp":
			s.Expires = value
		}
	}
	return s, s.Frontend != ""
}

// none is "" for HAProxy's "<NONE>" or "<none>".
func none(v string) string {
	if strings.EqualFold(v, "<none>") {
		return ""
	}
	return v
}

// humanSeconds reads HAProxy's human_time() - "45s", "1m30s", "2h3m",
// "4d5h" - as seconds; 0 for what it can't read.
func humanSeconds(v string) uint64 {
	var total, n uint64
	digits := false
	for _, c := range v {
		switch {
		case c >= '0' && c <= '9':
			n = n*10 + uint64(c-'0')
			digits = true
		case digits && (c == 'd' || c == 'h' || c == 'm' || c == 's'):
			total += n * map[rune]uint64{'d': 86400, 'h': 3600, 'm': 60, 's': 1}[c]
			n, digits = 0, false
		default:
			return 0
		}
	}
	return total
}
