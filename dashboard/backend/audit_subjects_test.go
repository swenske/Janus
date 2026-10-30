package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// auditPage is GET /api/audit's answer: its entries and headers.
func (a *authApp) auditPage(t *testing.T, cred, query string) (int, []audit.Entry, http.Header) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/audit?"+query, nil)
	r.RemoteAddr = "192.0.2.7:4242"
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cred})
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, r)
	var out []audit.Entry
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", rec.Body, err)
		}
	}
	return rec.Code, out, rec.Result().Header
}

// TestAuditNamesItsSubject: an entry names what it's about as it was
// then - a node's page's call, a label change, the node's deletion
// (named before it's gone), what a creation made; GET /api/audit says
// which are gone, names the entries written before subjects from what's
// still there, and filters by subject - a removed node's entries
// included.
func TestAuditNamesItsSubject(t *testing.T) {
	a := newAuthApp(t)
	root := a.login(t, "root", "root-password")
	edge1 := &store.Node{Name: "edge-1", Address: "127.0.0.1:1", CACertPEM: []byte("ca"), Fleet: true}
	edge2 := &store.Node{Name: "edge-2", Address: "127.0.0.1:1", CACertPEM: []byte("ca"), Fleet: true}
	for _, n := range []*store.Node{edge1, edge2} {
		if err := a.store.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	// An entry from before subjects, about edge-2: named from the store.
	if err := a.audit.Append(audit.Entry{Time: time.Now().UTC(), User: "root", Via: "session", Method: "PATCH", Path: "/api/nodes/" + edge2.ID, Status: 200}); err != nil {
		t.Fatal(err)
	}
	if code, body := a.req(t, "PATCH", "/api/nodes/"+edge1.ID, root, map[string]any{"labels": map[string]string{"team": "web"}}); code != http.StatusOK {
		t.Fatalf("labels: %d %s", code, body)
	}
	if code, body := a.req(t, "DELETE", "/api/nodes/"+edge1.ID, root, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, body)
	}
	if code, body := a.req(t, "POST", "/api/users", root, map[string]any{"name": "nina", "role": "reader"}); code != http.StatusCreated {
		t.Fatalf("an account: %d %s", code, body)
	}

	code, entries, h := a.auditPage(t, root, "subject=node:"+url.QueryEscape(edge1.ID))
	if code != http.StatusOK || len(entries) != 2 || h.Get("X-Total-Count") != "2" || h.Get("X-Next-Before") != "" {
		t.Fatalf("edge-1's entries: %d %+v %v", code, entries, h)
	}
	for _, e := range entries {
		if s := e.Subject; s == nil || s.Name != "edge-1" || !s.Gone {
			t.Errorf("%s %s: subject %+v, want edge-1, gone", e.Method, e.Path, s)
		}
	}
	if entries[0].Method != "DELETE" {
		t.Errorf("newest first: %+v", entries[0])
	}
	_, entries, _ = a.auditPage(t, root, "subject=node:"+url.QueryEscape(edge2.ID))
	if len(entries) != 1 || entries[0].Subject == nil || entries[0].Subject.Name != "edge-2" || entries[0].Subject.Gone {
		t.Fatalf("an old entry, named now: %+v", entries)
	}
	_, entries, _ = a.auditPage(t, root, "kind=accounts&method=post")
	if len(entries) != 1 || entries[0].Subject == nil || *entries[0].Subject != (audit.Subject{Kind: audit.KindAccount, ID: "nina", Name: "nina"}) {
		t.Fatalf("a creation names what it made: %+v", entries)
	}

	// The facets: every subject the audit names, the removed node too.
	r := httptest.NewRequest("GET", "/api/audit?facets=1", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: root})
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, r)
	var fc audit.Facets
	if err := json.Unmarshal(rec.Body.Bytes(), &fc); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("facets: %d %s", rec.Code, rec.Body)
	}
	found := map[string]audit.Subject{}
	for _, s := range fc.Subjects {
		found[s.Key()] = s
	}
	if s := found["node:"+edge1.ID]; s.Name != "edge-1" || !s.Gone {
		t.Errorf("facets, edge-1: %+v", fc.Subjects)
	}
	if s := found["node:"+edge2.ID]; s.Name != "edge-2" || s.Gone {
		t.Errorf("facets, edge-2: %+v", fc.Subjects)
	}
	if !strings.Contains(strings.Join(fc.Users, ","), "root") {
		t.Errorf("facets, users: %v", fc.Users)
	}

	// The record the log shipper sends names it too.
	all, _ := a.audit.Recent(10, func(e audit.Entry) bool { return e.Method == "DELETE" })
	rec2 := controllerAuditRecord(all[0])
	if !strings.HasPrefix(rec2.Message, "DELETE /api/nodes/"+edge1.ID+" 204 by root (session)") || !strings.HasSuffix(rec2.Message, ", node edge-1") {
		t.Errorf("record: %q", rec2.Message)
	}
	if s, _ := rec2.Fields["audit"].(map[string]any)["subject"].(map[string]any); s["name"] != "edge-1" || s["kind"] != "node" || s["id"] != edge1.ID {
		t.Errorf("record fields: %+v", rec2.Fields)
	}
	// A name a node announced: no line of its own in the message.
	if got := aboutSubject(&audit.Subject{Kind: "pending", ID: "p1", Name: "evil\nPUT /api/settings 200 by admin"}); got != ", pending evilPUT /api/settings 200 by admin" {
		t.Errorf("aboutSubject: %q", got)
	}
}

// TestAuditPagesAndFilters: GET /api/audit's pages, its filters, its
// refusals.
func TestAuditPagesAndFilters(t *testing.T) {
	a := newAuthApp(t)
	rita := a.login(t, "rita", "rita-password")
	a.req(t, "POST", "/api/auth/login", "", map[string]string{"name": "mallory", "password": "guess-guess"})
	a.req(t, "POST", "/api/hypervisors", rita, map[string]string{"name": "h"})
	root := a.login(t, "root", "root-password")
	for i := range 5 {
		a.req(t, "PUT", "/api/settings", root, map[string]int{"session_idle_minutes": 30 + i})
	}

	// A page at a time: every entry once, the total each time.
	var got []uint64
	before := ""
	for range 20 {
		q := "limit=2"
		if before != "" {
			q += "&before=" + before
		}
		code, entries, h := a.auditPage(t, root, q)
		if code != http.StatusOK || h.Get("X-Total-Count") != "9" {
			t.Fatalf("%s: %d, total %q", q, code, h.Get("X-Total-Count"))
		}
		for _, e := range entries {
			got = append(got, e.ID)
		}
		if before = h.Get("X-Next-Before"); before == "" {
			break
		}
	}
	if len(got) != 9 || got[0] != 9 || got[8] != 1 {
		t.Fatalf("pages: %v", got)
	}

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"kind=signin", 3},
		{"kind=signin&result=refused", 1},
		{"user=mallory", 1},
		{"result=refused&user=rita", 1},
		{"via=session&method=PUT", 5},
		{"via=none", 3},
		{"q=SETTINGS", 5},
		{"client=192.0.2.", 9},
		{"client=10.", 0},
		{"from=" + url.QueryEscape(time.Now().Add(-time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(time.Now().Add(time.Hour).Format(time.RFC3339)), 9},
		{"to=2020-01-01T00:00:00Z", 0},
	} {
		code, entries, h := a.auditPage(t, root, tc.query)
		if code != http.StatusOK || len(entries) != tc.want || h.Get("X-Total-Count") != strconv.Itoa(tc.want) {
			t.Errorf("%s: %d, %d entries, total %s - want %d", tc.query, code, len(entries), h.Get("X-Total-Count"), tc.want)
		}
	}
	for _, q := range []string{"result=maybe", "kind=nope", "from=yesterday", "before=x", "before=0"} {
		if code, _, _ := a.auditPage(t, root, q); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, code)
		}
	}
	// An admin's, filters or not.
	if code, _, _ := a.auditPage(t, rita, "facets=1"); code != http.StatusForbidden {
		t.Errorf("a reader's facets: %d", code)
	}
}
