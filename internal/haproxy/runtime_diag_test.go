package haproxy

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// testdata/show-table*.txt: this project's HAProxy 3.4 build, an IP table
// tracking the client - one request of it still running - and a string
// table keyed by a request header (one key with a space, which HAProxy
// escapes).
func TestParseStickTables(t *testing.T) {
	var tables []StickTable
	for _, line := range splitNonEmptyLines(fixture(t, "show-table.txt")) {
		tbl, ok := parseTableHeader(line)
		if !ok {
			t.Fatalf("header %q not read", line)
		}
		tables = append(tables, tbl)
	}
	want := []StickTable{{"st_users", "string", 100, 2}, {"web", "ip", 1024, 1}}
	if !reflect.DeepEqual(tables, want) {
		t.Errorf("tables = %+v, want %+v", tables, want)
	}

	lines := splitNonEmptyLines(fixture(t, "show-table-web.txt"))
	e, ok := parseTableEntry(lines[1])
	if !ok || e.Key != "127.0.0.1" || e.Use != 1 || e.ExpiresMS < 500000 {
		t.Fatalf("web entry = %+v", e)
	}
	var names []string
	for _, d := range e.Data {
		names = append(names, d.Name)
	}
	if strings.Join(names, " ") != "gpc0 conn_cur http_req_rate(10000) http_err_cnt bytes_out_rate(60000)" {
		t.Errorf("data = %v", e.Data)
	}
	if e.Data[2].Value != "8" {
		t.Errorf("http_req_rate = %q", e.Data[2].Value)
	}

	var keys []string
	for _, line := range splitNonEmptyLines(fixture(t, "show-table-users.txt"))[1:] {
		e, ok := parseTableEntry(line)
		if !ok {
			t.Fatalf("entry %q not read", line)
		}
		keys = append(keys, e.Key)
	}
	if !reflect.DeepEqual(keys, []string{"alice", "bob smith"}) {
		t.Errorf("keys = %q", keys)
	}
}

func TestUnescapeCLI(t *testing.T) {
	for in, want := range map[string]string{`a\ b`: "a b", `a\\b`: `a\b`, `\x41\x00`: "A\x00", `end\`: `end\`, `\xZZ`: "xZZ"} {
		if got := unescapeCLI(in); got != want {
			t.Errorf("unescapeCLI(%q) = %q, want %q", in, got, want)
		}
	}
}

// testdata/show-sess.txt: the runtime API's own stream, then a request
// held by a server that never answers.
func TestParseSessions(t *testing.T) {
	lines := splitNonEmptyLines(fixture(t, "show-sess.txt"))
	if s, ok := parseSession(lines[0]); !ok || s.Frontend != "GLOBAL" || s.Backend != "" || s.Proto != "unix_stream" {
		t.Errorf("the CLI's own stream = %+v", s)
	}
	s, ok := parseSession(lines[1])
	if !ok || s.Frontend != "web" || s.Backend != "be_hold" || s.Server != "h1" || s.Proto != "tcpv4" ||
		!strings.HasPrefix(s.Source, "127.0.0.1:") || s.Age != "1s" || s.AgeSeconds != 1 || s.Calls != 2 || s.Expires == "" || s.Raw != lines[1] {
		t.Errorf("held stream = %+v", s)
	}
	for in, want := range map[string]uint64{"0s": 0, "45s": 45, "1m30s": 90, "2h3m": 7380, "4d5h": 363600, "": 0, "<NEVER>": 0, "5ms": 0} {
		if got := humanSeconds(in); got != want {
			t.Errorf("humanSeconds(%q) = %d, want %d", in, got, want)
		}
	}
}

// testdata/show-errors.txt: an invalid request (a header name with a
// space) carrying Authorization and Cookie, an invalid response (a
// control character in a header name) carrying Set-Cookie.
func TestParseErrors(t *testing.T) {
	total, errs, err := parseErrors(fixture(t, "show-errors.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(errs) != 2 {
		t.Fatalf("total %d, %d errors", total, len(errs))
	}
	req, resp := errs[0], errs[1]
	if req.ProxyKind != "frontend" || req.Proxy != "web" || req.Direction != "request" || req.Frontend != "web" || req.Backend != "" || req.Server != "" ||
		!strings.HasPrefix(req.Source, "127.0.0.1:") || req.Event != 0 || req.Position != 102 || req.WhenUnixMS == 0 {
		t.Errorf("request error = %+v", req)
	}
	if resp.ProxyKind != "backend" || resp.Proxy != "be_slow" || resp.Direction != "response" || resp.Frontend != "web" || resp.Backend != "be_slow" || resp.Server != "s2" || resp.Event != 1 || resp.Position != 39 {
		t.Errorf("response error = %+v", resp)
	}
	if len(req.Details) == 0 || !strings.Contains(strings.Join(req.Details, "\n"), "error at position 102") {
		t.Errorf("details = %q", req.Details)
	}

	text := func(e CapturedError) string {
		var b strings.Builder
		for _, l := range e.Lines {
			b.WriteString(l.Text + "\n")
		}
		return b.String()
	}
	for _, secret := range []string{"c2VjcmV0OnBhc3M=", "sid=abc123", "token=zzz"} {
		if strings.Contains(text(req)+text(resp), secret) {
			t.Errorf("%q is shown", secret)
		}
	}
	for _, want := range []string{`Authorization: <redacted>\r\n`, `Cookie: <redacted>\r\n`, `Host: www.example.com\r\n`} {
		if !strings.Contains(text(req), want) {
			t.Errorf("request lacks %q:\n%s", want, text(req))
		}
	}
	if !strings.Contains(text(resp), `Set-Cookie: <redacted>\r\n`) {
		t.Errorf("response:\n%s", text(resp))
	}

	// The refused bytes: the space in "Bad Header", the \x01 in
	// "Bad\x01Header".
	for _, c := range []struct {
		e    CapturedError
		line string
		at   int
	}{{req, "Bad Header: x", 3}, {resp, `Bad\x01Header: y`, 3}} {
		found := false
		for _, l := range c.e.Lines {
			if strings.HasPrefix(l.Text, c.line) {
				found = l.ErrorAt == c.at
			} else if l.ErrorAt >= 0 {
				t.Errorf("%q marked at %d", l.Text, l.ErrorAt)
			}
		}
		if !found {
			t.Errorf("%q isn't marked at %d", c.line, c.at)
		}
	}

	if _, _, err := parseErrors("No such proxy.\n"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown proxy: %v", err)
	}
}

// A credential header cut over several lines is masked on each.
func TestMaskCredentialsContinued(t *testing.T) {
	lines := []ErrorLine{
		{Offset: 0, Text: `Authorization: Bearer aaaa`, ErrorAt: -1},
		{Offset: 26, Continued: true, Text: `bbbb\r\n`, ErrorAt: -1},
		{Offset: 32, Text: `Accept: */*\r\n`, ErrorAt: -1},
	}
	maskCredentials(lines)
	got := []string{lines[0].Text, lines[1].Text, lines[2].Text}
	want := []string{`Authorization: <redacted>`, `<redacted>\r\n`, `Accept: */*\r\n`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("masked = %q, want %q", got, want)
	}
}

func TestParsePatternMatch(t *testing.T) {
	for file, want := range map[string]PatternMatch{
		"get-map-hit.txt":    {Found: true, Method: "dom", Pattern: "www.example.com", Value: "be_default"},
		"get-map-dom.txt":    {Found: true, Method: "dom", Pattern: ".example.org", Value: "be_slow"},
		"get-map-miss.txt":   {Method: "dom"},
		"get-acl-hit.txt":    {Found: true, Method: "ip", Pattern: "10.1.2.0/24"},
		"get-acl-miss.txt":   {Method: "ip"},
		"get-acl-prefix.txt": {Found: true, Method: "beg", Pattern: "/api"},
	} {
		got, err := parsePatternMatch(strings.TrimSpace(fixture(t, file)))
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", file, got, err, want)
		}
	}
	if got, _ := parsePatternMatch(`type=str, case=insensitive, found=yes, idx=tree, key="a, b", value="x y", type="str"`); got.Pattern != "a, b" || got.Value != "x y" || !got.CaseInsensitive {
		t.Errorf("quoted comma: %+v", got)
	}
	if _, err := parsePatternMatch("Unknown map identifier. Please use #<id> or <file>."); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown map: %v", err)
	}
}

// TestRuntimeDiagCommands: what each call sends, values escaped.
func TestRuntimeDiagCommands(t *testing.T) {
	m, f := startFakeStatsSocket(t)
	_, _, _, _ = m.TableShow("web", "", []TableFilter{{"http_req_rate", "gt", 10}, {"gpc(0)", "eq", -1}}, 10)
	_, _, _, _ = m.TableShow("st_users", "bob smith", nil, 10)
	_ = m.TableClear("web", "10.0.0.1", nil, false)
	_ = m.TableClear("web", "", []TableFilter{{"gpc0", "ge", 1}}, false)
	_ = m.TableClear("web", "", nil, true)
	_ = m.SessionShutdown("0x7702b96d66a0")
	_, _, _ = m.ErrorsShow("web", "request")
	_, _, _ = m.ErrorsShow("", "response")
	_, _, _ = m.ErrorsShow("", "")
	_, _ = m.MapLookup("/etc/haproxy/maps/hosts.map", "www.example.com")
	_, _ = m.ACLLookup("#0", "/api x;clear acl #0")
	want := []string{
		"show table web data.http_req_rate gt 10 data.gpc(0) eq -1\n",
		`show table st_users key bob\ smith` + "\n",
		"clear table web key 10.0.0.1\n",
		"clear table web data.gpc0 ge 1\n",
		"clear table web\n",
		"shutdown session 0x7702b96d66a0\n",
		"show errors web request\n",
		"show errors -1 response\n",
		"show errors\n",
		"get map /etc/haproxy/maps/hosts.map www.example.com\n",
		`get acl #0 /api\ x\;clear\ acl\ #0` + "\n",
	}
	if got := f.received(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("commands sent:\n%q\nwant:\n%q", got, want)
	}
}

// TestRuntimeDiagRefused: nothing a caller sends can add a command or
// clear a whole table by mistake - refused before the socket.
func TestRuntimeDiagRefused(t *testing.T) {
	m, f := startFakeStatsSocket(t)
	five := []TableFilter{{"a", "eq", 1}, {"b", "eq", 1}, {"c", "eq", 1}, {"d", "eq", 1}, {"e", "eq", 1}}
	cases := map[string]func() error{
		"table newline": func() error { _, _, _, err := m.TableShow("web\nclear table web", "", nil, 1); return err },
		"table space":   func() error { _, _, _, err := m.TableShow("web x", "", nil, 1); return err },
		"key newline":   func() error { _, _, _, err := m.TableShow("web", "k\nclear table web", nil, 1); return err },
		"data type": func() error {
			_, _, _, err := m.TableShow("web", "", []TableFilter{{"gpc0;clear", "eq", 1}}, 1)
			return err
		},
		"operator": func() error {
			_, _, _, err := m.TableShow("web", "", []TableFilter{{"gpc0", "eq\n", 1}}, 1)
			return err
		},
		"five filters":       func() error { _, _, _, err := m.TableShow("web", "", five, 1); return err },
		"key and filters":    func() error { _, _, _, err := m.TableShow("web", "k", []TableFilter{{"gpc0", "eq", 1}}, 1); return err },
		"clear nothing":      func() error { return m.TableClear("web", "", nil, false) },
		"session id":         func() error { return m.SessionShutdown("0x1\nshutdown sessions server be/s1") },
		"session id word":    func() error { return m.SessionShutdown("all") },
		"proxy space":        func() error { _, _, err := m.ErrorsShow("web x", ""); return err },
		"direction":          func() error { _, _, err := m.ErrorsShow("", "both"); return err },
		"map newline":        func() error { _, err := m.MapLookup("m\nclear map m", "x"); return err },
		"lookup value tab":   func() error { _, err := m.MapLookup("m", "a\tb"); return err },
		"lookup value empty": func() error { _, err := m.ACLLookup("#0", ""); return err },
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := f.received(); len(got) != 0 {
		t.Fatalf("the stats socket received %q - nothing should reach it", got)
	}
}

// TestRuntimeDiagLimits: a table or stream list past the limit stops
// there and says so; the runtime API's own stream isn't listed.
func TestRuntimeDiagLimits(t *testing.T) {
	m, _ := startFakeStatsSocket(t)
	head, entries, truncated, err := m.TableShow("big", "", nil, 3)
	if err != nil || head.Name != "big" || len(entries) != 3 || !truncated || entries[2].Key != "10.0.0.2" {
		t.Errorf("TableShow: %+v %d %v %v", head, len(entries), truncated, err)
	}
	if _, entries, truncated, err := m.TableShow("big", "", nil, 50); err != nil || len(entries) != 10 || truncated {
		t.Errorf("TableShow, all: %d %v %v", len(entries), truncated, err)
	}

	all, truncated, err := m.SessionList(SessionFilter{}, 100)
	if err != nil || len(all) != 3 || truncated || all[0].ID != "0x2" {
		t.Fatalf("SessionList: %+v %v %v", all, truncated, err)
	}
	if s, more, _ := m.SessionList(SessionFilter{}, 2); len(s) != 2 || !more {
		t.Errorf("SessionList, limited: %d %v", len(s), more)
	}
	for f, want := range map[SessionFilter][]string{
		{Frontend: "web"}:                {"0x2", "0x3"},
		{Backend: "app"}:                 {"0x2", "0x3"},
		{Source: "10.0.0.1"}:             {"0x2"}, // not 10.0.0.11
		{Source: "10.0.0.11:5001"}:       {"0x3"},
		{Source: "2001:db8::1"}:          {"0x4"},
		{Frontend: "web", Backend: "db"}: nil,
	} {
		s, _, err := m.SessionList(f, 100)
		var ids []string
		for _, x := range s {
			ids = append(ids, x.ID)
		}
		if err != nil || !reflect.DeepEqual(ids, want) {
			t.Errorf("%+v: %v %v, want %v", f, ids, err, want)
		}
	}
	if all[2].AgeSeconds != 3720 || all[2].Backend != "" {
		t.Errorf("IPv6 stream = %+v", all[2])
	}
}
