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

// The svname of a proxy's own rows; every other svname is a server.
const (
	Frontend = "FRONTEND"
	Backend  = "BACKEND"
)

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

// Has reports whether the header names the column.
func (t *Table) Has(column string) bool {
	_, ok := t.index[column]
	return ok
}
