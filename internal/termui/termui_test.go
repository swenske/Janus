package termui

import (
	"math"
	"strings"
	"testing"
)

func TestFrameTextClips(t *testing.T) {
	f := NewFrame(6, 2)
	if n := f.Text(4, 0, "abcdef", Style{}, 0); n != 2 {
		t.Errorf("wrote %d cells past the edge, want 2", n)
	}
	if n := f.Text(0, 1, "abcdef", Style{}, 3); n != 3 {
		t.Errorf("wrote %d cells with maxW 3", n)
	}
	f.Text(-1, 5, "off", Style{}, 0) // outside: nothing happens
	if lines := f.Lines(); lines[0] != "    ab" || lines[1] != "abc" {
		t.Errorf("lines = %q", lines)
	}
	f.Text(0, 0, "a\tb\x01", Style{}, 0)
	if f.At(1, 0).R != ' ' || f.At(3, 0).R != ' ' {
		t.Error("control characters must become spaces")
	}
	f = NewFrame(6, 2)
	f.TextRight(0, 5, 1, "right", Style{})
	if f.Lines()[1] != " right" {
		t.Errorf("TextRight: %q", f.Lines()[1])
	}
	f.TextRight(0, 5, 1, "far too long", Style{})
	if f.Lines()[1] != "far t…" {
		t.Errorf("TextRight clipped: %q", f.Lines()[1])
	}
}

func TestBoxRounded(t *testing.T) {
	f := NewFrame(10, 4)
	in := f.Box(Rect{0, 0, 10, 4}, "CPU", Style{}, Style{Bold: true})
	want := []string{"╭ CPU ────╮", "│        │", "│        │", "╰────────╯"}
	want[0] = "╭ CPU ───╮"
	for i, l := range f.Lines() {
		if l != want[i] {
			t.Errorf("row %d = %q, want %q", i, l, want[i])
		}
	}
	if in != (Rect{1, 1, 8, 2}) {
		t.Errorf("inner = %+v", in)
	}
	if !f.At(2, 0).S.Bold || f.At(0, 0).S.Bold {
		t.Error("the title is bold, the border isn't")
	}
	f = NewFrame(8, 3)
	f.Box(Rect{0, 0, 8, 3}, "a long title", Style{}, Style{})
	if f.Lines()[0] != "╭ a l… ╮" {
		t.Errorf("title cut: %q", f.Lines()[0])
	}
	if r := f.Box(Rect{0, 0, 1, 1}, "x", Style{}, Style{}); !r.Empty() {
		t.Error("a 1x1 box is nothing")
	}
}

func TestGraphBraille(t *testing.T) {
	f := NewFrame(2, 1)
	Graph(f, Rect{0, 0, 2, 1}, []float64{0, 0.5, 1}, 1, Style{})
	// 4 slots (left-padded with nothing): -, 0, 0.5, 1 -> dots 0, 0, 2, 4.
	if got := f.Lines()[0]; got != " ⣼" {
		t.Errorf("graph = %q, want %q", got, " ⣼")
	}
	f = NewFrame(1, 2)
	Graph(f, Rect{0, 0, 1, 2}, []float64{math.NaN(), 1}, 1, Style{})
	// Right slot full over two rows (8 dots), left empty.
	if lines := f.Lines(); lines[0] != "⢸" || lines[1] != "⢸" {
		t.Errorf("two rows = %q", lines)
	}
	f = NewFrame(1, 1)
	Graph(f, Rect{0, 0, 1, 1}, []float64{2, 4}, 0, Style{}) // autoscale: 4 is the top
	if got := f.Lines()[0]; got != "⣼" {
		t.Errorf("autoscaled = %q", got)
	}
	f = NewFrame(1, 1)
	Graph(f, Rect{0, 0, 1, 1}, []float64{0.01, 0}, 1, Style{}) // anything above zero shows one dot
	if got := f.Lines()[0]; got != "⡀" {
		t.Errorf("tiny value = %q", got)
	}
	Graph(f, Rect{}, []float64{1}, 1, Style{}) // an empty rect is fine
}

func TestSpark(t *testing.T) {
	// A block a value, eight heights, the newest on the right.
	if got := Spark([]float64{0, 0.25, 0.5, 1, math.NaN()}, 1); got != " ▂▄█ " {
		t.Errorf("spark = %q, want %q", got, " ▂▄█ ")
	}
	if got := Spark([]float64{2, 4}, 0); got != "▄█" {
		t.Errorf("autoscaled = %q", got)
	}
	if got := Spark([]float64{0.01, 3}, 1); got != "▁█" {
		t.Errorf("anything above zero shows, nothing goes past the top: %q", got)
	}
	if got := Spark(nil, 1); got != "" {
		t.Errorf("no values = %q", got)
	}
}

func TestMeter(t *testing.T) {
	f := NewFrame(10, 1)
	used, cached := Style{FG: ColorAccent}, Style{FG: ColorInfo}
	Meter(f, Rect{0, 0, 10, 1}, []float64{3, 2}, []Style{used, cached}, 10, Style{FG: ColorMuted})
	if got := f.Lines()[0]; got != "█████░░░░░" {
		t.Errorf("meter = %q", got)
	}
	if f.At(0, 0).S != used || f.At(3, 0).S != cached || f.At(5, 0).S.FG != ColorMuted {
		t.Error("each part keeps its style")
	}
	Meter(f, Rect{0, 0, 10, 1}, []float64{30}, []Style{used}, 10, Style{}) // over total: clipped
	if got := f.Lines()[0]; got != "██████████" {
		t.Errorf("over = %q", got)
	}
}

func TestDistribute(t *testing.T) {
	cases := []struct {
		total         int
		mins, weights []int
		want          []int
	}{
		{20, []int{4, 4}, []int{1, 1}, []int{10, 10}},
		{21, []int{4, 4}, []int{1, 1}, []int{11, 10}},
		{20, []int{4, 4, 4}, []int{2, 1, 1}, []int{8, 6, 6}},
		{7, []int{4, 4}, []int{1, 1}, []int{7, 0}},  // the last doesn't fit: dropped
		{3, []int{4, 4}, []int{1, 1}, []int{0, 0}},  // nothing fits
		{10, []int{2, 2}, []int{0, 0}, []int{8, 2}}, // no weights: the first grows
		{10, []int{2, 2}, []int{0, 1}, []int{2, 8}}, // only the second grows
		{10, []int{2, 2, 2}, []int{1, 1, 1}, []int{4, 3, 3}},
	}
	for _, c := range cases {
		got := Distribute(c.total, c.mins, c.weights)
		if strings.Trim(strings.Join(strings.Fields(itoaSlice(got)), " "), "[]") != strings.Trim(strings.Join(strings.Fields(itoaSlice(c.want)), " "), "[]") {
			t.Errorf("Distribute(%d, %v, %v) = %v, want %v", c.total, c.mins, c.weights, got, c.want)
		}
		sum := 0
		for _, v := range got {
			sum += v
		}
		if sum > c.total {
			t.Errorf("Distribute(%d, %v, %v) gives %d rows", c.total, c.mins, c.weights, sum)
		}
	}
}

func itoaSlice(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = itoa(n)
	}
	return strings.Join(parts, " ")
}

func TestTableScrollAndDraw(t *testing.T) {
	tb := &Table{Columns: []Column{{Title: "PID", Width: 5, Right: true}, {Title: "COMMAND"}}}
	for i := 0; i < 10; i++ {
		tb.Rows = append(tb.Rows, []string{itoa(i), "cmd" + itoa(i) + " with a long command line"})
	}
	f := NewFrame(20, 4) // header + 3 rows
	r := Rect{0, 0, 20, 4}
	tb.Draw(f, r, true, Style{Bold: true}, Style{Reverse: true})
	lines := f.Lines()
	if lines[0] != "  PID COMMAND" || lines[1] != "    0 cmd0 with a l…" {
		t.Errorf("header/row = %q", lines[:2])
	}
	if !f.At(0, 1).S.Reverse || f.At(0, 2).S.Reverse {
		t.Error("only the cursor's row is reversed")
	}
	page := tb.Visible(r)
	for i := 0; i < 5; i++ {
		tb.Key(KeyDown, page)
	}
	if tb.Cursor != 5 || tb.Offset != 3 {
		t.Errorf("after 5 downs: cursor %d offset %d", tb.Cursor, tb.Offset)
	}
	tb.Key(KeyEnd, page)
	if tb.Cursor != 9 || tb.Offset != 7 {
		t.Errorf("End: cursor %d offset %d", tb.Cursor, tb.Offset)
	}
	tb.Key(KeyPgUp, page)
	if tb.Cursor != 6 {
		t.Errorf("PgUp: cursor %d", tb.Cursor)
	}
	tb.Key("\x1bOH", page) // application-mode Home
	if tb.Cursor != 0 || tb.Offset != 0 {
		t.Errorf("Home: cursor %d offset %d", tb.Cursor, tb.Offset)
	}
	if tb.Key("x", page) {
		t.Error("x isn't a table key")
	}
	tb.Rows = tb.Rows[:2]
	tb.Cursor = 9
	tb.Clamp(page)
	if tb.Cursor != 1 {
		t.Errorf("cursor after the rows shrank = %d", tb.Cursor)
	}
	f = NewFrame(20, 4)
	tb.Draw(f, r, false, Style{}, Style{Reverse: true})
	if f.At(0, 1).S.Reverse {
		t.Error("no cursor when not focused")
	}
	// Tone colours a cell; a heading row spans the table.
	tb.Rows = [][]string{{"web (backend)"}, {"1", "srv1"}}
	tb.Heading = func(i int) bool { return i == 0 }
	tb.Tone = func(i, c int) Style {
		if i == 1 && c == 1 {
			return Style{FG: ColorOK}
		}
		return Style{}
	}
	f = NewFrame(20, 4)
	tb.Draw(f, r, false, Style{}, Style{})
	if l := f.Lines(); l[1] != "web (backend)" || l[2] != "    1 srv1" {
		t.Errorf("heading/row = %q", l[1:3])
	}
	if !f.At(0, 1).S.Bold || f.At(6, 2).S.FG != ColorOK || f.At(4, 2).S.FG != ColorDefault {
		t.Error("heading bold, toned cell only")
	}
}

func TestTableDropsColumnsWhenNarrow(t *testing.T) {
	tb := &Table{Columns: []Column{{Title: "NAME"}, {Title: "STATUS", Width: 8}, {Title: "WGT", Width: 4}, {Title: "CHECK", Width: 8}}, Rows: [][]string{{"web1", "UP", "1", "L4OK"}}}
	f := NewFrame(20, 2)
	tb.Draw(f, Rect{0, 0, 20, 2}, false, Style{}, Style{})
	// 20 cells: NAME would get 20-8-4-8-3 = -3; without CHECK, 20-8-4-2 = 6 < 8; without WGT, 20-8-1 = 11.
	if l := f.Lines(); l[0] != "NAME        STATUS" || l[1] != "web1        UP" {
		t.Errorf("narrow table = %q", l)
	}
	f = NewFrame(40, 2)
	tb.Draw(f, Rect{0, 0, 40, 2}, false, Style{}, Style{})
	if l := f.Lines(); !strings.HasSuffix(l[0], "CHECK") {
		t.Errorf("wide table = %q", l)
	}
}

func TestRenderDiff(t *testing.T) {
	p := Palette{Depth16}
	a := NewFrame(3, 2)
	a.Text(0, 0, "ab", Style{FG: ColorOK}, 0)
	a.Text(0, 1, "cd", Style{}, 0)
	full := a.Render(nil, p)
	want := "\x1b[1;1H\x1b[0m\x1b[32mab\x1b[0m \x1b[0m\x1b[2;1Hcd \x1b[0m"
	if full != want {
		t.Errorf("full render = %q\n            want %q", full, want)
	}
	b := NewFrame(3, 2)
	b.Text(0, 0, "ab", Style{FG: ColorOK}, 0)
	b.Text(0, 1, "cx", Style{Bold: true}, 0)
	diff := b.Render(a, p)
	if strings.Contains(diff, "\x1b[1;1H") || !strings.Contains(diff, "\x1b[2;1H") {
		t.Errorf("only row 2 changed: %q", diff)
	}
	if b.Render(b, p) != "" {
		t.Error("an identical frame renders nothing")
	}
	if c := NewFrame(4, 2); !strings.Contains(c.Render(b, p), "\x1b[1;1H") {
		t.Error("another size redraws everything")
	}
	if got := a.Render(nil, Palette{DepthNone}); strings.Contains(got, "\x1b[32m") || strings.Contains(got, "[1m") {
		t.Errorf("no colour on a plain terminal: %q", got)
	}
}

func TestDetectPalette(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		env  map[string]string
		want Depth
	}{
		{map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor", "NO_COLOR": "1"}, DepthNone},
		{map[string]string{"TERM": "dumb", "COLORTERM": "truecolor"}, DepthNone},
		{map[string]string{"TERM": "xterm", "COLORTERM": "24bit"}, DepthTrue},
		{map[string]string{"TERM": "xterm-256color"}, Depth256},
		{map[string]string{"TERM": "screen.xterm-256color"}, Depth256},
		{map[string]string{"TERM": "xterm"}, Depth16},
		{map[string]string{}, Depth16},
	}
	for _, c := range cases {
		if got := DetectPalette(env(c.env)); got.Depth != c.want {
			t.Errorf("DetectPalette(%v) = %v, want %v", c.env, got.Depth, c.want)
		}
	}
	if s := (Palette{DepthTrue}).SGR(Style{FG: ColorAccent, Bold: true}); s != "\x1b[1;38;2;216;100;60m" {
		t.Errorf("true colour accent = %q", s)
	}
	if s := (Palette{Depth256}).SGR(Style{FG: ColorAccent}); s != "\x1b[38;5;166m" {
		t.Errorf("256 accent = %q", s)
	}
	if s := (Palette{Depth16}).SGR(Style{}); s != "" {
		t.Errorf("plain style = %q", s)
	}
}

func TestSplitKeys(t *testing.T) {
	got := SplitKeys("a\x1b[Bé\x1b[1;5C\x1bOA\x1b\r")
	want := []string{"a", "\x1b[B", "é", "\x1b[1;5C", "\x1bOA", "\x1b", "\r"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("SplitKeys = %q, want %q", got, want)
	}
	if Normalize("\x1bOA") != KeyUp || Normalize("\x1b[4~") != KeyEnd || Normalize("\n") != KeyEnter || Normalize("q") != "q" {
		t.Error("Normalize")
	}
}

func TestTruncatePad(t *testing.T) {
	if Truncate("héllo", 3) != "hé…" || Truncate("hi", 3) != "hi" || Truncate("hi", 1) != "…" || Truncate("hi", 0) != "" {
		t.Error("Truncate")
	}
	if Pad("ab", 4) != "ab  " || PadLeft("ab", 4) != "  ab" || Pad("abcde", 4) != "abc…" {
		t.Error("Pad")
	}
}
