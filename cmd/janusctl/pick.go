package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"
)

// The interactive picker: a list filtered as you type (fuzzy, like
// fzf), on the terminal itself (/dev/tty) - so it works with stdout
// piped. Used when janusctl runs alone in a terminal (the command
// palette), for -n ?, and for an argument left out that janusctl can
// list (pickers.go). Never when stdin or stderr isn't a terminal:
// scripts get the usage error they always got.

// errCancelled is Esc or Ctrl-C in a picker.
var errCancelled = errors.New("cancelled")

// maxRows is how many candidates a picker shows at once.
const maxRows = 10

// match is a candidate the query matches, its matched runes' indexes.
type match struct {
	c       candidate
	score   int
	matched []int
}

// fuzzyMatch reports whether every rune of query appears in text in
// order (case-insensitively), scoring runs, word starts and an early
// first match higher.
func fuzzyMatch(query, text string) (int, []int, bool) {
	if query == "" {
		return 0, nil, true
	}
	q := []rune(strings.ToLower(query))
	t := []rune(text)
	lower := []rune(strings.ToLower(text))
	var idx []int
	score, qi, prev := 0, 0, -2
	for i := 0; i < len(t) && qi < len(q); i++ {
		if lower[i] != q[qi] {
			continue
		}
		s := 1
		if i == prev+1 {
			s += 4 // a run
		}
		if i == 0 || !unicode.IsLetter(t[i-1]) && !unicode.IsDigit(t[i-1]) {
			s += 3 // a word's start
		}
		if qi == 0 {
			s -= min(i, 3) // matched late
		}
		score += s
		idx = append(idx, i)
		prev = i
		qi++
	}
	if qi < len(q) {
		return 0, nil, false
	}
	// The query as it was typed, whole, beats its letters apart.
	if at := strings.Index(string(lower), string(q)); at >= 0 {
		score += 10
		if at == 0 || !unicode.IsLetter(t[at-1]) && !unicode.IsDigit(t[at-1]) {
			score += 5
		}
	}
	return score, idx, true
}

// picker is the state of one: what it lists, what's typed, where the
// cursor is - kept apart from the terminal so it's tested alone.
type picker struct {
	title    string
	items    []candidate
	multi    bool
	query    []rune
	cursor   int
	offset   int
	matches  []match
	selected map[string]bool
}

func newPicker(title string, items []candidate, multi bool) *picker {
	p := &picker{title: title, items: items, multi: multi, selected: map[string]bool{}}
	p.filter()
	return p
}

func (p *picker) filter() {
	q := string(p.query)
	p.matches = p.matches[:0]
	for _, c := range p.items {
		// The value itself first; what it is only as a last resort.
		if s, idx, ok := fuzzyMatch(q, c.value); ok {
			p.matches = append(p.matches, match{c, s + 20, idx})
		} else if s, _, ok := fuzzyMatch(q, c.value+"  "+c.help); ok && c.help != "" {
			p.matches = append(p.matches, match{c, s, nil})
		}
	}
	if q != "" {
		slices.SortStableFunc(p.matches, func(a, b match) int { return b.score - a.score })
	}
	p.cursor, p.offset = 0, 0
}

// key handles one key: done with the choice made, or cancelled.
func (p *picker) key(k string) (done, cancelled bool) {
	switch k {
	case "\x03", "\x1b": // Ctrl-C, Esc
		return false, true
	case "\r", "\n":
		if p.multi && len(p.selected) > 0 {
			return true, false
		}
		if len(p.matches) == 0 {
			return false, false
		}
		if p.multi {
			p.selected[p.matches[p.cursor].c.value] = true
		}
		return true, false
	case "\x1b[A", "\x1bOA", "\x10": // up, Ctrl-P
		if p.cursor > 0 {
			p.cursor--
		} else if len(p.matches) > 0 {
			p.cursor = len(p.matches) - 1
		}
	case "\x1b[B", "\x1bOB", "\x0e": // down, Ctrl-N
		if p.cursor+1 < len(p.matches) {
			p.cursor++
		} else {
			p.cursor = 0
		}
	case "\t":
		if p.multi && len(p.matches) > 0 {
			v := p.matches[p.cursor].c.value
			if p.selected[v] {
				delete(p.selected, v)
			} else {
				p.selected[v] = true
			}
			if p.cursor+1 < len(p.matches) {
				p.cursor++
			}
		}
	case "\x7f", "\b": // Backspace
		if len(p.query) > 0 {
			p.query = p.query[:len(p.query)-1]
			p.filter()
		}
	case "\x15": // Ctrl-U
		p.query = nil
		p.filter()
	default:
		r, size := utf8.DecodeRuneInString(k)
		if size == len(k) && unicode.IsPrint(r) {
			p.query = append(p.query, r)
			p.filter()
		}
	}
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+maxRows {
		p.offset = p.cursor - maxRows + 1
	}
	return false, false
}

// choice is what was picked.
func (p *picker) choice() []string {
	if p.multi {
		var out []string
		for _, c := range p.items { // in the list's order
			if p.selected[c.value] {
				out = append(out, c.value)
			}
		}
		return out
	}
	return []string{p.matches[p.cursor].c.value}
}

// render is the picker's lines, at most width runes each.
func (p *picker) render(st style, width int) []string {
	hint := "type to filter, ↑↓, Enter"
	if p.multi {
		hint = "type to filter, ↑↓, Tab to mark, Enter"
	}
	lines := []string{st.cyan("?") + " " + st.bold(p.title) + " " + st.dim("("+hint+")") + " › " + string(p.query)}
	if len(p.matches) == 0 {
		return append(lines, st.dim("  nothing matches"))
	}
	valueWidth := 0
	for _, m := range p.matches {
		valueWidth = max(valueWidth, utf8.RuneCountInString(m.c.value))
	}
	valueWidth = min(valueWidth, max(width/2, 20))
	end := min(p.offset+maxRows, len(p.matches))
	for i := p.offset; i < end; i++ {
		m := p.matches[i]
		mark := "  "
		if i == p.cursor {
			mark = st.cyan("❯ ")
		}
		box := ""
		if p.multi {
			box = "◯ "
			if p.selected[m.c.value] {
				box = st.green("◉ ")
			}
		}
		value := highlight(st, m.c.value, m.matched, i == p.cursor)
		pad := strings.Repeat(" ", max(0, valueWidth-utf8.RuneCountInString(m.c.value)))
		line := mark + box + value + pad
		if m.c.help != "" {
			room := width - 4 - valueWidth - len([]rune(box))
			help := []rune(m.c.help)
			if room > 3 {
				if len(help) > room {
					help = append(help[:room-1], '…')
				}
				line += "  " + st.dim(string(help))
			}
		}
		lines = append(lines, line)
	}
	if len(p.matches) > maxRows {
		lines = append(lines, st.dim(fmt.Sprintf("  %d/%d", p.cursor+1, len(p.matches))))
	}
	return lines
}

// highlight bolds value's matched runes (those of the value itself).
func highlight(st style, value string, matched []int, current bool) string {
	runes := []rune(value)
	var b strings.Builder
	for i, r := range runes {
		s := string(r)
		switch {
		case slices.Contains(matched, i):
			s = st.wrap("1;36", s)
		case current:
			s = st.bold(s)
		}
		b.WriteString(s)
	}
	return b.String()
}

// canPick reports whether janusctl may ask: both its input and its
// messages on a terminal, and JANUS_NO_PICKER unset.
func canPick() bool {
	return isTerminal(os.Stdin) && isTerminal(os.Stderr) && os.Getenv("JANUS_NO_PICKER") == ""
}

// pick asks on the terminal for one of items (several with multi) -
// echo: then says what was picked.
func pick(title string, items []candidate, multi, echo bool) ([]string, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("%s: nothing to choose from", title)
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer tty.Close()
	state, err := term.MakeRaw(int(tty.Fd()))
	if err != nil {
		return nil, err
	}
	defer term.Restore(int(tty.Fd()), state) //nolint:errcheck // the terminal goes back as it can
	p := newPicker(title, items, multi)
	st := style{on: os.Getenv("NO_COLOR") == ""}
	drawn := 0
	draw := func() {
		width := 80
		if w, _, err := term.GetSize(int(tty.Fd())); err == nil && w > 10 {
			width = w
		}
		var b strings.Builder
		if drawn > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", drawn-1)
		}
		b.WriteString("\r\x1b[J\x1b[?25l")
		lines := p.render(st, width)
		b.WriteString(strings.Join(lines, "\r\n"))
		drawn = len(lines)
		_, _ = io.WriteString(tty, b.String())
	}
	clear := func() {
		if drawn > 0 {
			fmt.Fprintf(tty, "\x1b[%dA", drawn-1)
		}
		_, _ = io.WriteString(tty, "\r\x1b[J\x1b[?25h")
	}
	draw()
	buf := make([]byte, 64)
	for {
		n, err := tty.Read(buf)
		if err != nil {
			clear()
			return nil, err
		}
		for _, k := range splitKeys(string(buf[:n])) {
			done, cancelled := p.key(k)
			if cancelled {
				clear()
				return nil, errCancelled
			}
			if done {
				clear()
				got := p.choice()
				if echo {
					fmt.Fprintf(tty, "%s %s %s\r\n", st.green("✔"), st.bold(title), st.dim("· "+strings.Join(got, ", ")))
				}
				return got, nil
			}
		}
		draw()
	}
}

// splitKeys cuts what one read gave into keys: escape sequences whole,
// a rune each otherwise (a paste gives many).
func splitKeys(s string) []string {
	var out []string
	for s != "" {
		if s[0] == 0x1b && len(s) >= 3 && (s[1] == '[' || s[1] == 'O') {
			end := 2
			for end < len(s) && (s[end] < 0x40 || s[end] > 0x7e) {
				end++
			}
			if end < len(s) {
				end++
			}
			out = append(out, s[:end])
			s = s[end:]
			continue
		}
		_, size := utf8.DecodeRuneInString(s)
		out = append(out, s[:size])
		s = s[size:]
	}
	return out
}
