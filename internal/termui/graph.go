package termui

import "math"

// Braille dots, bottom to top, for the left and right column of a cell.
var (
	brailleLeft  = [4]rune{0x40, 0x04, 0x02, 0x01}
	brailleRight = [4]rune{0x80, 0x20, 0x10, 0x08}
)

// Graph draws values (oldest first) as an area chart in braille, the
// newest value in the rightmost column: two values per cell column,
// four levels per cell row, filled from the bottom. A NaN leaves its
// column empty; maxV <= 0 scales to the largest value shown.
func Graph(f *Frame, r Rect, values []float64, maxV float64, st Style) {
	if r.Empty() {
		return
	}
	levels := dotLevels(values, r.W*2, r.H*4, maxV)
	for cx := 0; cx < r.W; cx++ {
		for row := 0; row < r.H; row++ { // row 0 = the bottom
			f.Set(r.X+cx, r.Y+r.H-1-row, brailleCell(levels[2*cx]-row*4, levels[2*cx+1]-row*4), st)
		}
	}
}

// sparkBlocks are a sparkline's eight heights, from nothing.
var sparkBlocks = []rune(" ▁▂▃▄▅▆▇█")

// Spark is values (oldest first) as a sparkline for a table's cell: a
// block a value, eight heights - one row of braille has four, too few
// to tell a quiet node from an idle one on a scale shared with a busy
// one. Scaled as Graph scales: a NaN or a value <= 0 is a blank,
// anything above zero at least the lowest block.
func Spark(values []float64, maxV float64) string {
	levels := dotLevels(values, len(values), len(sparkBlocks)-1, maxV)
	out := make([]rune, len(levels))
	for i, l := range levels {
		out[i] = sparkBlocks[l]
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
// style (full blocks), the rest in rest (light blocks). A part below
// half a cell isn't drawn.
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
			f.Set(r.X+x, r.Y, '█', st)
		}
	}
	for ; x < r.W; x++ {
		f.Set(r.X+x, r.Y, '░', rest)
	}
}
