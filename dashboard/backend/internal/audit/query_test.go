package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSubjectOf(t *testing.T) {
	for path, want := range map[string]string{
		"/nodes/3f9c/api/haproxy/config":        "node:3f9c",
		"/nodes/3f9c/api/support":               "node:3f9c",
		"/api/nodes/3f9c":                       "node:3f9c",
		"/api/nodes/3f9c?force=true":            "node:3f9c",
		"/api/nodes":                            ":",
		"/api/nodes/status":                     ":",
		"/api/pending/p1/approve":               "pending:p1",
		"/api/machines/m1":                      "machine:m1",
		"/api/machines/m1/power":                "machine:m1",
		"/api/clusters/c1/members/lb-1/drain":   "cluster:c1",
		"/api/clusters":                         ":",
		"/api/hypervisors/h1/trust":             "hypervisor:h1",
		"/api/hypervisors/preparation":          ":",
		"/api/users/rita/ssh-keys":              "account:rita",
		"/api/tokens/t1":                        "token:t1",
		"/api/enroll-tokens/e1":                 "enrollment-token:e1",
		"/api/sso/providers/kc":                 "sso-provider:kc",
		"/api/sso/test":                         ":",
		"/api/settings":                         ":",
		"/api/auth/login":                       ":",
		"/api/clusters/c1?machines=destroy":     "cluster:c1",
		"/nodes/":                               ":",
		"/api/":                                 ":",
		"/api/fleet/recovery-kit":               ":",
		"/api/controller/tls":                   ":",
		"/api/auth/mfa/passkeys/k1":             ":",
		"/api/users/rita/identities/i1":         "account:rita",
		"/api/clusters/c1/balancer/sync":        "cluster:c1",
		"/api/machines/m1/apply-settings?x=1":   "machine:m1",
		"/nodes/3f9c/api/system/services/x/run": "node:3f9c",
	} {
		kind, id := SubjectOf(path)
		if got := kind + ":" + id; got != want {
			t.Errorf("SubjectOf(%s) = %s, want %s", path, got, want)
		}
	}
}

func TestCategory(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"POST", "/api/auth/login", CatSignIn},
		{"POST", "/api/auth/logout", CatSignIn},
		{"POST", "/api/auth/mfa/totp", CatSignIn},
		{"DELETE", "/api/auth/mfa/totp", CatAccounts},
		{"POST", "/api/auth/mfa/totp/setup", CatAccounts},
		{"POST", "/api/auth/mfa/passkey/finish", CatSignIn},
		{"POST", "/api/auth/mfa/passkeys/finish", CatAccounts},
		{"POST", "/api/auth/sso/kc/start", CatSignIn},
		{"POST", "/api/auth/sso/kc/link", CatAccounts},
		{"POST", "/api/auth/sso/finish", CatSignIn},
		{"POST", "/api/cli/ssh-login", CatSignIn},
		{"POST", "/api/cli/device/ABCD/approve", CatSignIn},
		{"POST", "/api/auth/password", CatAccounts},
		{"PATCH", "/api/users/rita", CatAccounts},
		{"DELETE", "/api/tokens/t1", CatAccounts},
		{"PUT", "/api/me/preferences", CatAccounts},
		{"DELETE", "/api/nodes/n1", CatNodes},
		{"POST", "/api/nodes", CatNodes},
		{"POST", "/api/pending/p1/approve", CatNodes},
		{"POST", "/nodes/n1/api/haproxy/config", CatNodes},
		{"GET", "/nodes/n1/api/support", CatNodes},
		{"POST", "/api/clusters/c1/apply", CatClusters},
		{"PATCH", "/api/machines/m1", CatMachines},
		{"POST", "/api/hypervisors/preparation", CatMachines},
		{"POST", "/api/fleet/setup", CatFleet},
		{"GET", "/api/fleet/recovery-kit", CatFleet},
		{"DELETE", "/api/enroll-tokens/e1", CatFleet},
		{"PUT", "/api/controller/tls", CatFleet},
		{"POST", "/api/controller/update", CatSettings},
		{"PUT", "/api/settings", CatSettings},
		{"PUT", "/api/log-forwarding", CatSettings},
		{"PUT", "/api/sso/providers/kc", CatSettings},
		{"POST", "/api/backups/run", CatSettings},
		{"POST", "/api/nodesx", CatSettings},
	} {
		if got := Category(tc.method, tc.path); got != tc.want {
			t.Errorf("Category(%s %s) = %s, want %s", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestFilterMatch(t *testing.T) {
	at := time.Date(2026, 10, 29, 14, 30, 0, 0, time.UTC)
	e := Entry{Time: at, User: "rita", Via: "token t1", Method: "PATCH", Path: "/api/machines/m1", Status: 409, Client: "10.1.2.3",
		Subject: &Subject{Kind: KindMachine, ID: "m1", Name: "edge-1"}}
	for _, tc := range []struct {
		f    Filter
		want bool
	}{
		{Filter{}, true},
		{Filter{User: "rita"}, true},
		{Filter{User: "rit"}, false},
		{Filter{Via: "token"}, true},
		{Filter{Via: "token t1"}, true},
		{Filter{Via: "token t"}, false},
		{Filter{Via: "session"}, false},
		{Filter{Via: "none"}, false},
		{Filter{Subject: "machine:m1"}, true},
		{Filter{Subject: "node:m1"}, false},
		{Filter{Result: "refused"}, true},
		{Filter{Result: "ok"}, false},
		{Filter{Result: "failed"}, false},
		{Filter{Category: CatMachines}, true},
		{Filter{Category: CatNodes}, false},
		{Filter{Method: "PATCH"}, true},
		{Filter{Method: "POST"}, false},
		{Filter{From: at}, true},
		{Filter{From: at.Add(time.Second)}, false},
		{Filter{To: at}, false},
		{Filter{From: at.Add(-time.Hour), To: at.Add(time.Hour)}, true},
		{Filter{Client: "10.1.2."}, true},
		{Filter{Client: "10.1.3"}, false},
		{Filter{Text: "MACHINES"}, true},
		{Filter{Text: "Edge-1"}, true}, // the subject's name
		{Filter{Text: "edge-2"}, false},
		{Filter{User: "rita", Result: "refused", Category: CatMachines, Text: "m1"}, true},
		{Filter{User: "rita", Result: "failed", Category: CatMachines}, false},
	} {
		if got := tc.f.Match(e); got != tc.want {
			t.Errorf("%+v: %v, want %v", tc.f, got, tc.want)
		}
	}
	if (Filter{Via: "none"}).Match(Entry{}) != true || (Filter{Result: "failed"}).Match(Entry{Status: 502}) != true {
		t.Error("none / failed")
	}
	// An entry written before subjects: its path names it.
	if !(Filter{Subject: "node:n1"}).Match(Entry{Method: "DELETE", Path: "/api/nodes/n1"}) {
		t.Error("a subject from the path")
	}
	if (Filter{Result: "maybe"}).Check() == nil || (Filter{Category: "x"}).Check() == nil || (Filter{Result: "ok", Category: CatSignIn}).Check() != nil {
		t.Error("Check")
	}
}

// walk reads every page of f, limit at a time, and checks each page's
// total; append, when set, runs between pages.
func walk(t *testing.T, l *Log, f Filter, limit int, between func(page int)) []Entry {
	t.Helper()
	var out []Entry
	before := ""
	for page := 0; ; page++ {
		p, err := l.Query(f, before, limit)
		if err != nil {
			t.Fatalf("page %d (%q): %v", page, before, err)
		}
		if len(p.Entries) > limit || p.Next != "" && len(p.Entries) != limit {
			t.Fatalf("page %d: %d entries, next %q", page, len(p.Entries), p.Next)
		}
		out = append(out, p.Entries...)
		if p.Next == "" {
			return out
		}
		before = p.Next
		if page > 10000 {
			t.Fatal("no end")
		}
		if between != nil {
			between(page)
		}
	}
}

// Pages go back through both files, a filter applied, every entry once,
// newest first - while entries are appended and the file rotates under
// the reader.
func TestQueryPages(t *testing.T) {
	dir := t.TempDir()
	old := MaxSize
	MaxSize = 6000
	t.Cleanup(func() { MaxSize = old })
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	add := func(i int) {
		e := Entry{Time: time.Unix(int64(i), 0), User: []string{"rita", "olga", "root"}[i%3], Via: "session", Method: "POST", Path: fmt.Sprintf("/api/nodes/n%d", i%5), Status: []int{200, 403, 500, 204}[i%4]}
		if err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 120 {
		add(i)
	}
	f := Filter{User: "rita", Result: "ok"}
	whole, _ := l.Query(f, "", 1000)
	if whole.Total != len(whole.Entries) || whole.Total == 0 {
		t.Fatalf("whole: total %d, %d entries", whole.Total, len(whole.Entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl.1")); err != nil {
		t.Fatal("never rotated: make MaxSize smaller")
	}
	got := walk(t, l, f, 3, nil)
	if !slices.EqualFunc(got, whole.Entries, func(a, b Entry) bool { return a.ID == b.ID }) {
		t.Fatalf("paged: %v\nwhole: %v", ids(got), ids(whole.Entries))
	}
	for _, e := range got {
		if e.Subject == nil || e.Subject.Kind != KindNode {
			t.Fatalf("no subject from the path: %+v", e)
		}
	}
	// Appending between pages (and rotating) changes nothing for the pages
	// already under way: no entry twice, none missed until rotated away.
	n := 120
	got = walk(t, l, Filter{}, 7, func(int) {
		for range 3 {
			add(n)
			n++
		}
	})
	for i := 1; i < len(got); i++ {
		if got[i].ID >= got[i-1].ID {
			t.Fatalf("not strictly newest first at %d: %d after %d", i, got[i].ID, got[i-1].ID)
		}
	}
	// A cursor: older than its entry.
	first, _ := l.Query(Filter{}, "", 1000)
	oldest := first.Entries[len(first.Entries)-1].ID
	if p, _ := l.Query(Filter{}, fmt.Sprint(oldest+2), 1); len(p.Entries) != 1 || p.Entries[0].ID != oldest+1 {
		t.Fatalf("cursor %d: %+v", oldest+2, p.Entries)
	}
	// Its entry rotated away: nothing older is left.
	if p, _ := l.Query(Filter{}, "1", 5); len(p.Entries) != 0 || p.Next != "" {
		t.Fatalf("cursor rotated away: %+v", p)
	}
	if p, _ := l.Query(Filter{}, "", 0); len(p.Entries) != 1 || p.Next == "" {
		t.Fatalf("a limit below 1 is 1: %+v", p)
	}
	for _, bad := range []string{"x", "0", "0~0", "3~x", "3~-1", "-1", "3+1"} {
		if _, err := l.Query(Filter{}, bad, 5); err == nil {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}

func ids(es []Entry) []uint64 {
	out := make([]uint64, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

// Entries written before IDs existed are reached by position, the pages
// going on across them - with and without a newer entry with an ID.
func TestQueryEntriesWithoutID(t *testing.T) {
	for _, withIDs := range []bool{true, false} {
		dir := t.TempDir()
		var lines []string
		for i := range 11 {
			b, _ := json.Marshal(Entry{Time: time.Unix(int64(i), 0), User: fmt.Sprint("old", i), Method: "POST", Path: "/api/x", Status: 200})
			lines = append(lines, string(b))
		}
		if err := os.WriteFile(filepath.Join(dir, "audit.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		l, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if withIDs {
			for i := range 4 {
				_ = l.Append(Entry{Time: time.Unix(int64(100+i), 0), User: fmt.Sprint("new", i), Method: "POST", Path: "/api/x", Status: 200})
			}
		}
		for _, limit := range []int{1, 2, 3, 4, 5, 20} {
			got := walk(t, l, Filter{}, limit, nil)
			want := 11
			if withIDs {
				want = 15
			}
			if len(got) != want {
				t.Fatalf("ids %v, limit %d: %d entries", withIDs, limit, len(got))
			}
			for i := 1; i < len(got); i++ {
				if !got[i].Time.Before(got[i-1].Time) {
					t.Fatalf("ids %v, limit %d: %v after %v", withIDs, limit, got[i].Time, got[i-1].Time)
				}
			}
		}
		// A filter over them, a page at a time.
		got := walk(t, l, Filter{User: "old3"}, 1, nil)
		if len(got) != 1 || got[0].User != "old3" {
			t.Fatalf("filtered: %+v", got)
		}
	}
}

// An entry written before subjects is named by a newer one about the same
// thing; a subject filter finds both.
func TestQueryNamesOldEntries(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Append(Entry{Method: "PATCH", Path: "/api/nodes/n1", Status: 200})
	_ = l.Append(Entry{Method: "POST", Path: "/nodes/n1/api/haproxy/reload", Status: 200, Subject: &Subject{Kind: KindNode, ID: "n1", Name: "edge-1"}})
	_ = l.Append(Entry{Method: "DELETE", Path: "/api/nodes/n2", Status: 204, Subject: &Subject{Kind: KindNode, ID: "n2", Name: "edge-2", Gone: true}})
	// Already gone when written: named by the entry before.
	_ = l.Append(Entry{Method: "DELETE", Path: "/api/nodes/n2", Status: 404, Subject: &Subject{Kind: KindNode, ID: "n2"}})
	if p, _ := l.Query(Filter{Subject: "node:n2"}, "", 1); p.Total != 2 || p.Entries[0].Status != 404 || p.Entries[0].Subject.Name != "edge-2" {
		t.Fatalf("a subject already gone: %+v", p)
	}
	p, _ := l.Query(Filter{Subject: "node:n1"}, "", 10)
	if p.Total != 2 || len(p.Entries) != 2 || p.Entries[1].Subject == nil || p.Entries[1].Subject.Name != "edge-1" {
		t.Fatalf("%+v", p)
	}
	// Gone is the reader's, never written.
	data, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if strings.Contains(string(data), "gone") {
		t.Errorf("written: %s", data)
	}
	fc, err := l.Facets()
	if err != nil {
		t.Fatal(err)
	}
	want := []Subject{{Kind: KindNode, ID: "n1", Name: "edge-1"}, {Kind: KindNode, ID: "n2", Name: "edge-2"}}
	if !slices.Equal(fc.Subjects, want) || !slices.Equal(fc.Methods, []string{"DELETE", "PATCH", "POST"}) {
		// edge-2's name kept although the newest entry about it had none.
		t.Fatalf("facets: %+v", fc)
	}
}

func TestFacets(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{
		{User: "rita", Via: "session", Method: "POST", Path: "/api/auth/login"},
		{User: "olga", Via: "token t1", Method: "PATCH", Path: "/api/machines/m1", Subject: &Subject{Kind: KindMachine, ID: "m1", Name: "old-name"}},
		{User: "olga", Via: "token t1", Method: "PATCH", Path: "/api/machines/m1", Subject: &Subject{Kind: KindMachine, ID: "m1", Name: "new-name"}},
		{Method: "POST", Path: "/api/auth/login", Status: 401},
		{User: "root", Via: "session", Method: "DELETE", Path: "/api/clusters/c1"},
	} {
		if err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	fc, _ := l.Facets()
	if !slices.Equal(fc.Users, []string{"olga", "rita", "root"}) || !slices.Equal(fc.Vias, []string{"session", "token t1"}) {
		t.Fatalf("%+v", fc)
	}
	want := []Subject{{Kind: KindCluster, ID: "c1"}, {Kind: KindMachine, ID: "m1", Name: "new-name"}}
	if !slices.Equal(fc.Subjects, want) {
		t.Fatalf("subjects: %+v", fc.Subjects)
	}
}

// fillToMax fills both files to MaxSize with entries like a busy
// Controller's: node pages' calls, sign-ins, machines.
func fillToMax(tb testing.TB, dir string) *Log {
	tb.Helper()
	l, err := Open(dir)
	if err != nil {
		tb.Fatal(err)
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; ; i++ {
		e := Entry{Time: start.Add(time.Duration(i) * time.Minute), User: []string{"admin", "rita", "olga", "automation"}[i%4], Via: "session", Method: "POST", Status: 200, Client: fmt.Sprintf("10.1.2.%d", i%50)}
		switch i % 5 {
		case 0:
			e.Path = fmt.Sprintf("/nodes/%032x/api/haproxy/config", i%40)
			e.Subject = &Subject{Kind: KindNode, ID: fmt.Sprintf("%032x", i%40), Name: fmt.Sprintf("edge-%d", i%40)}
		case 1:
			e.Path, e.Status, e.Via = "/api/auth/login", []int{204, 401}[i%2], ""
		case 2:
			e.Method, e.Path = "PATCH", fmt.Sprintf("/api/machines/%032x", i%30)
			e.Subject = &Subject{Kind: KindMachine, ID: fmt.Sprintf("%032x", i%30), Name: fmt.Sprintf("edge-%d", i%30)}
		case 3:
			e.Via, e.Path = "token 7f3a9c21", "/api/clusters/0d9e4c1b-1111-4222-8333-944455556666/apply"
			e.Subject = &Subject{Kind: KindCluster, ID: "0d9e4c1b-1111-4222-8333-944455556666", Name: "edge"}
		case 4:
			e.Method, e.Path, e.Status = "PUT", "/api/log-forwarding", 400
		}
		if err := l.Append(e); err != nil {
			tb.Fatal(err)
		}
		if fi, err := os.Stat(filepath.Join(dir, "audit.jsonl.1")); err == nil && fi.Size() > MaxSize-300 && l.size > MaxSize-300 {
			return l
		}
	}
}

// The reading cost of a page with both files full (MaxSize each): what
// GET /api/audit pays per request.
func BenchmarkQueryFull(b *testing.B) {
	l := fillToMax(b, b.TempDir())
	all, _ := l.Query(Filter{}, "", 1)
	for _, bc := range []struct {
		name string
		f    Filter
	}{
		{"none", Filter{}},
		{"user+result", Filter{User: "rita", Result: "refused"}},
		{"subject", Filter{Subject: "node:" + fmt.Sprintf("%032x", 7)}},
		{"category+text", Filter{Category: CatMachines, Text: "edge-1"}},
		{"nothing-matches", Filter{User: "nobody"}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := l.Query(bc.f, "", 100); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(all.Total), "entries")
		})
	}
	b.Run("facets", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := l.Facets(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// Pages read while entries are appended and the file rotates: no race,
// every page newest first.
func TestQueryWhileAppending(t *testing.T) {
	old := MaxSize
	MaxSize = 4000
	t.Cleanup(func() { MaxSize = old })
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 300 {
			_ = l.Append(Entry{User: "w", Method: "POST", Path: fmt.Sprintf("/api/nodes/n%d", i%3), Status: 200, Subject: &Subject{Kind: KindNode, ID: fmt.Sprintf("n%d", i%3), Name: "edge"}})
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
		}
		before := ""
		for range 5 {
			p, err := l.Query(Filter{Subject: "node:n1"}, before, 4)
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i < len(p.Entries); i++ {
				if p.Entries[i].ID >= p.Entries[i-1].ID {
					t.Fatalf("not newest first: %v", ids(p.Entries))
				}
			}
			if before = p.Next; before == "" {
				break
			}
		}
		if _, err := l.Facets(); err != nil {
			t.Fatal(err)
		}
	}
}
