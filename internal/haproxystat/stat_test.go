package haproxystat

import (
	"os"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	raw, err := os.ReadFile("../api/testdata/show-stat.csv")
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Columns[0] != "pxname" || tbl.Columns[1] != "svname" || !tbl.Has("check_status") {
		t.Errorf("columns start %v", tbl.Columns[:2])
	}
	if len(tbl.Rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(tbl.Rows))
	}
	fe, web1, backend := tbl.Rows[0], tbl.Rows[1], tbl.Rows[3]
	if tbl.Get(fe, "svname") != Frontend || tbl.Get(backend, "svname") != Backend || tbl.Get(web1, "pxname") != "web" {
		t.Errorf("pxname/svname: %q %q / %q %q / %q", tbl.Get(fe, "pxname"), tbl.Get(fe, "svname"), tbl.Get(backend, "pxname"), tbl.Get(backend, "svname"), tbl.Get(web1, "pxname"))
	}
	if tbl.Get(web1, "status") != "DOWN" {
		t.Errorf("web1 status = %q", tbl.Get(web1, "status"))
	}
	// A frontend has no queue: "" is "not applicable", not 0.
	if v, ok := tbl.Uint(fe, "qcur"); ok {
		t.Errorf("frontend qcur = %d, want none", v)
	}
	if v, ok := tbl.Uint(web1, "qcur"); !ok || v != 0 {
		t.Errorf("web1 qcur = %d %v, want 0 true", v, ok)
	}
	if v, ok := tbl.Uint(web1, "weight"); !ok || v == 0 {
		t.Errorf("web1 weight = %d %v", v, ok)
	}
	if tbl.Get(web1, "no_such_column") != "" || tbl.Has("no_such_column") {
		t.Error("a missing column must read as empty")
	}
	if _, ok := tbl.Uint(web1, "status"); ok {
		t.Error("a word isn't a number")
	}
}

func TestParseEmptyAndBroken(t *testing.T) {
	empty, err := Parse(nil)
	if err != nil || len(empty.Rows) != 0 || len(empty.Columns) != 0 || empty.Get(nil, "pxname") != "" {
		t.Errorf("empty = %+v, %v", empty, err)
	}
	if _, err := Parse([]byte("# a,b\n\"unterminated\n")); err == nil {
		t.Error("a broken CSV must fail")
	}
}

// Rows by kind - the type column -, as issue #64's configuration has
// them (../api/testdata/show-stat-socket-stats.csv, HAProxy 3.4.6):
// option socket-stats gives the HTTP frontend's two binds rows of their
// own, listeners, never servers.
func TestKind(t *testing.T) {
	raw, err := os.ReadFile("../api/testdata/show-stat-socket-stats.csv")
	if err != nil {
		t.Fatal(err)
	}
	tab, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range tab.Rows {
		got = append(got, tab.Get(r, "pxname")+"/"+tab.Get(r, "svname")+"="+tab.Kind(r))
	}
	want := []string{
		"fe_https/FRONTEND=frontend", "fe_https/https=listener", "fe_https/https-alt=listener",
		"be_web/web1=server", "be_web/web2=server", "be_web/BACKEND=backend",
		"fe_relay/FRONTEND=frontend", "be_relay/relay1=server", "be_relay/BACKEND=backend",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("kinds:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Without the type column, by svname; a type it doesn't know: none.
	for _, c := range []struct{ typ, sv, want string }{
		{"", "FRONTEND", KindFrontend}, {"", "BACKEND", KindBackend}, {"", "web1", KindServer}, {"", "", ""}, {"4", "x", ""},
	} {
		if got := KindOf(c.typ, c.sv); got != c.want {
			t.Errorf("KindOf(%q, %q) = %q, want %q", c.typ, c.sv, got, c.want)
		}
	}
}
