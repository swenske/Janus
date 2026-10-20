package haproxy

import (
	"bytes"
	"regexp"
	"sort"
	"strings"

	"github.com/swenske/Janus/internal/haproxyflow"
)

// Redacted stands in for a secret RedactConfig took out.
const Redacted = "<redacted>"

// credentialHeaders are the HTTP headers whose value is a credential
// (lower case: header names aren't case-sensitive).
var credentialHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true, "set-cookie": true,
	"x-api-key": true, "x-auth-token": true,
}

var (
	// headerFetch is a sample fetch reading a credential header -
	// hdr(x-api-key), req.hdr(Authorization), req.fhdr(cookie),lower,
	// hdr_beg(...) - whose ACL patterns are then the credentials it's
	// compared with.
	headerFetch = regexp.MustCompile(`(?i)^(?:req\.|res\.)?f?hdr(?:_[a-z]+)?\(\s*(?:authorization|proxy-authorization|cookie|set-cookie|x-api-key|x-auth-token)\s*[,)]`)
	// inlineHeader is a credential header written into one word, as in
	// "option httpchk GET / HTTP/1.0\r\nAuthorization:\ Basic\ ...".
	inlineHeader = regexp.MustCompile(`(?i)(?:authorization|proxy-authorization|cookie|set-cookie|x-api-key|x-auth-token):(?:\\ | )*`)
)

// RedactConfig is cfg with the secrets someone who may only read a
// node mustn't see replaced by Redacted (JANUS-2026-006): userlist
// passwords (hashed ones too - they can be cracked offline), "stats
// auth" passwords, setenv/presetenv and acme-vars values, the values of
// credential headers HAProxy sets or sends to a health check, and the
// patterns an ACL compares such a header with. Every line keeps its
// place - the line numbers haproxy -c cites still point at the same
// line - and everything else is left byte for byte. Best effort: a
// secret written anywhere else (a comment, a map file, a Lua string)
// stays.
func RedactConfig(cfg []byte) []byte {
	var b bytes.Buffer
	b.Grow(len(cfg))
	for _, line := range strings.SplitAfter(string(cfg), "\n") {
		body := strings.TrimSuffix(line, "\n")
		b.WriteString(redactLine(body))
		b.WriteString(line[len(body):])
	}
	return b.Bytes()
}

// cut is a part of a line to replace.
type cut struct {
	start, end int
	with       string
}

func redactLine(line string) string {
	ws := haproxyflow.Words(line)
	if len(ws) == 0 {
		return line
	}
	var cuts []cut
	whole := func(w haproxyflow.Word) { cuts = append(cuts, cut{w.Start, w.End, Redacted}) }
	word := func(i int) string {
		if i < len(ws) {
			return ws[i].Text
		}
		return ""
	}

	switch word(0) {
	case "user": // user <name> [password|insecure-password <password>] [groups ...]
		for i := 2; i+1 < len(ws); i++ {
			if ws[i].Text == "password" || ws[i].Text == "insecure-password" {
				whole(ws[i+1])
				i++
			}
		}
	case "stats": // stats auth <user>:<password>
		if word(1) == "auth" && len(ws) > 2 {
			cuts = append(cuts, afterColon(line, ws[2]))
		}
	case "setenv", "presetenv": // setenv <name> <value>
		if len(ws) > 2 {
			whole(ws[2])
		}
	case "acme-vars": // acme-vars "<name>=<value>[,...]": a DNS provider's credentials
		for _, w := range ws[1:] {
			whole(w)
		}
	case "http-request", "http-response", "http-after-response":
		// set-header|add-header <name> <value>, replace-header|replace-value <name> <regex> <value>
		if credentialHeaders[strings.ToLower(word(2))] {
			switch word(1) {
			case "set-header", "add-header":
				if len(ws) > 3 {
					whole(ws[3])
				}
			case "replace-header", "replace-value":
				if len(ws) > 4 {
					whole(ws[4])
				}
			}
		}
	case "http-check": // http-check send ... hdr <name> <value> ...
		if word(1) == "send" {
			for i := 2; i+2 < len(ws); i++ {
				if ws[i].Text == "hdr" && credentialHeaders[strings.ToLower(ws[i+1].Text)] {
					whole(ws[i+2])
					i += 2
				}
			}
		}
	}

	// An ACL comparing a credential header with patterns: "acl <name>
	// <fetch> [flags] <pattern>..." or an anonymous one, "{ <fetch>
	// [flags] <pattern>... }".
	for i, w := range ws {
		inACL := i == 2 && word(0) == "acl" || i > 0 && ws[i-1].Text == "{"
		if !inACL || !headerFetch.MatchString(w.Text) {
			continue
		}
		flags := true
		for j := i + 1; j < len(ws) && ws[j].Text != "}"; j++ {
			t := ws[j].Text
			if flags && t == "--" {
				flags = false
				continue
			}
			if flags && len(t) > 1 && t[0] == '-' {
				if t == "-f" || t == "-m" || t == "-M" || t == "-u" {
					j++ // its argument: a file, a match method, an ID
				}
				continue
			}
			flags = false
			whole(ws[j])
		}
	}

	// A credential header inside a word: its value, up to the next
	// escaped CR or the word's end.
	for _, w := range ws {
		raw := line[w.Start:w.End]
		if strings.IndexByte(raw, ':') < 0 {
			continue // inlineHeader needs the header's colon: most words
		}
		for _, m := range inlineHeader.FindAllStringIndex(raw, -1) {
			end := len(raw)
			if k := strings.Index(raw[m[1]:], `\r`); k >= 0 {
				end = m[1] + k
			} else if q := raw[end-1]; (q == '"' || q == '\'') && end-1 > m[1] {
				end--
			}
			if end > m[1] {
				cuts = append(cuts, cut{w.Start + m[1], w.Start + end, Redacted})
			}
		}
	}

	if len(cuts) == 0 {
		return line
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].start < cuts[j].start })
	var b strings.Builder
	at := 0
	for _, c := range cuts {
		if c.start < at { // inside a part already replaced
			continue
		}
		b.WriteString(line[at:c.start])
		b.WriteString(c.with)
		at = c.end
	}
	b.WriteString(line[at:])
	return b.String()
}

// afterColon replaces what follows the first colon of w - the password
// of "<user>:<password>" - keeping a closing quote; all of w when it
// has no colon, nothing when the password is empty.
func afterColon(line string, w haproxyflow.Word) cut {
	raw := line[w.Start:w.End]
	i := strings.IndexByte(raw, ':')
	if i < 0 {
		return cut{w.Start, w.End, Redacted}
	}
	end := len(raw)
	if q := raw[0]; (q == '"' || q == '\'') && end > i+1 && raw[end-1] == q {
		end--
	}
	if end == i+1 {
		return cut{w.End, w.End, ""}
	}
	return cut{w.Start + i + 1, w.Start + end, Redacted}
}
