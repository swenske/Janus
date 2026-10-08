package termui

// Column is one column of a Table: Width 0 shares the slack with the
// other flexible columns; Right aligns numbers.
type Column struct {
	Title string
	Width int
	Right bool
}

// Table is rows of strings under a header, with a cursor the arrow keys
// move and a window that follows it.
type Table struct {
	Columns []Column
	Rows    [][]string
	// Tone is the style of a cell (the row's base when it returns the
	// zero Style); nil draws everything plain.
	Tone func(row, col int) Style
	// Heading marks rows drawn across the table in bold, like a group's
	// name above its members; nil for none.
	Heading func(row int) bool
	Cursor  int
	Offset  int
}

// Visible is how many rows r shows under the header.
func (t *Table) Visible(r Rect) int { return max(r.H-1, 0) }

// Clamp keeps the cursor on a row and the window around it.
func (t *Table) Clamp(visible int) {
	if len(t.Rows) == 0 {
		t.Cursor, t.Offset = 0, 0
		return
	}
	t.Cursor = min(max(t.Cursor, 0), len(t.Rows)-1)
	if visible <= 0 {
		return
	}
	if t.Cursor < t.Offset {
		t.Offset = t.Cursor
	}
	if t.Cursor >= t.Offset+visible {
		t.Offset = t.Cursor - visible + 1
	}
	t.Offset = min(max(t.Offset, 0), max(len(t.Rows)-visible, 0))
}

// Key moves the cursor: arrows, PgUp/PgDn (by page rows), Home/End.
// It reports whether k was one of them.
func (t *Table) Key(k string, page int) bool {
	switch Normalize(k) {
	case KeyUp:
		t.Cursor--
	case KeyDown:
		t.Cursor++
	case KeyPgUp:
		t.Cursor -= max(page, 1)
	case KeyPgDn:
		t.Cursor += max(page, 1)
	case KeyHome:
		t.Cursor = 0
	case KeyEnd:
		t.Cursor = len(t.Rows) - 1
	default:
		return false
	}
	t.Clamp(page)
	return true
}

// minFlex is the least a flexible column gets before the rightmost
// columns are dropped to make room.
const minFlex = 8

// widths lays the columns out in w cells: fixed widths as given, the
// flexible ones sharing what's left. On a narrow table the rightmost
// columns go (width 0, not drawn) rather than squeezing every column
// into nothing.
func (t *Table) widths(w int) []int {
	out := make([]int, len(t.Columns))
	shown := len(t.Columns)
	for shown > 1 {
		fixed, flex := 0, 0
		for _, c := range t.Columns[:shown] {
			if c.Width > 0 {
				fixed += c.Width
			} else {
				flex++
			}
		}
		room := w - fixed - (shown - 1)
		if flex == 0 && room >= 0 || flex > 0 && room/flex >= minFlex {
			break
		}
		shown--
	}
	fixed, flex := 0, 0
	for i, c := range t.Columns[:shown] {
		if c.Width > 0 {
			out[i] = c.Width
			fixed += c.Width
		} else {
			flex++
		}
	}
	if flex > 0 {
		share := max((w-fixed-(shown-1))/flex, 3)
		for i, c := range t.Columns[:shown] {
			if c.Width == 0 {
				out[i] = share
			}
		}
	}
	return out
}

// Draw paints the header and the rows in view; the cursor's row is
// drawn in cursor when focused. Cells are cut to their column.
func (t *Table) Draw(f *Frame, r Rect, focused bool, head, cursor Style) {
	if r.Empty() {
		return
	}
	visible := t.Visible(r)
	t.Clamp(visible)
	widths := t.widths(r.W)
	x := r.X
	for i, c := range t.Columns {
		if x >= r.X+r.W || widths[i] == 0 {
			break
		}
		w := min(widths[i], r.X+r.W-x)
		title := Pad(c.Title, w)
		if c.Right {
			title = PadLeft(c.Title, w)
		}
		f.Text(x, r.Y, title, head, w)
		x += widths[i] + 1
	}
	for n := 0; n < visible; n++ {
		i := t.Offset + n
		if i >= len(t.Rows) {
			break
		}
		y := r.Y + 1 + n
		row := t.Rows[i]
		base := Style{}
		if t.Tone != nil {
			base = t.Tone(i, -1)
		}
		if focused && i == t.Cursor {
			base = cursor
			f.Fill(Rect{r.X, y, r.W, 1}, ' ', base)
		}
		if t.Heading != nil && t.Heading(i) {
			text := ""
			if len(row) > 0 {
				text = row[0]
			}
			st := base
			st.Bold = true
			f.Text(r.X, y, Truncate(text, r.W), st, r.W)
			continue
		}
		x = r.X
		for c, col := range t.Columns {
			if x >= r.X+r.W || widths[c] == 0 {
				break
			}
			w := min(widths[c], r.X+r.W-x)
			cell := ""
			if c < len(row) {
				cell = row[c]
			}
			st := base
			if t.Tone != nil && (!focused || i != t.Cursor) {
				if s := t.Tone(i, c); s != (Style{}) {
					st = s
				}
			}
			if col.Right {
				cell = PadLeft(cell, w)
			} else {
				cell = Pad(cell, w)
			}
			f.Text(x, y, cell, st, w)
			x += widths[c] + 1
		}
	}
}
