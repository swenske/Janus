package main

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
)

// The audit names what each entry is about (audit.Subject): the
// middleware looks the path's ID up before the handler runs - a
// deletion still names what it removed -, a handler that made something
// says what (noteAuditSubject), and GET /api/audit names the older
// entries' subjects from what's still there.

// subjectName is the name of kind's id now - false: there's none any
// more (an account's name is its ID either way).
func (a *app) subjectName(kind, id string) (string, bool) {
	switch kind {
	case audit.KindNode:
		if a.store != nil {
			if n, ok := a.store.Get(id); ok {
				return n.Name, true
			}
		}
	case audit.KindPending:
		if a.pending != nil {
			if p, ok := a.pending.Get(id); ok {
				return p.Name, true
			}
		}
	case audit.KindMachine:
		if a.machines != nil {
			if m, ok := a.machines.Get(id); ok {
				return m.Spec.Name, true
			}
		}
	case audit.KindCluster:
		if a.clusters != nil && a.clusters.Store != nil {
			if c, ok := a.clusters.Store.Get(id); ok {
				return c.Name, true
			}
		}
	case audit.KindHypervisor:
		if a.hypervisors != nil {
			if h, ok := a.hypervisors.Get(id); ok {
				return h.Name, true
			}
		}
	case audit.KindAccount:
		if a.auth != nil {
			_, ok := a.auth.User(id)
			return id, ok
		}
		return id, false
	case audit.KindToken:
		if a.tokens != nil {
			if t, ok := a.tokens.Get(id); ok {
				return t.Name, true
			}
		}
	case audit.KindEnrollment:
		if a.enroll != nil {
			for _, t := range a.enroll.List() {
				if t.ID == id {
					return t.Name, true
				}
			}
		}
	case audit.KindSSO:
		if a.sso != nil && a.sso.store != nil {
			if p, ok := a.sso.store.Get(id); ok {
				return p.Name, true
			}
		}
	}
	return "", false
}

// auditSubjectOf is what a request to path is about, named as it is now
// - before its handler changes anything; nil when the path names
// nothing.
func (a *app) auditSubjectOf(path string) *audit.Subject {
	kind, id := audit.SubjectOf(path)
	if kind == "" {
		return nil
	}
	name, _ := a.subjectName(kind, id)
	return &audit.Subject{Kind: kind, ID: id, Name: name}
}

// noteAuditSubject tells the audit what r was about, when its path
// couldn't: what its handler made (a node added or approved, a machine
// created...).
func noteAuditSubject(r *http.Request, kind, id, name string) {
	if n, ok := r.Context().Value(auditNoteKey{}).(*auditNote); ok {
		n.subject = &audit.Subject{Kind: kind, ID: id, Name: name}
	}
}

// aboutSubject ends an audit line: ", node edge-1" (nothing when it has
// no name) - a name a node announced itself, its control characters
// dropped: it can't forge a line of its own.
func aboutSubject(s *audit.Subject) string {
	if s == nil || s.Name == "" {
		return ""
	}
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s.Name)
	return ", " + s.Kind + " " + name
}

// checkSubject says whether s still exists, and names it from what's
// there when its entry couldn't (one from before subjects).
func (a *app) checkSubject(s *audit.Subject) {
	name, ok := a.subjectName(s.Kind, s.ID)
	s.Gone = !ok
	if s.Name == "" && name != "" {
		s.Name = name
	}
}

// auditFilter reads GET /api/audit's filters.
func auditFilter(q url.Values) (audit.Filter, error) {
	f := audit.Filter{
		User:     q.Get("user"),
		Via:      q.Get("via"),
		Subject:  q.Get("subject"),
		Result:   q.Get("result"),
		Category: q.Get("kind"),
		Method:   strings.ToUpper(q.Get("method")),
		Client:   q.Get("client"),
		Text:     q.Get("q"),
	}
	for name, t := range map[string]*time.Time{"from": &f.From, "to": &f.To} {
		if v := q.Get(name); v != "" {
			var err error
			if *t, err = time.Parse(time.RFC3339, v); err != nil {
				return f, errors.New(name + ": want an RFC 3339 time (2026-10-29T14:00:00Z)")
			}
		}
	}
	return f, f.Check()
}

// handleAudit is GET /api/audit: a page of entries, newest first,
// filtered - user, via, subject (kind:id), result (ok, refused,
// failed), kind (audit.Categories), method, from/to (RFC 3339), client
// (an address or its start), q (in the path or the subject's name) -,
// older than before (the X-Next-Before of the previous page), limit of
// them (500 by default, 5000 at most). X-Total-Count is how many match
// in all. ?facets=1 answers the names the filters can take instead
// (audit.Facets).
func (a *app) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("facets") != "" {
		fc, err := a.audit.Facets()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for i := range fc.Subjects {
			a.checkSubject(&fc.Subjects[i])
		}
		writeJSON(w, http.StatusOK, fc)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	f, err := auditFilter(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := a.audit.Query(f, q.Get("before"), limit)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, audit.ErrCursor) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	for i := range p.Entries {
		if s := p.Entries[i].Subject; s != nil {
			a.checkSubject(s)
		}
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(p.Total))
	if p.Next != "" {
		w.Header().Set("X-Next-Before", p.Next)
	}
	writeJSON(w, http.StatusOK, nonNil(p.Entries))
}
