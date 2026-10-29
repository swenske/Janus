package audit

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SubjectOf is what a request path is about, from the ID it carries -
// the Controller's routes that name one thing (kind "" for the others):
// a node, its page's calls (/nodes/<id>/api/...), a pending
// registration, a machine, a cluster, a hypervisor, an account, an API
// or enrollment token, a single sign-on provider. A query string is
// ignored.
func SubjectOf(path string) (kind, id string) {
	path, _, _ = strings.Cut(path, "?")
	seg := strings.Split(strings.TrimPrefix(path, "/"), "/")
	at := func(i int) string {
		if i < len(seg) {
			return seg[i]
		}
		return ""
	}
	if at(0) == "nodes" && at(1) != "" {
		return KindNode, at(1)
	}
	if at(0) != "api" || at(2) == "" {
		return "", ""
	}
	id = at(2)
	switch at(1) {
	case "nodes":
		if id == "status" || id == "metrics" {
			return "", ""
		}
		return KindNode, id
	case "pending":
		return KindPending, id
	case "machines":
		return KindMachine, id
	case "clusters":
		return KindCluster, id
	case "hypervisors":
		if id == "preparation" {
			return "", ""
		}
		return KindHypervisor, id
	case "users":
		return KindAccount, id
	case "tokens":
		return KindToken, id
	case "enroll-tokens":
		return KindEnrollment, id
	case "sso":
		if id == "providers" && at(3) != "" {
			return KindSSO, at(3)
		}
	}
	return "", ""
}

// The kinds of action an entry is, by its path (Category).
const (
	CatSignIn   = "signin"   // signing in or out, janusctl's sign-ins
	CatNodes    = "nodes"    // nodes, their pages' calls, approvals
	CatClusters = "clusters" // clusters
	CatFleet    = "fleet"    // the fleet's PKI, enrollment, the Controller's certificate
	CatAccounts = "accounts" // accounts, tokens, an account's own factors and keys
	CatMachines = "machines" // machines and hypervisors
	CatSettings = "settings" // the Controller's settings, backups, updates...
)

// Categories are the kinds of action, in the order a picker lists them.
var Categories = []string{CatSignIn, CatNodes, CatClusters, CatFleet, CatAccounts, CatMachines, CatSettings}

// signIns are the paths of a sign-in (or out): a password, a second
// factor, a single sign-on's end - janusctl's are all of /api/cli/.
var signIns = map[string]bool{
	"/api/auth/setup": true, "/api/auth/login": true, "/api/auth/logout": true,
	"/api/auth/mfa/totp": true, "/api/auth/mfa/recovery": true,
	"/api/auth/mfa/passkey/begin": true, "/api/auth/mfa/passkey/finish": true,
	"/api/auth/sso/finish": true,
}

// Category is the kind of action a request is (one of Categories).
func Category(method, path string) string {
	path, _, _ = strings.Cut(path, "?")
	has := func(prefix string) bool {
		return path == prefix || strings.HasPrefix(path, prefix+"/")
	}
	switch {
	case strings.HasPrefix(path, "/nodes/"), has("/api/nodes"), has("/api/pending"):
		return CatNodes
	case has("/api/clusters"):
		return CatClusters
	case has("/api/machines"), has("/api/hypervisors"):
		return CatMachines
	case has("/api/fleet"), has("/api/enroll-tokens"), has("/api/controller/tls"):
		return CatFleet
	case signIns[path] && (method != "DELETE" || path != "/api/auth/mfa/totp"), // DELETE: TOTP removed
		has("/api/cli"),
		strings.HasPrefix(path, "/api/auth/sso/") && strings.HasSuffix(path, "/start"):
		return CatSignIn
	case has("/api/users"), has("/api/tokens"), has("/api/auth"), has("/api/me"):
		return CatAccounts
	}
	return CatSettings
}

// Filter says which entries a reader wants; its zero value keeps every
// one. Its fields combine (and).
type Filter struct {
	// User is the account, exactly.
	User string
	// Via is how: "none" (no credential), or Entry.Via's value or its
	// first words - "session", "token" (any API token), "token <id>",
	// "sso", "ssh key"...
	Via string
	// Subject is what it's about: Subject.Key ("node:<id>").
	Subject string
	// Result: "ok" (below 400), "refused" (4xx), "failed" (5xx).
	Result string
	// Category is the kind of action (Categories).
	Category string
	Method   string
	// From and To bound the time (zero: unbounded), To excluded.
	From, To time.Time
	// Client is the address the request came from, or its start
	// ("10.1.2.").
	Client string
	// Text is found in the path or the subject's name, in any case.
	Text string
}

// Check refuses a filter with an unknown result or category.
func (f Filter) Check() error {
	switch f.Result {
	case "", "ok", "refused", "failed":
	default:
		return fmt.Errorf("result %q: want ok, refused or failed", f.Result)
	}
	if f.Category != "" && !slices.Contains(Categories, f.Category) {
		return fmt.Errorf("kind %q: want one of %s", f.Category, strings.Join(Categories, ", "))
	}
	return nil
}

// subjectOf is e's subject: its own, or the one its path names (an
// entry written before subjects) - nil for neither.
func subjectOf(e Entry) *Subject {
	if e.Subject != nil {
		return e.Subject
	}
	if kind, id := SubjectOf(e.Path); kind != "" {
		return &Subject{Kind: kind, ID: id}
	}
	return nil
}

// Match reports whether f keeps e.
func (f Filter) Match(e Entry) bool {
	if f.User != "" && e.User != f.User {
		return false
	}
	switch f.Via {
	case "":
	case "none":
		if e.Via != "" {
			return false
		}
	default:
		if e.Via != f.Via && !strings.HasPrefix(e.Via, f.Via+" ") {
			return false
		}
	}
	switch f.Result {
	case "ok":
		if e.Status >= 400 {
			return false
		}
	case "refused":
		if e.Status < 400 || e.Status >= 500 {
			return false
		}
	case "failed":
		if e.Status < 500 {
			return false
		}
	}
	if f.Method != "" && e.Method != f.Method {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) || !f.To.IsZero() && !e.Time.Before(f.To) {
		return false
	}
	if f.Client != "" && !strings.HasPrefix(e.Client, f.Client) {
		return false
	}
	if f.Category != "" && Category(e.Method, e.Path) != f.Category {
		return false
	}
	if f.Subject != "" || f.Text != "" {
		s := subjectOf(e)
		if f.Subject != "" && (s == nil || s.Key() != f.Subject) {
			return false
		}
		if f.Text != "" && !containsFold(e.Path, f.Text) && (s == nil || !containsFold(s.Name, f.Text)) {
			return false
		}
	}
	return true
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// Page is one page of a Query, newest first.
type Page struct {
	Entries []Entry
	// Total is how many entries the filter keeps, in both files.
	Total int
	// Next is the cursor of the page after this one (older), "" when
	// none is left.
	Next string
}

// Query returns up to limit entries f keeps, newest first, older than
// the cursor before ("": from the newest) - Page.Next is the next
// page's. Each entry has its subject: an entry written before subjects
// gets the one its path names; one without a name (from before
// subjects, or already gone when written) is named by the newest entry
// about it that had one.
//
// A cursor is the ID of the last entry a page returned, or, for an entry
// without one (written before IDs existed), "<ID>~<n>": the n entries
// below the entry with that ID - the nearest above it that has one -
// skipped ("0~<n>": the n newest skipped). Nothing in it needs escaping
// in a URL. They stay valid as entries are appended and the files
// rotate, until the entry itself is rotated away - then the page goes on
// from the newest entry older than it.
func (l *Log) Query(f Filter, before string, limit int) (Page, error) {
	limit = max(limit, 1)
	anchor, skip, err := parseCursor(before)
	if err != nil {
		return Page{}, err
	}
	l.mu.Lock()
	all, err := l.all()
	l.mu.Unlock()
	if err != nil {
		return Page{}, err
	}
	// all is oldest first: the page walks it from the end. start is the
	// index (in all) of the first entry the page may return.
	start := len(all) - 1
	if anchor > 0 || skip > 0 {
		at := len(all) // the anchor's index; len(all): above the top
		if anchor > 0 {
			at = -1
			for i := len(all) - 1; i >= 0; i-- {
				if all[i].ID == anchor {
					at = i
					break
				}
			}
		}
		if at >= 0 {
			start = at - 1 - skip
		} else {
			// Rotated away: from the newest entry older than it.
			start = -1
			for i := len(all) - 1; i >= 0; i-- {
				if all[i].ID < anchor {
					start = i
					break
				}
			}
		}
	}
	var p Page
	last := -1
	names := map[string]string{}
	for i := len(all) - 1; i >= 0; i-- {
		e := all[i]
		if e.Subject != nil && e.Subject.Name != "" {
			if _, ok := names[e.Subject.Key()]; !ok {
				names[e.Subject.Key()] = e.Subject.Name // the newest name it had
			}
		}
		if !f.Match(e) {
			continue
		}
		p.Total++
		if i > start {
			continue
		}
		if len(p.Entries) < limit {
			p.Entries = append(p.Entries, e)
			last = i
		} else if p.Next == "" {
			p.Next = cursorAt(all, last)
		}
	}
	for i := range p.Entries {
		e := &p.Entries[i]
		e.Subject = subjectOf(*e)
		if s := e.Subject; s != nil && s.Name == "" {
			s.Name = names[s.Key()]
		}
	}
	return p, nil
}

// cursorAt is the cursor past all[i].
func cursorAt(all []Entry, i int) string {
	if all[i].ID > 0 {
		return strconv.FormatUint(all[i].ID, 10)
	}
	// n counts the entries from the one just below the anchor to all[i].
	for j := i + 1; j < len(all); j++ {
		if all[j].ID > 0 {
			return fmt.Sprintf("%d~%d", all[j].ID, j-i)
		}
	}
	return fmt.Sprintf("0~%d", len(all)-i)
}

// ErrCursor is a cursor Query can't read.
var ErrCursor = errors.New("audit cursor")

func parseCursor(c string) (anchor uint64, skip int, err error) {
	if c == "" {
		return 0, 0, nil
	}
	a, s, plus := strings.Cut(c, "~")
	if anchor, err = strconv.ParseUint(a, 10, 64); err == nil && plus {
		skip, err = strconv.Atoi(s)
		if err == nil && skip < 0 {
			err = errors.New("negative")
		}
	}
	if err != nil || anchor == 0 && skip == 0 {
		return 0, 0, fmt.Errorf("%w %q: want an entry's ID, or <ID>~<n>", ErrCursor, c)
	}
	return anchor, skip, nil
}

// Facets are what the entries name - the choices of a reader's filters.
type Facets struct {
	// Users are the accounts named, sorted.
	Users []string `json:"users"`
	// Vias are the ways they came (Entry.Via), sorted.
	Vias []string `json:"vias"`
	// Subjects are what the entries are about, each with the newest
	// name an entry gave it, by kind then name.
	Subjects []Subject `json:"subjects"`
	// Methods are the methods seen, sorted.
	Methods []string `json:"methods"`
}

// Facets reads what the entries kept name.
func (l *Log) Facets() (Facets, error) {
	l.mu.Lock()
	all, err := l.all()
	l.mu.Unlock()
	if err != nil {
		return Facets{}, err
	}
	users, vias, methods := map[string]bool{}, map[string]bool{}, map[string]bool{}
	subjects := map[string]*Subject{}
	for _, e := range all { // oldest first: the newest name wins
		if e.User != "" {
			users[e.User] = true
		}
		if e.Via != "" {
			vias[e.Via] = true
		}
		methods[e.Method] = true
		s := subjectOf(e)
		if s == nil {
			continue
		}
		if have, ok := subjects[s.Key()]; !ok {
			c := *s
			subjects[s.Key()] = &c
		} else if s.Name != "" {
			have.Name = s.Name
		}
	}
	out := Facets{Users: sortedKeys(users), Vias: sortedKeys(vias), Methods: sortedKeys(methods), Subjects: []Subject{}}
	for _, s := range subjects {
		out.Subjects = append(out.Subjects, *s)
	}
	slices.SortFunc(out.Subjects, func(a, b Subject) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
