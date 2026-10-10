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
	slots := r.W * 2
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
	levels := make([]int, slots) // dots lit in each slot, from the bottom
	top := r.H * 4
	for i, v := range values {
		if math.IsNaN(v) || v <= 0 {
			continue
		}
		n := int(math.Round(v / maxV * float64(top)))
		levels[slots-len(values)+i] = min(max(n, 1), top) // anything above zero shows
	}
	for cx := 0; cx < r.W; cx++ {
		for row := 0; row < r.H; row++ { // row 0 = the bottom
			bits := rune(0)
			for d := 0; d < 4; d++ {
				if levels[2*cx]-row*4 > d {
					bits |= brailleLeft[d]
				}
				if levels[2*cx+1]-row*4 > d {
					bits |= brailleRight[d]
				}
			}
			ch := ' '
			if bits != 0 {
				ch = 0x2800 + bits
			}
			f.Set(r.X+cx, r.Y+r.H-1-row, ch, st)
		}
	}
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
