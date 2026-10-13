// Package haproxystat reads HAProxy's "show stat" CSV by column name:
// HAProxy versions add and move columns, so a position means nothing.
// Shared by the Controller (the node page's HAProxy status) and
// janusctl's dashboard.
package haproxystat

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"
)

// The svname of a proxy's own rows.
const (
	Frontend = "FRONTEND"
	Backend  = "BACKEND"
)

// The kinds of rows "show stat" has - its type column: 0, 1, 2, 3. A
// listener is a frontend's bind, with its own row under `option
// socket-stats`: its svname is the bind's name, its addr the bind's -
// never a server.
const (
	KindFrontend = "frontend"
	KindBackend  = "backend"
	KindServer   = "server"
	KindListener = "listener"
)

// KindOf is a row's kind from its type column, typ; from its svname when
// typ is empty (a show stat without that column), where a listener can't
// be told from a server. "" for a type it doesn't know.
func KindOf(typ, svname string) string {
	switch typ {
	case "0":
		return KindFrontend
	case "1":
		return KindBackend
	case "2":
		return KindServer
	case "3":
		return KindListener
	case "":
		switch svname {
		case Frontend:
			return KindFrontend
		case Backend:
			return KindBackend
		case "":
			return ""
		}
		return KindServer
	}
	return ""
}

// Table is "show stat" as its header plus rows: the JSON the Controller
// serves (columns, rows), and column lookups by name.
type Table struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
	index   map[string]int
}

// Parse reads the CSV "show stat" writes: a header line starting with
// "# ", a trailing comma on every line. Empty input is an empty table.
func Parse(raw []byte) (*Table, error) {
	raw = bytes.TrimPrefix(bytes.TrimSpace(raw), []byte("# "))
	rd := csv.NewReader(bytes.NewReader(raw))
	rd.FieldsPerRecord = -1
	records, err := rd.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse show stat: %w", err)
	}
	t := &Table{Columns: []string{}, Rows: [][]string{}}
	if len(records) > 0 {
		t.Columns, t.Rows = records[0], append([][]string{}, records[1:]...)
	}
	t.index = make(map[string]int, len(t.Columns))
	for i, name := range t.Columns {
		if _, dup := t.index[name]; !dup { // "-" appears more than once
			t.index[name] = i
		}
	}
	return t, nil
}

// Get is row's value in the column named, "" when the column or the
// value is missing.
func (t *Table) Get(row []string, column string) string {
	i, ok := t.index[column]
	if !ok || i >= len(row) {
		return ""
	}
	return row[i]
}

// Uint is Get as a number; false for "" (HAProxy's "not applicable") or
// anything that isn't one.
func (t *Table) Uint(row []string, column string) (uint64, bool) {
	s := t.Get(row, column)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Kind is row's kind (KindOf).
func (t *Table) Kind(row []string) string {
	return KindOf(t.Get(row, "type"), t.Get(row, "svname"))
}

// Has reports whether the header names the column.
func (t *Table) Has(column string) bool {
	_, ok := t.index[column]
	return ok
}
