// Package termui draws a full-screen terminal page without any terminal
// library: a Frame of cells the screens paint (text, boxes, graphs,
// tables), rendered to the escape sequences a terminal understands,
// only the rows that changed. The model never sees an escape code, so a
// screen can be tested as plain text (Frame.Lines). Glyphs are single
// width: box drawing, braille and blocks; East-Asian width isn't
// computed.
package termui

import (
	"strings"
	"unicode/utf8"
)

// Rect is an area of the frame.
type Rect struct{ X, Y, W, H int }

// Inner is r without its border.
func (r Rect) Inner() Rect { return Rect{r.X + 1, r.Y + 1, max(r.W-2, 0), max(r.H-2, 0)} }

// Empty reports whether r has no cell.
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Cell is one character of the frame with its style.
type Cell struct {
	R rune
	S Style
}

// Frame is a page of W×H cells, blank until painted.
type Frame struct {
	W, H  int
	Graph GraphSymbols // what Graph draws with
	cells []Cell
}

// NewFrame is a blank frame.
func NewFrame(w, h int) *Frame {
	w, h = max(w, 0), max(h, 0)
	f := &Frame{W: w, H: h, cells: make([]Cell, w*h)}
	for i := range f.cells {
		f.cells[i].R = ' '
	}
	return f
}

// Set paints one cell; outside the frame it does nothing.
func (f *Frame) Set(x, y int, r rune, s Style) {
	if x < 0 || y < 0 || x >= f.W || y >= f.H {
		return
	}
	f.cells[y*f.W+x] = Cell{r, s}
}

// At is the cell at x, y (a blank one outside the frame).
func (f *Frame) At(x, y int) Cell {
	if x < 0 || y < 0 || x >= f.W || y >= f.H {
		return Cell{R: ' '}
	}
	return f.cells[y*f.W+x]
}

// Text writes s from x on row y, clipped to the frame and to maxW cells
// (maxW <= 0: to the frame's edge), and returns the cells used. Control
// characters become spaces.
func (f *Frame) Text(x, y int, s string, st Style, maxW int) int {
	if maxW <= 0 || x+maxW > f.W {
		maxW = f.W - x
	}
	n := 0
	for _, r := range s {
		if n >= maxW {
			break
		}
		if r < ' ' || r == 0x7f {
			r = ' '
		}
		f.Set(x+n, y, r, st)
		n++
	}
	return max(n, 0)
}

// TextRight writes s ending at column x2 (inclusive), clipped on the
// left to x1.
func (f *Frame) TextRight(x1, x2, y int, s string, st Style) int {
	w := utf8.RuneCountInString(s)
	if room := x2 - x1 + 1; w > room {
		s = Truncate(s, room)
		w = utf8.RuneCountInString(s)
	}
	return f.Text(x2-w+1, y, s, st, w)
}

// Fill paints r with ch.
func (f *Frame) Fill(r Rect, ch rune, st Style) {
	for y := r.Y; y < r.Y+r.H; y++ {
		for x := r.X; x < r.X+r.W; x++ {
			f.Set(x, y, ch, st)
		}
	}
}

// Box draws a rounded border around r with title in its top edge, and
// returns the inside. A box narrower than 2 or shorter than 2 draws
// nothing.
func (f *Frame) Box(r Rect, title string, border, titleSt Style) Rect {
	if r.W < 2 || r.H < 2 {
		return Rect{}
	}
	x2, y2 := r.X+r.W-1, r.Y+r.H-1
	for x := r.X + 1; x < x2; x++ {
		f.Set(x, r.Y, '─', border)
		f.Set(x, y2, '─', border)
	}
	for y := r.Y + 1; y < y2; y++ {
		f.Set(r.X, y, '│', border)
		f.Set(x2, y, '│', border)
	}
	f.Set(r.X, r.Y, '╭', border)
	f.Set(x2, r.Y, '╮', border)
	f.Set(r.X, y2, '╰', border)
	f.Set(x2, y2, '╯', border)
	if title != "" && r.W > 4 {
		t := " " + Truncate(title, r.W-4) + " "
		f.Text(r.X+1, r.Y, t, titleSt, r.W-2)
	}
	return r.Inner()
}

// Lines is the frame as plain text, one string per row, trailing spaces
// trimmed: what -once prints and what tests compare.
func (f *Frame) Lines() []string {
	out := make([]string, f.H)
	var b strings.Builder
	for y := 0; y < f.H; y++ {
		b.Reset()
		for x := 0; x < f.W; x++ {
			b.WriteRune(f.cells[y*f.W+x].R)
		}
		out[y] = strings.TrimRight(b.String(), " ")
	}
	return out
}

// Render is the escape sequence that makes the terminal show f: every
// row when prev is nil or another size, else only the rows that differ.
// Rows are addressed absolutely, so nothing ever scrolls; each row ends
// with the attributes reset. A change of palette needs prev nil: the
// cells are the same, what they look like isn't.
func (f *Frame) Render(prev *Frame, p Palette) string {
	var b strings.Builder
	sgr := map[Style]string{}
	codes := func(s Style) string {
		c, ok := sgr[s]
		if !ok {
			c = p.SGR(s)
			sgr[s] = c
		}
		return c
	}
	glyph := p.glyphs()
	for y := 0; y < f.H; y++ {
		if prev != nil && prev.W == f.W && prev.H == f.H && rowsEqual(f.cells[y*f.W:(y+1)*f.W], prev.cells[y*f.W:(y+1)*f.W]) {
			continue
		}
		b.WriteString("\x1b[")
		b.WriteString(itoa(y + 1))
		b.WriteString(";1H")
		cur := ""
		for x := 0; x < f.W; x++ {
			c := f.cells[y*f.W+x]
			if s := codes(c.S); s != cur {
				b.WriteString("\x1b[0m")
				b.WriteString(s)
				cur = s
			}
			b.WriteRune(glyph(c.R))
		}
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// glyphs is how the palette's terminal draws a rune: square corners
// instead of rounded ones, and on the Linux console what its font (CP437)
// has instead of what it hasn't.
func (p Palette) glyphs() func(rune) rune {
	if !p.Square && !p.Console {
		return func(r rune) rune { return r }
	}
	return func(r rune) rune {
		switch r {
		case '╭':
			return '┌'
		case '╮':
			return '┐'
		case '╰':
			return '└'
		case '╯':
			return '┘'
		}
		if !p.Console {
			return r
		}
		if c, ok := consoleGlyphs[r]; ok {
			return c
		}
		return r
	}
}

// consoleGlyphs are the glyphs the dashboard draws that the Linux
// console's font lacks, and what it draws instead. Graphs and
// sparklines choose their own (GraphTTY).
var consoleGlyphs = map[rune]rune{
	'…': '~', '▸': '►', '●': '•',
}

func rowsEqual(a, b []Cell) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Truncate cuts s to w cells, the last one an ellipsis when something
// was cut.
func Truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	rs := []rune(s)
	if w == 1 {
		return "…"
	}
	return string(rs[:w-1]) + "…"
}

// Pad is s padded with spaces to w cells (or cut to w).
func Pad(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return Truncate(s, w)
	}
	return s + strings.Repeat(" ", w-n)
}

// PadLeft is s right-aligned in w cells.
func PadLeft(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return Truncate(s, w)
	}
	return strings.Repeat(" ", w-n) + s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
