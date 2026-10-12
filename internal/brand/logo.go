// Package brand holds Janus's logo as text, for the places that draw it
// with characters: janusd's console banner and janusctl tui's splash.
package brand

// Logo is brand/favicon/favicon.svg drawn with half-block characters
// (one character = two square "pixels" vertically, 3 SVG units per
// pixel) - only glyphs the kernel's 8x16 console font has (█ ▀ ▄ ▌ ▐).
// The two outward-facing triangles are Janus's two faces, their edges a
// regular 45° staircase so they read as straight; the taller bar between
// them is the brand-orange stroke, five cells wide visually (▐ and ▌ are
// half cells) with its corners left out for the SVG's rounded ones.
// Columns [BarStart, BarEnd) are the bar; the drawing is symmetric
// around its middle.
var Logo = []string{
	"           ▄▄▄▄           ",
	"          ▐████▌          ",
	"          ▐████▌          ",
	"        ▄ ▐████▌ ▄        ",
	"      ▄██ ▐████▌ ██▄      ",
	"    ▄████ ▐████▌ ████▄    ",
	"  ▄██████ ▐████▌ ██████▄  ",
	"  ▀██████ ▐████▌ ██████▀  ",
	"    ▀████ ▐████▌ ████▀    ",
	"      ▀██ ▐████▌ ██▀      ",
	"        ▀ ▐████▌ ▀        ",
	"          ▐████▌          ",
	"          ▐████▌          ",
	"           ▀▀▀▀           ",
}

// The bar's columns in every Logo row.
const (
	BarStart = 10
	BarEnd   = 16
)

// Width is Logo's width in columns.
var Width = len([]rune(Logo[0]))

// Split cuts a Logo row into its left face, the bar and its right face.
func Split(row string) (left, bar, right string) {
	r := []rune(row)
	return string(r[:BarStart]), string(r[BarStart:BarEnd]), string(r[BarEnd:])
}
