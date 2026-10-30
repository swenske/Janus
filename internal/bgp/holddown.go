package bgp

import "strings"

// heldDown is what HoldDown adds at the end of a haproxy_* protocol's
// body: on the line of its closing brace, so BIRD's messages keep the
// operator's line numbers.
const heldDown = "disabled; /* held down by janusd */ "

// HoldDown returns config with its haproxy_* protocols disabled - the
// copy BIRD reads while the gate is closed: BIRD started or reconfigured
// with it has them down from the start - and the names of those it
// disabled. A "disabled" statement is added at the end of each one's
// body, after anything bird.conf says there. A protocol bird.conf itself
// disables in its body is left as it is: it isn't janusd's to hold.
// config is returned unchanged when it can't be read (an unterminated
// comment or string, unbalanced braces): BIRD refuses it anyway.
func HoldDown(config string) (string, []string) {
	toks, ok := lexBIRD(config)
	if !ok {
		return config, nil
	}
	var at []int // where to add heldDown: the closing braces
	var names []string
	depth := 0
	for k := 0; k < len(toks); k++ {
		switch t := toks[k]; {
		case t.is("{"):
			depth++
			continue
		case t.is("}"):
			if depth--; depth < 0 {
				return config, nil
			}
			continue
		case depth != 0 || !t.is("protocol"):
			continue
		}
		// protocol TYPE [NAME] [from TEMPLATE] { ... } - a TYPE may be
		// two words (ospf v3); "timeformat protocol ...;" is no protocol.
		name := ""
		j := k + 1
		for ; j < len(toks) && !toks[j].is("{") && !toks[j].is(";") && !toks[j].is("}"); j++ {
			if name == "" && !toks[j-1].is("from") && strings.HasPrefix(toks[j].text, GatePrefix) {
				name = toks[j].text
			}
		}
		if j == len(toks) || !toks[j].is("{") {
			continue
		}
		// Its body, to the matching brace; a "disabled" statement of its
		// own (not in a channel's block).
		end, own, d := -1, false, 0
	body:
		for b := j; b < len(toks); b++ {
			switch {
			case toks[b].is("{"):
				d++
			case toks[b].is("}"):
				if d--; d == 0 {
					end = b
					break body
				}
			case d == 1 && toks[b].is("disabled") && (toks[b-1].is("{") || toks[b-1].is(";") || toks[b-1].is("}")):
				own = disabledValue(toks, b+1)
			}
		}
		if end < 0 {
			return config, nil
		}
		k = end
		if name == "" || own {
			continue
		}
		at = append(at, toks[end].pos)
		names = append(names, name)
	}
	if depth != 0 || len(at) == 0 {
		return config, nil
	}
	var out strings.Builder
	prev := 0
	for _, p := range at {
		out.WriteString(config[prev:p])
		out.WriteString(heldDown)
		prev = p
	}
	out.WriteString(config[prev:])
	return out.String(), names
}

// disabledValue reads the switch after "disabled" (BIRD's "bool"): none,
// on/yes/true or a nonzero number disable; anything else (off/no/false,
// 0, an expression) is taken as not disabling.
func disabledValue(toks []token, i int) bool {
	if i >= len(toks) {
		return false
	}
	t := toks[i]
	switch {
	case t.is(";"), t.is("on"), t.is("yes"), t.is("true"):
		return true
	case !t.quoted && t.text != "" && t.text[0] >= '1' && t.text[0] <= '9' && isNumber(t.text) && i+1 < len(toks) && toks[i+1].is(";"):
		return true
	}
	return false
}

func isNumber(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// token is a word of bird.conf (a keyword, a symbol, a number - IP
// addresses come in pieces), an apostrophed symbol without its
// apostrophes, or one of { } ;.
type token struct {
	text   string
	pos    int // its offset in the text
	quoted bool
}

// is: t is that keyword or punctuation - an apostrophed symbol never is.
func (t token) is(s string) bool { return !t.quoted && t.text == s }

// lexBIRD splits bird.conf into tokens as BIRD 2's lexer (conf/cf-lex.l)
// sees them, as far as HoldDown needs: "#" comments to the end of the
// line, /* */ comments (not nested), "..." strings on one line, '...'
// symbols on one line. ok is false for an unterminated one.
func lexBIRD(s string) (toks []token, ok bool) {
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return nil, false
			}
			i += 2 + end + 2
		case c == '"' || c == '\'':
			end := strings.IndexAny(s[i+1:], string(c)+"\n")
			if end < 0 || s[i+1+end] != c {
				return nil, false
			}
			if c == '\'' {
				toks = append(toks, token{text: s[i+1 : i+1+end], pos: i, quoted: true})
			}
			i += 1 + end + 1
		case c == '{' || c == '}' || c == ';':
			toks = append(toks, token{text: s[i : i+1], pos: i})
			i++
		case isWordByte(c):
			j := i
			for j < len(s) && isWordByte(s[j]) {
				j++
			}
			toks = append(toks, token{text: s[i:j], pos: i})
			i = j
		default:
			i++
		}
	}
	return toks, true
}

func isWordByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
