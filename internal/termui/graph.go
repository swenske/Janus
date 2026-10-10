package termui

import "math"

// Braille dots, bottom to top, for the left and right column of a cell.
var (
	brailleLeft  = [4]rune{0x40, 0x04, 0x02, 0x01}
	brailleRight = [4]rune{0x80, 0x20, 0x10, 0x08}
)

// GraphSymbols are the glyphs graphs and sparklines are drawn with.
type GraphSymbols uint8

const (
	GraphBraille GraphSymbols = iota // two values a cell, four heights a row
	GraphBlock                       // a value a cell, eight heights a row (▁ to █)
	GraphTTY                         // a value a cell, two heights a row (▄ █): the Linux console's font
)

// graphGlyphs is, per symbol set, how many values a cell holds and
// how many heights a row; braille draws its own.
var graphGlyphs = map[GraphSymbols]struct {
	perCell, heights int
	glyphs           []rune
}{
	GraphBraille: {2, 4, nil},
	GraphBlock:   {1, 8, []rune(" ▁▂▃▄▅▆▇█")},
	GraphTTY:     {1, 2, []rune(" ▄█")},
}

// Graph draws values (oldest first) as an area chart in the frame's
// graph symbols, the newest value in the rightmost column, filled from
// the bottom. A NaN leaves its column empty; maxV <= 0 scales to the
// largest value shown. A style with a gradient colours each cell by its
// height: the start at the bottom, the end at the top.
func Graph(f *Frame, r Rect, values []float64, maxV float64, st Style) {
	if r.Empty() {
		return
	}
	g, ok := graphGlyphs[f.Graph]
	if !ok {
		g = graphGlyphs[GraphBraille]
	}
	top := r.H * g.heights
	levels := dotLevels(values, r.W*g.perCell, top, maxV)
	for cx := 0; cx < r.W; cx++ {
		high := levels[cx*g.perCell]
		if g.perCell == 2 {
			high = max(high, levels[cx*2+1])
		}
		for row := 0; row < r.H; row++ { // row 0 = the bottom
			var ch rune
			if g.glyphs == nil {
				ch = brailleCell(levels[2*cx]-row*4, levels[2*cx+1]-row*4)
			} else {
				ch = g.glyphs[min(max(levels[cx]-row*g.heights, 0), g.heights)]
			}
			cell := st
			if st.Grad != GradNone {
				// The colour of the highest dot in this cell, or of its
				// bottom when it's empty.
				h := min(max(high, row*g.heights+1), (row+1)*g.heights)
				cell.FG, cell.Grad = st.Grad.At(float64(h-1)/float64(max(top-1, 1))), GradNone
			}
			f.Set(r.X+cx, r.Y+r.H-1-row, ch, cell)
		}
	}
}

// sparkGlyphs are a sparkline's heights, from nothing: eight blocks, or
// four shades on the Linux console.
var sparkGlyphs = map[GraphSymbols][]rune{
	GraphBraille: []rune(" ▁▂▃▄▅▆▇█"),
	GraphBlock:   []rune(" ▁▂▃▄▅▆▇█"),
	GraphTTY:     []rune(" ░▒▓█"),
}

// Spark is values (oldest first) as a sparkline for a table's cell: a
// glyph a value, eight heights in blocks - one row of braille has four,
// too few to tell a quiet node from an idle one on a scale shared with a
// busy one - or four shades with GraphTTY. Scaled as Graph scales: a
// NaN or a value <= 0 is a blank, anything above zero at least the
// lowest height.
func Spark(values []float64, maxV float64, sym GraphSymbols) string {
	glyphs, ok := sparkGlyphs[sym]
	if !ok {
		glyphs = sparkGlyphs[GraphBlock]
	}
	levels := dotLevels(values, len(values), len(glyphs)-1, maxV)
	out := make([]rune, len(levels))
	for i, l := range levels {
		out[i] = glyphs[l]
	}
	return string(out)
}

// dotLevels is how many dots each of slots lights, from the bottom, out
// of top: the newest value in the last slot. A NaN or a value <= 0
// lights none, anything above zero at least one; maxV <= 0 scales to
// the largest value.
func dotLevels(values []float64, slots, top int, maxV float64) []int {
	if len(values) > slots {
		values = values[len(values)-slots:]
	}
	if maxV <= 0 {
		for _, v := range values {
			if !math.IsNaN(v) && v > maxV {
				maxV = v
			}
		}
		if maxV <= 0 {
			maxV = 1
		}
	}
	levels := make([]int, slots)
	for i, v := range values {
		if math.IsNaN(v) || v <= 0 {
			continue
		}
		n := int(math.Round(v / maxV * float64(top)))
		levels[slots-len(values)+i] = min(max(n, 1), top) // anything above zero shows
	}
	return levels
}

// brailleCell is the cell whose left and right columns light left and
// right dots from the bottom (at most four each; <= 0 none): a blank
// when neither does.
func brailleCell(left, right int) rune {
	bits := rune(0)
	for d := 0; d < 4; d++ {
		if left > d {
			bits |= brailleLeft[d]
		}
		if right > d {
			bits |= brailleRight[d]
		}
	}
	if bits == 0 {
		return ' '
	}
	return 0x2800 + bits
}

// Meter draws one row as a bar: each part's share of total in its
// style (full blocks), the rest in rest (light shades). A part below
// half a cell isn't drawn. A part's gradient colours its cells by their
// place along the whole bar.
func Meter(f *Frame, r Rect, parts []float64, styles []Style, total float64, rest Style) {
	if r.Empty() || total <= 0 {
		return
	}
	x, used := 0, 0.0
	for i, p := range parts {
		if p < 0 {
			p = 0
		}
		used += p
		end := int(math.Round(min(used, total) / total * float64(r.W)))
		st := rest
		if i < len(styles) {
			st = styles[i]
		}
		for ; x < end; x++ {
			cell := st
			if st.Grad != GradNone {
				cell.FG, cell.Grad = st.Grad.At(float64(x)/float64(max(r.W-1, 1))), GradNone
			}
			f.Set(r.X+x, r.Y, '█', cell)
		}
	}
	for ; x < r.W; x++ {
		f.Set(r.X+x, r.Y, '░', rest)
	}
}
