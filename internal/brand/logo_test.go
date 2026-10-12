package brand

import (
	"strings"
	"testing"
)

func TestLogoIsRectangular(t *testing.T) {
	for i, row := range Logo {
		if n := len([]rune(row)); n != Width {
			t.Errorf("logo row %d is %d runes wide, want %d", i, n, Width)
		}
		_, bar, _ := Split(row)
		if strings.Trim(bar, "█▄▀▐▌ ") != "" {
			t.Errorf("logo row %d: bar columns %q contain something other than the bar", i, bar)
		}
		if strings.Trim(row, "█▄▀▐▌ ") != "" {
			t.Errorf("logo row %d: %q uses a glyph the console font may not have", i, row)
		}
		// Janus's two faces mirror each other: read right to left (with
		// the half blocks swapped), every row is itself.
		r := []rune(row)
		mirror := make([]rune, len(r))
		for j, c := range r {
			switch c {
			case '▐':
				c = '▌'
			case '▌':
				c = '▐'
			}
			mirror[len(r)-1-j] = c
		}
		if string(mirror) != row {
			t.Errorf("logo row %d isn't symmetric: %q vs %q", i, row, string(mirror))
		}
	}
	if BarStart+BarEnd-1 != Width-1 {
		t.Errorf("bar columns [%d, %d) aren't centered in a %d-column logo", BarStart, BarEnd, Width)
	}
}

func TestSplit(t *testing.T) {
	left, bar, right := Split(Logo[6])
	if left+bar+right != Logo[6] || bar != "▐████▌" || len([]rune(left)) != BarStart {
		t.Errorf("Split(%q) = %q %q %q", Logo[6], left, bar, right)
	}
}
