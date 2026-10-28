package main

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A shell command line read the way a POSIX shell (bash, zsh) would
// split it into commands and words - what janusctl __effect needs to
// find the janusctl invocations in a command Claude Code is about to run
// (effect_hook.go). Nothing is expanded or run: a word holding an
// expansion ($VAR, $(...), `...`, a glob, a brace) is marked dynamic,
// and the commands inside $(...), `...`, <(...) and (...) are read as
// commands of their own. What it can't read - an unterminated quote or
// parenthesis - is an error: the hook then asks, if janusctl is named.

// shWord is a word of a command, its quotes removed.
type shWord struct {
	text string
	// dynamic: its value isn't known here - an expansion, a glob, a
	// brace; split too when one of them is unquoted (the shell may make
	// several words of it).
	dynamic, split bool
}

// shRedir is a redirection: its operator and target - and a heredoc's
// body, once read.
type shRedir struct {
	op, target string
	body       *string
}

// shCommand is a simple command: its words, its redirections.
type shCommand struct {
	words  []shWord
	redirs []shRedir
	// piped: part of a pipeline with another command.
	piped bool
}

// shLine is what parseShell found: every simple command, nested ones
// included, in the order they appear.
type shLine struct {
	cmds []shCommand
	err  error
}

type shParser struct {
	s        string
	i        int
	out      *shLine
	heredocs []heredoc
	depth    int
}

type heredoc struct {
	delim       string
	stripTabs   bool
	expandsBody bool
	body        *string
}

const maxShellDepth = 16

// parseShell reads s.
func parseShell(s string) shLine {
	var line shLine
	p := &shParser{s: s, out: &line}
	p.list(0)
	return line
}

func (p *shParser) fail(msg string) {
	if p.out.err == nil {
		p.out.err = errors.New(msg)
	}
}

// sub reads s - the inside of a $(...) found as a string, a heredoc's
// expansions - into the same line.
func (p *shParser) sub(s string) {
	if p.depth >= maxShellDepth {
		p.fail("nested too deep")
		return
	}
	q := &shParser{s: s, out: p.out, depth: p.depth + 1}
	q.list(0)
}

// list reads commands up to end (0: the end of the input; ')' or '`'
// for a nested list) and past it.
func (p *shParser) list(end byte) {
	if p.depth >= maxShellDepth {
		p.fail("nested too deep")
		p.i = len(p.s)
		return
	}
	var cur shCommand
	piped := false
	flush := func(pipeNext bool) {
		if len(cur.words) > 0 || len(cur.redirs) > 0 {
			cur.piped = cur.piped || piped || pipeNext
			p.out.cmds = append(p.out.cmds, cur)
		}
		cur = shCommand{}
		piped = pipeNext
	}
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case end != 0 && c == end:
			p.i++
			flush(false)
			return
		case c == ' ' || c == '\t' || c == '\r':
			p.i++
		case c == '\\' && p.i+1 < len(p.s) && p.s[p.i+1] == '\n':
			p.i += 2
		case c == '\n':
			p.i++
			flush(false)
			p.readHeredocs()
		case c == '#' && (p.i == 0 || strings.IndexByte(" \t\n;&|()", p.s[p.i-1]) >= 0):
			for p.i < len(p.s) && p.s[p.i] != '\n' {
				p.i++
			}
		case c == ';':
			p.i++
			for p.i < len(p.s) && (p.s[p.i] == ';' || p.s[p.i] == '&') { // ;; ;& ;;& in a case
				p.i++
			}
			flush(false)
		case c == '|':
			p.i++
			switch {
			case p.i < len(p.s) && p.s[p.i] == '|':
				p.i++
				flush(false)
			default:
				if p.i < len(p.s) && p.s[p.i] == '&' { // |&
					p.i++
				}
				flush(true)
			}
		case c == '&' && !p.at("&>"):
			p.i++
			if p.i < len(p.s) && p.s[p.i] == '&' {
				p.i++
			}
			flush(false)
		case (c == '<' || c == '>') && p.i+1 < len(p.s) && p.s[p.i+1] == '(':
			// Process substitution: a command of its own, a word here.
			p.i += 2
			p.list(')')
			cur.words = append(cur.words, shWord{text: "/dev/fd/63", dynamic: true})
		case c == '(':
			p.i++
			flush(false)
			p.list(')')
		case c == ')':
			p.i++
			p.fail("a ) without its (")
		case p.redirStart():
			p.redirect(&cur)
		default:
			w, ok := p.word()
			if !ok {
				return
			}
			cur.words = append(cur.words, w)
		}
	}
	flush(false)
	if end != 0 {
		p.fail("unterminated " + map[byte]string{')': "(", '`': "`"}[end])
	}
}

func (p *shParser) at(prefix string) bool { return strings.HasPrefix(p.s[p.i:], prefix) }

// redirStart: a redirection starts here - [N]< [N]> &> and their kin.
func (p *shParser) redirStart() bool {
	j := p.i
	if p.at("&>") {
		return true
	}
	for j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
		j++
	}
	if j < len(p.s) && p.s[j] == '{' && j == p.i { // {fd}>file
		if k := strings.IndexByte(p.s[j:], '}'); k > 0 && j+k+1 < len(p.s) && (p.s[j+k+1] == '<' || p.s[j+k+1] == '>') {
			return true
		}
	}
	return j < len(p.s) && (p.s[j] == '<' || p.s[j] == '>')
}

func (p *shParser) redirect(cur *shCommand) {
	start := p.i
	if p.s[p.i] == '{' {
		p.i += strings.IndexByte(p.s[p.i:], '}') + 1
	}
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	ops := []string{"<<<", "<<-", "<<", "<>", "<&", "<", "&>>", "&>", ">>", ">&", ">|", ">"}
	op := ""
	for _, o := range ops {
		if p.at(o) {
			op = o
			break
		}
	}
	p.i += len(op)
	full := p.s[start:p.i]
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
	if p.i >= len(p.s) || strings.IndexByte("\n;&|()<>", p.s[p.i]) >= 0 {
		p.fail("a redirection without its target")
		return
	}
	if op == "<<" || op == "<<-" {
		rawStart := p.i
		w, ok := p.word()
		if !ok {
			return
		}
		raw := p.s[rawStart:p.i]
		body := new(string)
		p.heredocs = append(p.heredocs, heredoc{delim: w.text, stripTabs: op == "<<-", expandsBody: !strings.ContainsAny(raw, `'"\`), body: body})
		cur.redirs = append(cur.redirs, shRedir{op: full, target: w.text, body: body})
		return
	}
	w, ok := p.word()
	if !ok {
		return
	}
	cur.redirs = append(cur.redirs, shRedir{op: full, target: w.text})
}

// readHeredocs skips the bodies of the heredocs started on the line just
// ended - reading the commands an unquoted one's expansions run.
func (p *shParser) readHeredocs() {
	docs := p.heredocs
	p.heredocs = nil
	for _, h := range docs {
		var body strings.Builder
		for p.i < len(p.s) {
			nl := strings.IndexByte(p.s[p.i:], '\n')
			line := p.s[p.i:]
			if nl >= 0 {
				line = p.s[p.i : p.i+nl]
				p.i += nl + 1
			} else {
				p.i = len(p.s)
			}
			check := line
			if h.stripTabs {
				check = strings.TrimLeft(line, "\t")
			}
			if check == h.delim {
				break
			}
			body.WriteString(line)
			body.WriteByte('\n')
		}
		*h.body = body.String()
		if h.expandsBody {
			p.expansions(body.String())
		}
	}
}

// expansions reads the commands in s's $(...) and `...` - a heredoc's
// body, the inside of "..." already taken apart.
func (p *shParser) expansions(s string) {
	q := &shParser{s: s, out: p.out, depth: p.depth + 1}
	for q.i < len(q.s) {
		switch {
		case q.s[q.i] == '\\':
			q.i += 2
		case q.at("$(("):
			q.arith()
		case q.at("$("):
			q.i += 2
			q.depth++
			q.list(')')
			q.depth--
		case q.s[q.i] == '`':
			q.backtick()
		default:
			q.i++
		}
	}
}

// word reads one word, its quotes removed; false when the input ends
// inside it (an error then).
func (p *shParser) word() (shWord, bool) {
	var b strings.Builder
	var w shWord
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case strings.IndexByte(" \t\r\n;&|()<>", c) >= 0:
			if c == '<' || c == '>' || c == '(' {
				if p.i == start {
					p.i++ // a lone metacharacter: not a word, skip it
					p.fail("unexpected " + string(c))
				}
			}
			w.text = b.String()
			return w, true
		case c == '\\':
			if p.i+1 < len(p.s) {
				if p.s[p.i+1] != '\n' {
					b.WriteByte(p.s[p.i+1])
				}
				p.i += 2
			} else {
				p.i++
			}
		case c == '\'':
			end := strings.IndexByte(p.s[p.i+1:], '\'')
			if end < 0 {
				p.i = len(p.s)
				p.fail("unterminated '")
				return w, false
			}
			b.WriteString(p.s[p.i+1 : p.i+1+end])
			p.i += end + 2
		case c == '"':
			p.i++
			if !p.dquote(&b, &w) {
				return w, false
			}
		case c == '$' && p.at("$'"):
			p.i += 2
			s, ok := p.ansiC()
			if !ok {
				return w, false
			}
			b.WriteString(s)
		case c == '$' && p.at(`$"`):
			p.i += 2
			if !p.dquote(&b, &w) {
				return w, false
			}
		case c == '$':
			if !p.dollar(&b) {
				return w, false
			}
			w.dynamic, w.split = true, true
		case c == '`':
			b.WriteString("`...`")
			if !p.backtick() {
				return w, false
			}
			w.dynamic, w.split = true, true
		case c == '*' || c == '?' || c == '[':
			b.WriteByte(c)
			p.i++
			w.dynamic, w.split = true, true
		case c == '{' && (p.i > start || (p.i+1 < len(p.s) && strings.IndexByte(" \t\n;&|", p.s[p.i+1]) < 0)):
			b.WriteByte(c)
			p.i++
			w.dynamic, w.split = true, true // a brace expansion, maybe
		default:
			b.WriteByte(c)
			p.i++
		}
	}
	w.text = b.String()
	return w, true
}

// dquote reads the inside of "..." past its closing quote.
func (p *shParser) dquote(b *strings.Builder, w *shWord) bool {
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return true
		case c == '\\' && p.i+1 < len(p.s):
			n := p.s[p.i+1]
			switch n {
			case '$', '`', '"', '\\':
				b.WriteByte(n)
			case '\n':
			default:
				b.WriteByte('\\')
				b.WriteByte(n)
			}
			p.i += 2
		case c == '$' && p.i+1 < len(p.s) && (p.s[p.i+1] == '(' || p.s[p.i+1] == '{' || isNameStart(p.s[p.i+1]) || strings.IndexByte("@*#?$!-0123456789", p.s[p.i+1]) >= 0):
			if !p.dollar(b) {
				return false
			}
			w.dynamic = true
		case c == '`':
			b.WriteString("`...`")
			if !p.backtick() {
				return false
			}
			w.dynamic = true
		default:
			b.WriteByte(c)
			p.i++
		}
	}
	p.fail(`unterminated "`)
	return false
}

// dollar reads an expansion at $: $NAME, ${...}, $(...), $((...)),
// $1, $@...
func (p *shParser) dollar(b *strings.Builder) bool {
	start := p.i
	switch {
	case p.at("$(("):
		if !p.arith() {
			return false
		}
	case p.at("$("):
		p.i += 2
		p.list(')')
		if p.out.err != nil && p.i >= len(p.s) {
			return false
		}
	case p.at("${"):
		depth := 0
		j := p.i + 1
		for ; j < len(p.s); j++ {
			if p.s[j] == '{' {
				depth++
			} else if p.s[j] == '}' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if j >= len(p.s) {
			p.i = len(p.s)
			p.fail("unterminated ${")
			return false
		}
		inner := p.s[p.i+2 : j]
		p.i = j + 1
		if strings.Contains(inner, "$(") || strings.Contains(inner, "`") {
			p.expansions(inner)
		}
	case p.i+1 < len(p.s) && isNameStart(p.s[p.i+1]):
		p.i++
		for p.i < len(p.s) && isNameChar(p.s[p.i]) {
			p.i++
		}
	case p.i+1 < len(p.s) && strings.IndexByte("@*#?$!-0123456789", p.s[p.i+1]) >= 0:
		p.i += 2
	default:
		p.i++ // a lone $
		b.WriteByte('$')
		return true
	}
	b.WriteString(p.s[start:min(p.i, len(p.s))])
	return true
}

// arith skips $((...)), reading the commands its $(...) run.
func (p *shParser) arith() bool {
	depth := 0
	j := p.i + 1
	for ; j < len(p.s); j++ {
		if p.s[j] == '(' {
			depth++
		} else if p.s[j] == ')' {
			depth--
			if depth == 0 {
				break
			}
		}
	}
	if j >= len(p.s) {
		p.i = len(p.s)
		p.fail("unterminated $((")
		return false
	}
	inner := p.s[p.i+3 : max(p.i+3, j-1)]
	p.i = j + 1
	if strings.Contains(inner, "$(") || strings.Contains(inner, "`") {
		p.expansions(inner)
	}
	return true
}

// backtick reads `...` as a command line of its own.
func (p *shParser) backtick() bool {
	var inner strings.Builder
	j := p.i + 1
	for ; j < len(p.s); j++ {
		if p.s[j] == '\\' && j+1 < len(p.s) && strings.IndexByte("$`\\", p.s[j+1]) >= 0 {
			inner.WriteByte(p.s[j+1])
			j++
			continue
		}
		if p.s[j] == '`' {
			break
		}
		inner.WriteByte(p.s[j])
	}
	if j >= len(p.s) {
		p.i = len(p.s)
		p.fail("unterminated `")
		return false
	}
	p.i = j + 1
	p.sub(inner.String())
	return true
}

// ansiC reads the inside of $'...', its escapes decoded.
func (p *shParser) ansiC() (string, bool) {
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '\'':
			p.i++
			return b.String(), true
		case c == '\\' && p.i+1 < len(p.s):
			n := p.s[p.i+1]
			p.i += 2
			switch n {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 'e', 'E':
				b.WriteByte(0x1b)
			case 'f':
				b.WriteByte('\f')
			case 'v':
				b.WriteByte('\v')
			case 'x':
				b.WriteString(p.number(16, 2, 1))
			case 'u':
				b.WriteString(p.number(16, 4, 0))
			case 'U':
				b.WriteString(p.number(16, 8, 0))
			case 'c':
				if p.i < len(p.s) {
					b.WriteByte(p.s[p.i] & 0x1f)
					p.i++
				}
			case '0', '1', '2', '3', '4', '5', '6', '7':
				p.i--
				b.WriteString(p.number(8, 3, 1))
			default:
				if n != '\'' && n != '"' && n != '\\' && n != '?' {
					b.WriteByte('\\')
				}
				b.WriteByte(n)
			}
		default:
			b.WriteByte(c)
			p.i++
		}
	}
	p.fail("unterminated $'")
	return "", false
}

// number reads up to n digits in base: a byte (bytes 1), else a rune.
func (p *shParser) number(base, n, bytes int) string {
	j := p.i
	for j < len(p.s) && j-p.i < n && strings.IndexByte("0123456789abcdefABCDEF"[:min(base, 10)+max(0, base-10)*2], p.s[j]) >= 0 {
		j++
	}
	v, err := strconv.ParseUint(p.s[p.i:j], base, 32)
	p.i = j
	if err != nil {
		return ""
	}
	if bytes == 1 {
		return string([]byte{byte(v)})
	}
	return string(utf8.AppendRune(nil, rune(v)))
}

func isNameStart(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') }
func isNameChar(c byte) bool  { return isNameStart(c) || (c >= '0' && c <= '9') }
