package termui

import (
	"strconv"
	"strings"
)

// Color is a colour named for what it means - a slot the palette's
// theme fills (theme.go) - or one step of a gradient (Gradient.At).
type Color uint16

const (
	ColorDefault Color = iota // the text: the terminal's own, or the theme's over its background
	ColorMuted                // secondary text
	ColorAccent               // Janus's orange; a theme's highlight
	ColorOK
	ColorWarn
	ColorDanger
	ColorInfo
	ColorTitle      // a box's title
	ColorKey        // a key in the footer, the help, a dialog
	ColorFocus      // the focused box's border
	ColorBoxCPU     // the borders of the other boxes, by what they show
	ColorBoxMem     //
	ColorBoxNet     //
	ColorBoxProc    //
	ColorMeterBG    // a meter's empty part
	ColorSelected   // the cursor's row (on ColorSelectedBG; reversed without one)
	ColorSelectedBG //
	ColorDownload   // received traffic
	ColorUpload     // sent traffic
	ColorBackground // the theme's background, behind everything when the palette paints it
	numSlots
)

// Gradient is a range of colours a graph or a meter draws by height or
// position: a theme's start, middle and end.
type Gradient uint8

const (
	GradNone Gradient = iota
	GradCPU
	GradUsed
	GradCached
	GradDownload
	GradUpload
	GradProcess
	numGradients
)

// gradBase is the first gradient step; each gradient has gradSteps.
const gradBase Color = 64

const gradSteps = 32

// At is the gradient's colour at frac (0 the start, 1 the end).
func (g Gradient) At(frac float64) Color {
	step := int(frac*(gradSteps-1) + 0.5)
	step = min(max(step, 0), gradSteps-1)
	return gradBase + Color(int(g)*gradSteps+step)
}

// gradient is the gradient and step a colour is, ok false for a slot.
func (c Color) gradient() (Gradient, int, bool) {
	if c < gradBase || c&paintedText != 0 {
		return 0, 0, false
	}
	i := int(c - gradBase)
	return Gradient(i / gradSteps), i % gradSteps, true
}

// Style is how a cell is drawn. Grad asks Graph and Meter to colour each
// cell by its height or position; a cell itself keeps a Color.
type Style struct {
	FG      Color
	BG      Color
	Grad    Gradient
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

// Palette is how a frame is drawn on one terminal: its colours, the
// theme that fills the slots (nil: Janus's), whether the theme's
// background is painted, and the glyphs the terminal's font has.
type Palette struct {
	Depth      Depth
	Theme      *Theme
	Background bool // paint the theme's background (and text colour) everywhere
	Square     bool // square corners instead of rounded ones
	Console    bool // only glyphs the Linux console's font has (CP437)
}

// DetectPalette reads the usual variables: NO_COLOR or TERM=dumb means
// none (https://no-color.org); COLORTERM truecolor/24bit means 24-bit;
// a TERM naming 256 colours means 256; anything else the 16 basic ones.
func DetectPalette(getenv func(string) string) Palette {
	return Palette{Depth: DetectDepth(getenv)}
}

// DetectDepth is DetectPalette's depth.
func DetectDepth(getenv func(string) string) Depth {
	if getenv("NO_COLOR") != "" || getenv("TERM") == "dumb" {
		return DepthNone
	}
	switch strings.ToLower(getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return DepthTrue
	}
	if strings.Contains(getenv("TERM"), "256color") {
		return Depth256
	}
	return Depth16
}

func (p Palette) theme() *Theme {
	if p.Theme == nil {
		return ThemeJanus
	}
	return p.Theme
}

// painted reports whether the theme's background is drawn.
func (p Palette) painted() bool {
	return p.Depth != DepthNone && p.Background && p.theme().slot(ColorBackground).set
}

// SGR is the "\x1b[...m" that sets s on this palette - "" for the plain
// style without a painted background, or on a terminal without colours.
func (p Palette) SGR(s Style) string {
	if p.Depth == DepthNone {
		return ""
	}
	t := p.theme()
	painted := p.painted()
	if s == (Style{}) && !painted {
		return ""
	}
	fg, bg := s.FG, s.BG
	reverse := s.Reverse
	if bg == ColorSelectedBG && !t.slot(ColorSelectedBG).set {
		reverse, bg = true, 0 // a theme without a selection colour reverses the row
	}
	if fg == ColorDefault && painted {
		fg = paintedText
	}
	if bg == 0 && painted {
		bg = ColorBackground
	}
	var parts []string
	if s.Bold {
		parts = append(parts, "1")
	}
	if s.Dim {
		parts = append(parts, "2")
	}
	if reverse {
		parts = append(parts, "7")
	}
	if c := t.code(fg, p.Depth, false); c != "" {
		parts = append(parts, c)
	}
	if bg != 0 {
		if c := t.code(bg, p.Depth, true); c != "" {
			parts = append(parts, c)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(parts, ";") + "m"
}

// paintedText is ColorDefault when the theme's background is painted:
// the theme's own text colour then, not the terminal's.
const paintedText Color = 1 << 15

// code is the SGR parameters of c on depth, foreground or background:
// "" for a slot the theme leaves to the terminal.
func (t *Theme) code(c Color, d Depth, bg bool) string {
	var sh Shade
	if c == paintedText {
		sh = t.slot(ColorDefault)
	} else if g, step, ok := c.gradient(); ok {
		sh = t.gradientStep(g, step)
	} else {
		sh = t.slot(c)
	}
	if !sh.set {
		return ""
	}
	if t.ANSI || d == Depth16 {
		i := sh.c16
		if i < 0 {
			i = nearest16(sh.rgb)
		}
		base := 30
		if bg {
			base = 40
		}
		if i >= 8 {
			base += 60
			i -= 8
		}
		return strconv.Itoa(base + i)
	}
	lead := "38"
	if bg {
		lead = "48"
	}
	if d == Depth256 {
		i := sh.c256
		if i < 0 {
			i = nearest256(sh.rgb)
		}
		return lead + ";5;" + strconv.Itoa(i)
	}
	return lead + ";2;" + strconv.Itoa(int(sh.rgb.R)) + ";" + strconv.Itoa(int(sh.rgb.G)) + ";" + strconv.Itoa(int(sh.rgb.B))
}
