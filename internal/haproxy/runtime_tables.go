package haproxy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Stick tables through the runtime API: "show table" lists them, "show
// table <name> [key <k> | data.<type> <op> <value>...]" dumps entries,
// "clear table" removes them. Answers captured from this project's
// HAProxy 3.0, 3.2 and 3.4 builds (testdata/show-table*.txt): a header
// "# table: <name>, type: <type>, size:<n>, used:<n>", then one line per
// entry, "<ptr>: key=<k> use=<n> exp=<ms> shard=<n> <data>=<value>...",
// a string key's spaces escaped with a backslash.

// StickTable is one of the running configuration's stick tables.
type StickTable struct {
	Name string `json:"name"`
	Type string `json:"type"` // the key's: ip, ipv6, integer, string, binary
	Size uint64 `json:"size"`
	Used uint64 `json:"used"`
}

// TableFilter keeps the entries whose stored Data compares to Value with
// Op (eq, ne, lt, le, gt, ge).
type TableFilter struct {
	Data  string `json:"data"` // "http_req_rate", "gpc0", "conn_cur"...
	Op    string `json:"op"`
	Value int64  `json:"value"`
}

// TableEntry is one entry of a stick table.
type TableEntry struct {
	Key       string `json:"key"`
	Use       uint64 `json:"use"`        // streams tracking it now
	ExpiresMS int64  `json:"expires_ms"` // until it expires
	// Data are what the table stores, by HAProxy's names - a rate with
	// its period in milliseconds: "http_req_rate(10000)".
	Data []TableData `json:"data"`
}

// TableData is one stored value of an entry.
type TableData struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MaxTableFilters is how many filters HAProxy takes at once.
const MaxTableFilters = 4

var (
	tableHeaderRE = regexp.MustCompile(`^# table: (\S+), type: (\S+), size:(\d+), used:(\d+)`)
	dataTypeRE    = regexp.MustCompile(`^[a-z][a-z0-9_]*(\([0-9]{1,3}\))?$`)
	tableOps      = map[string]bool{"eq": true, "ne": true, "lt": true, "le": true, "gt": true, "ge": true}
)

// TableList runs "show table": the running configuration's stick tables.
func (m *Manager) TableList() ([]StickTable, error) {
	out, err := m.statsCommand("show table")
	if err != nil {
		return nil, err
	}
	var tables []StickTable
	for _, line := range splitNonEmptyLines(string(out)) {
		t, ok := parseTableHeader(line)
		if !ok {
			return nil, fmt.Errorf("show table: %s", strings.TrimSpace(string(out)))
		}
		tables = append(tables, t)
	}
	return tables, nil
}

// TableShow runs "show table <table>": the entry whose key is key, or
// those whose data match every filter, limit at most (truncated: more
// matched).
func (m *Manager) TableShow(table, key string, filters []TableFilter, limit int) (StickTable, []TableEntry, bool, error) {
	cmd, err := tableCommand("show table", table, key, filters, false)
	if err != nil {
		return StickTable{}, nil, false, err
	}
	var (
		head    StickTable
		entries []TableEntry
		answer  []string // what isn't a table's dump: HAProxy's refusal
		more    bool
	)
	truncated, err := m.statsStream(cmd, 64<<20, func(line string) bool {
		if t, ok := parseTableHeader(line); ok {
			head = t
			return true
		}
		if e, ok := parseTableEntry(line); ok {
			if len(entries) == limit {
				more = true
				return false
			}
			entries = append(entries, e)
			return true
		}
		if strings.TrimSpace(line) != "" {
			answer = append(answer, strings.TrimSpace(line))
		}
		return true
	})
	if err != nil {
		return StickTable{}, nil, false, err
	}
	if head.Name == "" {
		return StickTable{}, nil, false, tableRefusal(strings.Join(answer, " "))
	}
	return head, entries, more || truncated, nil
}

// TableClear runs "clear table <table>": removes the entry whose key is
// key, or every entry whose data match the filters - every entry only
// when all says so.
func (m *Manager) TableClear(table, key string, filters []TableFilter, all bool) error {
	if key == "" && len(filters) == 0 && !all {
		return fmt.Errorf("%w: name a key or filters to clear, or clear every entry", ErrInvalidArgument)
	}
	cmd, err := tableCommand("clear table", table, key, filters, true)
	if err != nil {
		return err
	}
	if err := mustEmpty(m.statsCommand(cmd)); err != nil {
		return tableRefusal(err.Error())
	}
	return nil
}

// tableCommand is "<verb> <table> [key <k> | data.<type> <op> <v>...]",
// every argument checked.
func tableCommand(verb, table, key string, filters []TableFilter, clear bool) (string, error) {
	t, err := cliToken("table", table)
	if err != nil {
		return "", err
	}
	cmd := verb + " " + t
	if key != "" {
		if len(filters) > 0 {
			return "", fmt.Errorf("%w: a key or filters, not both", ErrInvalidArgument)
		}
		k, err := cliText("key", key)
		if err != nil {
			return "", err
		}
		return cmd + " key " + k, nil
	}
	if len(filters) > MaxTableFilters {
		return "", fmt.Errorf("%w: %d filters, HAProxy takes %d at most", ErrInvalidArgument, len(filters), MaxTableFilters)
	}
	for _, f := range filters {
		if !dataTypeRE.MatchString(f.Data) {
			return "", fmt.Errorf("%w: %q isn't a stick table data type", ErrInvalidArgument, f.Data)
		}
		if !tableOps[f.Op] {
			return "", fmt.Errorf("%w: operator %q: eq, ne, lt, le, gt or ge", ErrInvalidArgument, f.Op)
		}
		cmd += fmt.Sprintf(" data.%s %s %d", f.Data, f.Op, f.Value)
	}
	return cmd, nil
}

// tableRefusal is HAProxy's answer to a table command it refused, as an
// error: ErrNotFound for a table it doesn't have.
func tableRefusal(answer string) error {
	switch {
	case answer == "":
		return fmt.Errorf("HAProxy gave no answer")
	case strings.HasPrefix(answer, "No such table"):
		return fmt.Errorf("%w: %s", ErrNotFound, answer)
	case strings.HasPrefix(answer, "Filter entry"), strings.HasPrefix(answer, "Invalid"), strings.HasPrefix(answer, "Unknown"):
		return fmt.Errorf("%w: %s", ErrInvalidArgument, answer)
	}
	return fmt.Errorf("%s", answer)
}

func parseTableHeader(line string) (StickTable, bool) {
	m := tableHeaderRE.FindStringSubmatch(line)
	if m == nil {
		return StickTable{}, false
	}
	size, _ := strconv.ParseUint(m[3], 10, 64)
	used, _ := strconv.ParseUint(m[4], 10, 64)
	return StickTable{Name: m[1], Type: m[2], Size: size, Used: used}, true
}

// parseTableEntry reads "<ptr>: key=<k> use=<n> exp=<ms> shard=<n>
// <data>=<value>...".
func parseTableEntry(line string) (TableEntry, bool) {
	ptr, rest, ok := strings.Cut(line, ": ")
	if !ok || !strings.HasPrefix(ptr, "0x") {
		return TableEntry{}, false
	}
	var e TableEntry
	seenKey := false
	for _, field := range splitEscaped(rest) {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch name {
		case "key":
			e.Key, seenKey = unescapeCLI(value), true
		case "use":
			e.Use, _ = strconv.ParseUint(value, 10, 64)
		case "exp":
			e.ExpiresMS, _ = strconv.ParseInt(value, 10, 64)
		case "shard":
		default:
			e.Data = append(e.Data, TableData{Name: name, Value: value})
		}
	}
	return e, seenKey
}

// splitEscaped splits s on the spaces a backslash doesn't escape,
// keeping the escapes.
func splitEscaped(s string) []string {
	var fields []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case ' ':
			if i > start {
				fields = append(fields, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		fields = append(fields, s[start:])
	}
	return fields
}

// unescapeCLI undoes the backslashes HAProxy prints a key with: "\ "
// is a space, "\xHH" a byte.
func unescapeCLI(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		if s[i] == 'x' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
