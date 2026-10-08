package termui

import "strings"

// Color is a colour of the palette, named for what it means - the
// palette says what each one is on this terminal.
type Color uint8

const (
	ColorDefault Color = iota // the terminal's own foreground
	ColorMuted                // secondary text, borders
	ColorAccent               // the brand orange: titles, the focus
	ColorOK
	ColorWarn
	ColorDanger
	ColorInfo
)

// Style is how a cell is drawn.
type Style struct {
	FG      Color
	Bold    bool
	Dim     bool
	Reverse bool
}

// Depth is how many colours the terminal takes.
type Depth uint8

const (
	DepthNone Depth = iota // no colour and no attribute at all (NO_COLOR, TERM=dumb)
	Depth16
	Depth256
	DepthTrue
)

// Palette turns styles into SGR sequences for one terminal.
type Palette struct{ Depth Depth }

// DetectPalette reads the usual variables: NO_COLOR or TERM=dumb means
// none (https://no-color.org); COLORTERM truecolor/24bit means 24-bit;
// a TERM naming 256 colours means 256; anything else the 16 basic ones.
func DetectPalette(getenv func(string) string) Palette {
	if getenv("NO_COLOR") != "" || getenv("TERM") == "dumb" {
		return Palette{DepthNone}
	}
	switch strings.ToLower(getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return Palette{DepthTrue}
	}
	if strings.Contains(getenv("TERM"), "256color") {
		return Palette{Depth256}
	}
	return Palette{Depth16}
}

// The Controller's dark theme, where the terminal's background is dark
// too: --accent #D8643C, --ok #66bb6a, --warn #f0a93b, --danger #ef5350,
// --info #60a5fa, --muted #a39d91.
var (
	trueColors = map[Color]string{
		ColorMuted:  "38;2;163;157;145",
		ColorAccent: "38;2;216;100;60",
		ColorOK:     "38;2;102;187;106",
		ColorWarn:   "38;2;240;169;59",
		ColorDanger: "38;2;239;83;80",
		ColorInfo:   "38;2;96;165;250",
	}
	colors256 = map[Color]string{ // the nearest of xterm's 256; 166 is the motd's orange
		ColorMuted:  "38;5;245",
		ColorAccent: "38;5;166",
		ColorOK:     "38;5;71",
		ColorWarn:   "38;5;178",
		ColorDanger: "38;5;167",
		ColorInfo:   "38;5;67",
	}
	colors16 = map[Color]string{
		ColorMuted:  "90",
		ColorAccent: "33",
		ColorOK:     "32",
		ColorWarn:   "33",
		ColorDanger: "31",
		ColorInfo:   "36",
	}
)

// SGR is the "\x1b[...m" that sets s on this palette - "" for the plain
// style, or on a terminal without colours.
func (p Palette) SGR(s Style) string {
	if p.Depth == DepthNone || s == (Style{}) {
		return ""
	}
	var parts []string
	if s.Bold {
		parts = append(parts, "1")
	}
	if s.Dim {
		parts = append(parts, "2")
	}
	if s.Reverse {
		parts = append(parts, "7")
	}
	var table map[Color]string
	switch p.Depth {
	case DepthTrue:
		table = trueColors
	case Depth256:
		table = colors256
	default:
		table = colors16
	}
	if c, ok := table[s.FG]; ok {
		parts = append(parts, c)
	}
	if len(parts) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(parts, ";") + "m"
}
