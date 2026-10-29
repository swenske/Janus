// Package haproxyflow reads where an HAProxy configuration may send
// traffic: which backends each frontend (or listen section) reaches -
// its default_backend, its use_backend rules, a listen section's own
// servers, and a map's values when use_backend reads one. "show stat"
// counts each proxy but never says which frontend sends to which
// backend: that is only in the configuration.
//
// janusctl tui's Traffic flow uses it; the Controller's node page reads
// configurations the same way in the browser
// (dashboard/frontend/src/node/flow.js), and testdata/cases.json holds
// both to the same answers.
package haproxyflow

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Topology is what a configuration says about where traffic goes.
type Topology struct {
	Frontends []Frontend
	// Backends are the backend and listen sections, in order.
	Backends []string
	// Maps are the map files a dynamic use_backend reads.
	Maps []string
}

// Frontend is a frontend or listen section.
type Frontend struct {
	Name string
	Kind string // frontend or listen
	Mode string
	// Binds are its addresses, as written.
	Binds []string
	// TLSBinds are those of Binds with ssl.
	TLSBinds []string
	// ACLs are its named ACLs: each line's words after the name (the
	// fetch, its flags, its values), by name - one name may have
	// several lines.
	ACLs map[string][][]string
	// Answers are what it answers itself with: return, redirect,
	// use-service <name>, stats page.
	Answers []string
	Rules   []Rule
}

// Rule is one way a frontend picks a backend.
type Rule struct {
	Type      string // use, dynamic, default, self
	Backend   string // use, default, self
	Cond      string // use, dynamic: "if ..." or "unless ..."
	Inherited bool   // default: from a defaults section
	// A dynamic rule: use_backend <prefix>%[expr]<suffix>, and what the
	// expression reads - maps, str() literals through a set-var.
	Format   string
	Prefix   string
	Suffix   string
	MapCalls []MapCall
	Literals []string
	// Exprs are the sample expressions it reads: its own, and those
	// set-var gives the variables it reads.
	Exprs []string
}

// MapCall is a map converter: the file it reads and the value it gives
// when no key matches - and the converter, "map", "map_dom", "map_reg"
// (how its keys match).
type MapCall struct{ Path, Default, Converter string }

// Link is a frontend that may send to a backend, and the rules that do.
type Link struct {
	Frontend string     `json:"frontend"`
	Backend  string     `json:"backend"`
	Rules    []LinkRule `json:"rules"`
}

// LinkRule is one rule behind a link: its line, or the map keys that
// pick the backend.
type LinkRule struct {
	Text      string   `json:"text"`
	Map       string   `json:"map,omitempty"`
	Keys      []string `json:"keys,omitempty"`
	IsDefault bool     `json:"isDefault,omitempty"` // the map's default: no key matched
	Via       string   `json:"via,omitempty"`       // set-var: the backend named in a variable
}

// Unresolved is a dynamic use_backend nothing resolved: what it reads
// is unknown or couldn't be read.
type Unresolved struct {
	Frontend string   `json:"frontend"`
	Format   string   `json:"format"`
	Maps     []string `json:"maps"`
}

// sections are the top-level keywords that open a section: any of them
// ends the proxy being read.
var sections = map[string]bool{
	"global": true, "defaults": true, "frontend": true, "backend": true, "listen": true, "peers": true, "resolvers": true,
	"userlist": true, "mailers": true, "program": true, "http-errors": true, "ring": true, "cache": true, "crt-store": true,
	"log-forward": true, "traces": true, "acme": true, "fcgi-app": true,
}

// Tokenize splits a configuration line into words as HAProxy does:
// blanks separate them, quotes group them ('...' as is, "..." with
// backslash escapes), a backslash escapes the next character, and an
// unquoted # starts a comment.
func Tokenize(line string) []string {
	var words []string
	for _, w := range Words(line) {
		words = append(words, w.Text)
	}
	return words
}

// Word is one word of a configuration line: its text as HAProxy reads
// it, and the bytes it takes in the line, quotes and backslashes
// included.
type Word struct {
	Text       string
	Start, End int
}

// Words splits a configuration line as Tokenize does, keeping where
// each word stands.
func Words(line string) []Word {
	var words []Word
	var cur strings.Builder
	start := -1 // between words; "" is still a word
	end := len(line)
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			switch {
			case c == quote:
				quote = 0
			case c == '\\' && quote == '"' && i+1 < len(line):
				i += escaped(&cur, line[i+1:])
			default:
				cur.WriteByte(c)
			}
			continue
		}
		if c == '#' {
			end = i
			break
		}
		if c == ' ' || c == '\t' || c == '\r' {
			if start >= 0 {
				words = append(words, Word{cur.String(), start, i})
			}
			cur.Reset()
			start = -1
			continue
		}
		if start < 0 {
			start = i
		}
		switch {
		case c == '"' || c == '\'':
			quote = c
		case c == '\\' && i+1 < len(line):
			i += escaped(&cur, line[i+1:])
		default:
			cur.WriteByte(c)
		}
	}
	if start >= 0 {
		words = append(words, Word{cur.String(), start, end})
	}
	return words
}

// escaped writes the character s starts with - the one a backslash
// escapes - and is how many bytes it takes.
func escaped(b *strings.Builder, s string) int {
	_, n := utf8.DecodeRuneInString(s)
	b.WriteString(s[:n])
	return n
}

// sectionArgs reads "<name> [args...] [from <defaults>]".
func sectionArgs(args []string) (name string, extra []string, from string) {
	own := args
	for i, a := range args {
		if a == "from" {
			own = args[:i]
			if i+1 < len(args) {
				from = args[i+1]
			}
			break
		}
	}
	if len(own) > 0 {
		name, extra = own[0], own[1:]
	}
	return name, extra, from
}

var (
	setVar  = regexp.MustCompile(`^set-var(?:-fmt)?\(([^,)]+)`)
	mapCall = regexp.MustCompile(`\b(map(?:_[a-z0-9_]+)?)\(([^)]*)\)`)
	readVar = regexp.MustCompile(`\bvar\(([^,)]+)`)
	strLit  = regexp.MustCompile(`^str\(([^)]*)\)$`)
)

// varName is the variable a "set-var(txn.x[,cond])" or
// "set-var-fmt(txn.x)" word sets, or "".
func varName(word string) string {
	if m := setVar.FindStringSubmatch(word); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func unquote(s string) string {
	t := strings.TrimSpace(s)
	if len(t) >= 2 && (t[0] == '"' || t[0] == '\'') && t[len(t)-1] == t[0] {
		return t[1 : len(t)-1]
	}
	return t
}

// MapCalls finds the map converters in a sample expression -
// map(file[,default]), map_str(...), map_dom(...), map_str_int(...) -
// as the file each reads and the value it gives when no key matches.
func MapCalls(expr string) []MapCall {
	var out []MapCall
	for _, m := range mapCall.FindAllStringSubmatch(expr, -1) {
		args := strings.Split(m[2], ",")
		path := unquote(args[0])
		if path == "" {
			continue
		}
		mc := MapCall{Path: path, Converter: m[1]}
		if len(args) > 1 {
			mc.Default = unquote(strings.Join(args[1:], ","))
		}
		out = append(out, mc)
	}
	return out
}

// splitFormat cuts a log-format string with exactly one %[...]
// expression into what comes before, the expression and what comes
// after; false for anything else.
func splitFormat(format string) (prefix, expr, suffix string, ok bool) {
	start := strings.Index(format, "%[")
	if start < 0 || strings.Contains(format[start+2:], "%[") {
		return "", "", "", false
	}
	depth := 0
	for i := start + 1; i < len(format); i++ {
		switch format[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return format[:start], format[start+2 : i], format[i+1:], true
			}
		}
	}
	return "", "", "", false
}

// proxy is a section being read.
type proxy struct {
	kind           string // defaults, frontend, listen, backend
	name           string
	mode           string
	defaultBackend string
	inherited      string
	binds          []string
	tlsBinds       []string
	acls           map[string][][]string
	answers        []string
	uses           [][2]string // target, condition
	vars           map[string][]string
}

func (p *proxy) answer(a string) {
	for _, have := range p.answers {
		if have == a {
			return
		}
	}
	p.answers = append(p.answers, a)
}

// Parse reads which backends each frontend (or listen) section may send
// to. Conditional blocks (.if/.endif) are read as if they all held: a
// link that may exist is shown.
func Parse(cfg string) Topology {
	named := map[string]*proxy{}     // named defaults sections
	last := &proxy{kind: "defaults"} // what a proxy without "from" inherits
	var fronts []*proxy
	var t Topology
	var cur *proxy

	for _, raw := range strings.Split(cfg, "\n") {
		w := Tokenize(raw)
		if len(w) == 0 || strings.HasPrefix(w[0], ".") {
			continue
		}
		if sections[w[0]] {
			name, extra, from := sectionArgs(w[1:])
			base := last
			if from != "" {
				if d, ok := named[from]; ok {
					base = d
				}
			}
			switch w[0] {
			case "defaults":
				// A defaults section starts over, or from the one it names.
				cur = &proxy{kind: "defaults"}
				if from != "" {
					cur.defaultBackend, cur.mode = base.defaultBackend, base.mode
				}
				if name != "" {
					named[name] = cur
				}
				last = cur
			case "frontend", "listen":
				cur = &proxy{kind: w[0], name: name, mode: base.mode, inherited: base.defaultBackend, vars: map[string][]string{}, acls: map[string][][]string{}}
				if len(extra) > 0 {
					cur.binds = append(cur.binds, extra[0]) // "listen name :80": the old inline bind
				}
				fronts = append(fronts, cur)
				if w[0] == "listen" {
					t.Backends = append(t.Backends, name)
				}
			case "backend":
				cur = &proxy{kind: "backend", name: name}
				t.Backends = append(t.Backends, name)
			default:
				cur = nil
			}
			continue
		}
		if cur == nil || cur.kind == "backend" {
			continue
		}
		kw := w[0]
		arg := func(i int) string {
			if i < len(w) {
				return w[i]
			}
			return ""
		}
		switch {
		case kw == "mode" && arg(1) != "":
			cur.mode = w[1]
		case kw == "default_backend" && arg(1) != "":
			cur.defaultBackend = w[1]
		}
		if cur.kind == "defaults" {
			continue
		}

		switch {
		case kw == "bind" && arg(1) != "":
			cur.binds = append(cur.binds, w[1])
			if slices.Contains(w[2:], "ssl") {
				cur.tlsBinds = append(cur.tlsBinds, w[1])
			}
		case kw == "acl" && arg(2) != "":
			cur.acls[w[1]] = append(cur.acls[w[1]], w[2:])
		case kw == "use_backend" && arg(1) != "":
			cond := ""
			if arg(2) == "if" || arg(2) == "unless" {
				cond = strings.Join(w[2:], " ")
			}
			cur.uses = append(cur.uses, [2]string{w[1], cond})
		case kw == "http-request" && (arg(1) == "return" || arg(1) == "redirect"):
			cur.answer(w[1])
		case kw == "http-request" && arg(1) == "use-service", kw == "tcp-request" && arg(2) == "use-service":
			at := 2
			if kw == "tcp-request" {
				at = 3
			}
			cur.answer(strings.TrimSpace("use-service " + arg(at)))
		case kw == "stats" && (arg(1) == "enable" || arg(1) == "uri"):
			cur.answer("stats page")
		}
		// Variables a dynamic use_backend may read: set-var(txn.be) <expr>.
		if kw == "http-request" || kw == "tcp-request" || kw == "http-response" {
			for i := 1; i < len(w)-1; i++ {
				if v := varName(w[i]); v != "" {
					cur.vars[v] = append(cur.vars[v], w[i+1])
				}
			}
		}
	}

	seenMap := map[string]bool{}
	for _, f := range fronts {
		fe := Frontend{Name: f.name, Kind: f.kind, Mode: f.mode, Binds: f.binds, TLSBinds: f.tlsBinds, ACLs: f.acls, Answers: f.answers}
		for _, u := range f.uses {
			target, cond := u[0], u[1]
			if !strings.Contains(target, "%[") {
				fe.Rules = append(fe.Rules, Rule{Type: "use", Backend: target, Cond: cond})
				continue
			}
			r := Rule{Type: "dynamic", Format: target, Cond: cond}
			prefix, expr, suffix, ok := splitFormat(target)
			if ok {
				r.Prefix, r.Suffix = prefix, suffix
				exprs := []string{expr}
				for _, m := range readVar.FindAllStringSubmatch(expr, -1) {
					exprs = append(exprs, f.vars[strings.TrimSpace(m[1])]...)
				}
				r.Exprs = exprs
				for _, e := range exprs {
					r.MapCalls = append(r.MapCalls, MapCalls(e)...)
					// set-var(txn.be) str(be_api): a backend named as is.
					if m := strLit.FindStringSubmatch(e); m != nil {
						r.Literals = append(r.Literals, unquote(m[1]))
					}
				}
				for _, mc := range r.MapCalls {
					if !seenMap[mc.Path] {
						seenMap[mc.Path] = true
						t.Maps = append(t.Maps, mc.Path)
					}
				}
			}
			fe.Rules = append(fe.Rules, r)
		}
		switch {
		case f.defaultBackend != "":
			fe.Rules = append(fe.Rules, Rule{Type: "default", Backend: f.defaultBackend})
		case f.inherited != "":
			fe.Rules = append(fe.Rules, Rule{Type: "default", Backend: f.inherited, Inherited: true})
		case f.kind == "listen":
			fe.Rules = append(fe.Rules, Rule{Type: "self", Backend: f.name})
		}
		t.Frontends = append(t.Frontends, fe)
	}
	return t
}

// Links turns the rules into links between proxies that exist
// (backends), each with the rules that make it - a map's keys included,
// from maps (path: key: value, the maps read). A dynamic rule nothing
// resolves is listed in unresolved.
func (t Topology) Links(backends []string, maps map[string]map[string]string) (links []Link, unresolved []Unresolved) {
	known := map[string]bool{}
	for _, b := range backends {
		known[b] = true
	}
	index := map[[2]string]int{}
	add := func(fe, be string, r LinkRule) {
		if !known[be] {
			return
		}
		k := [2]string{fe, be}
		i, ok := index[k]
		if !ok {
			i = len(links)
			index[k] = i
			links = append(links, Link{Frontend: fe, Backend: be})
		}
		links[i].Rules = append(links[i].Rules, r)
	}
	withCond := func(s, cond string) string {
		if cond != "" {
			return s + " " + cond
		}
		return s
	}
	for _, f := range t.Frontends {
		for _, r := range f.Rules {
			switch r.Type {
			case "use":
				add(f.Name, r.Backend, LinkRule{Text: withCond("use_backend "+r.Backend, r.Cond)})
			case "default":
				text := "default_backend " + r.Backend
				if r.Inherited {
					text += " (from defaults)"
				}
				add(f.Name, r.Backend, LinkRule{Text: text})
			case "self":
				add(f.Name, r.Backend, LinkRule{Text: "its own servers (listen)"})
			case "dynamic":
				resolved := len(r.Literals) > 0
				for _, lit := range r.Literals {
					add(f.Name, r.Prefix+lit+r.Suffix, LinkRule{Text: withCond("use_backend "+r.Format, r.Cond), Via: "set-var"})
				}
				for _, m := range r.MapCalls {
					entries, ok := maps[m.Path]
					if !ok {
						continue
					}
					resolved = true
					keys := make([]string, 0, len(entries))
					for k := range entries {
						keys = append(keys, k)
					}
					sort.Strings(keys) // the order the Controller's JSON gives them
					var order []string
					byValue := map[string][]string{}
					for _, k := range keys {
						be := r.Prefix + entries[k] + r.Suffix
						if _, seen := byValue[be]; !seen {
							order = append(order, be)
						}
						byValue[be] = append(byValue[be], k)
					}
					for _, be := range order {
						add(f.Name, be, LinkRule{Text: "use_backend " + r.Format, Map: m.Path, Keys: byValue[be]})
					}
					if m.Default != "" {
						add(f.Name, r.Prefix+m.Default+r.Suffix, LinkRule{Text: "use_backend " + r.Format, Map: m.Path, IsDefault: true})
					}
				}
				if !resolved {
					u := Unresolved{Frontend: f.Name, Format: r.Format, Maps: []string{}}
					for _, m := range r.MapCalls {
						u.Maps = append(u.Maps, m.Path)
					}
					unresolved = append(unresolved, u)
				}
			}
		}
	}
	return links, unresolved
}
