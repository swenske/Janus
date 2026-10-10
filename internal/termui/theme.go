package termui

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// RGB is a 24-bit colour.
type RGB struct{ R, G, B uint8 }

// Shade is one colour of a theme: 24-bit, with the xterm 256 and basic
// 16 colours a smaller terminal gets (-1: the nearest).
type Shade struct {
	rgb  RGB
	c256 int
	c16  int
	set  bool // false: the terminal's own
}

// Hex is the shade #rrggbb or #ww (a grey, bpytop's shorthand); "" or
// anything else is no shade.
func Hex(s string) Shade {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	var v []uint64
	switch len(s) {
	case 2:
		g, err := strconv.ParseUint(s, 16, 8)
		if err != nil {
			return Shade{}
		}
		v = []uint64{g, g, g}
	case 6:
		for i := 0; i < 6; i += 2 {
			c, err := strconv.ParseUint(s[i:i+2], 16, 8)
			if err != nil {
				return Shade{}
			}
			v = append(v, c)
		}
	default:
		return Shade{}
	}
	return Shade{rgb: RGB{uint8(v[0]), uint8(v[1]), uint8(v[2])}, c256: -1, c16: -1, set: true}
}

// fixed is a shade with its 256- and 16-colour codes chosen by hand.
func fixed(hex string, c256, c16 int) Shade {
	s := Hex(hex)
	s.c256, s.c16 = c256, c16
	return s
}

// ansi is one of the terminal's own 16 colours (0-15).
func ansi(i int) Shade { return Shade{c256: i, c16: i, set: true} }

// Theme fills a palette's colour slots and gradients.
type Theme struct {
	Name  string
	About string // one line: where it comes from
	// ANSI: only the terminal's 16 colours, whatever its depth.
	ANSI  bool
	slots [numSlots]Shade
	grads [numGradients][3]Shade // start, middle (optional), end (optional)

	once  sync.Once
	steps [numGradients][gradSteps]Shade
}

func (t *Theme) slot(c Color) Shade {
	if c >= numSlots {
		return Shade{}
	}
	return t.slots[c]
}

// gradientStep is step of g, interpolated between the theme's colours
// the first time it's asked.
func (t *Theme) gradientStep(g Gradient, step int) Shade {
	if g >= numGradients {
		return Shade{}
	}
	t.once.Do(t.interpolate)
	return t.steps[g][step]
}

func (t *Theme) interpolate() {
	for g := range t.grads {
		stops := []Shade{}
		for _, s := range t.grads[g] {
			if s.set {
				stops = append(stops, s)
			}
		}
		for i := 0; i < gradSteps; i++ {
			switch len(stops) {
			case 0:
			case 1:
				t.steps[g][i] = stops[0]
			default:
				pos := float64(i) / (gradSteps - 1) * float64(len(stops)-1)
				k := min(int(pos), len(stops)-2)
				t.steps[g][i] = blend(stops[k], stops[k+1], pos-float64(k), t.ANSI)
			}
		}
	}
}

// blend is a between a and b at f; the terminal's own colours don't mix
// - the nearer one is taken.
func blend(a, b Shade, f float64, ansiOnly bool) Shade {
	if ansiOnly || a.c16 >= 0 && a.rgb == (RGB{}) || b.c16 >= 0 && b.rgb == (RGB{}) {
		if f < 0.5 {
			return a
		}
		return b
	}
	if a.rgb == b.rgb && a.c256 == b.c256 && a.c16 == b.c16 {
		return a // a flat gradient keeps its hand-picked codes
	}
	mix := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*f)) }
	return Shade{rgb: RGB{mix(a.rgb.R, b.rgb.R), mix(a.rgb.G, b.rgb.G), mix(a.rgb.B, b.rgb.B)}, c256: -1, c16: -1, set: true}
}

// Light reports whether the theme draws dark text on a light background.
func (t *Theme) Light() bool {
	bg := t.slots[ColorBackground]
	if bg.set && !t.ANSI {
		return luminance(bg.rgb) > 0.5
	}
	return false
}

func luminance(c RGB) float64 {
	return (0.2126*float64(c.R) + 0.7152*float64(c.G) + 0.0722*float64(c.B)) / 255
}

// The xterm palette: the 16 basic colours as xterm draws them, the
// 6x6x6 cube's levels, the 24 greys.
var (
	xterm16 = [16]RGB{
		{0, 0, 0}, {205, 0, 0}, {0, 205, 0}, {205, 205, 0}, {0, 0, 238}, {205, 0, 205}, {0, 205, 205}, {229, 229, 229},
		{127, 127, 127}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0}, {92, 92, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255},
	}
	cubeLevels = [6]int{0, 95, 135, 175, 215, 255}
)

// distance is how far apart two colours look ("redmean").
func distance(a, b RGB) float64 {
	rm := (float64(a.R) + float64(b.R)) / 2
	dr, dg, db := float64(a.R)-float64(b.R), float64(a.G)-float64(b.G), float64(a.B)-float64(b.B)
	return (2+rm/256)*dr*dr + 4*dg*dg + (2+(255-rm)/256)*db*db
}

// nearest16 is the basic colour nearest c.
func nearest16(c RGB) int {
	best, bestD := 0, math.Inf(1)
	for i, x := range xterm16 {
		if d := distance(c, x); d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// nearest256 is the colour of xterm's cube or grey ramp nearest c (16
// and up: the first 16 are whatever the terminal makes them).
func nearest256(c RGB) int {
	level := func(v uint8) int {
		best := 0
		for i, l := range cubeLevels {
			if math.Abs(float64(int(v)-l)) < math.Abs(float64(int(v)-cubeLevels[best])) {
				best = i
			}
		}
		return best
	}
	r, g, b := level(c.R), level(c.G), level(c.B)
	cube := RGB{uint8(cubeLevels[r]), uint8(cubeLevels[g]), uint8(cubeLevels[b])}
	avg := (int(c.R) + int(c.G) + int(c.B)) / 3
	gi := min(max((avg-8+5)/10, 0), 23)
	grey := RGB{uint8(8 + 10*gi), uint8(8 + 10*gi), uint8(8 + 10*gi)}
	if distance(c, grey) < distance(c, cube) {
		return 232 + gi
	}
	return 16 + 36*r + 6*g + b
}

// The status colours of the Controller's themes, readable on a dark
// background and on a light one.
var (
	statusDark  = [4]Shade{Hex("#66bb6a"), Hex("#f0a93b"), Hex("#ef5350"), Hex("#60a5fa")}
	statusLight = [4]Shade{Hex("#276b2b"), Hex("#8a5200"), Hex("#b71c1c"), Hex("#1d4ed8")}
)

// ThemeJanus is Janus's own: the Controller's dark palette on the
// terminal's own background - --accent #D8643C, --ok #66bb6a, --warn
// #f0a93b, --danger #ef5350, --info #60a5fa, --muted #a39d91; 166 is
// the motd's orange.
var ThemeJanus = func() *Theme {
	muted := fixed("#a39d91", 245, 8)
	accent := fixed("#d8643c", 166, 3)
	ok, warn, danger, info := fixed("#66bb6a", 71, 2), fixed("#f0a93b", 178, 3), fixed("#ef5350", 167, 1), fixed("#60a5fa", 67, 6)
	t := &Theme{Name: "janus", About: "Janus's own: the Controller's colours, on your terminal's background"}
	t.slots = [numSlots]Shade{
		ColorMuted: muted, ColorAccent: accent, ColorOK: ok, ColorWarn: warn, ColorDanger: danger, ColorInfo: info,
		ColorTitle: accent, ColorKey: accent, ColorFocus: accent,
		ColorBoxCPU: muted, ColorBoxMem: muted, ColorBoxNet: muted, ColorBoxProc: muted, ColorMeterBG: muted,
		ColorDownload: info, ColorUpload: accent,
	}
	t.grads = [numGradients][3]Shade{
		GradCPU: {accent}, GradUsed: {accent}, GradCached: {info}, GradDownload: {info}, GradUpload: {accent}, GradProcess: {accent},
	}
	return t
}()

// ThemeTTY is the terminal's own 16 colours - for the Linux console, or
// a terminal whose palette you chose - in the spirit of btop's TTY
// theme.
var ThemeTTY = func() *Theme {
	t := &Theme{Name: "tty", About: "your terminal's own 16 colours, for the Linux console", ANSI: true}
	t.slots = [numSlots]Shade{
		ColorDefault: ansi(7), ColorMuted: ansi(8), ColorAccent: ansi(11), ColorOK: ansi(10), ColorWarn: ansi(11), ColorDanger: ansi(9), ColorInfo: ansi(14),
		ColorTitle: ansi(15), ColorKey: ansi(9), ColorFocus: ansi(15),
		ColorBoxCPU: ansi(2), ColorBoxMem: ansi(3), ColorBoxNet: ansi(5), ColorBoxProc: ansi(1), ColorMeterBG: ansi(8),
		ColorSelected: ansi(15), ColorSelectedBG: ansi(1), ColorDownload: ansi(12), ColorUpload: ansi(13), ColorBackground: ansi(0),
	}
	t.grads = [numGradients][3]Shade{
		GradCPU: {ansi(10), ansi(11), ansi(9)}, GradUsed: {ansi(1), {}, ansi(9)}, GradCached: {ansi(6), {}, ansi(14)},
		GradDownload: {ansi(4), {}, ansi(12)}, GradUpload: {ansi(5), {}, ansi(13)}, GradProcess: {ansi(2), ansi(3), ansi(1)},
	}
	return t
}()

// bpytopDefault is bpytop's built-in theme ("Default"), every other
// bpytop theme's fallback for a key it doesn't set.
var bpytopDefault = map[string]string{
	"main_bg": "#00", "main_fg": "#cc", "title": "#ee", "hi_fg": "#969696", "selected_bg": "#7e2626", "selected_fg": "#ee",
	"inactive_fg": "#40", "graph_text": "#60", "meter_bg": "#40", "proc_misc": "#0de756", "cpu_box": "#3d7b46", "mem_box": "#8a882e",
	"net_box": "#423ba5", "proc_box": "#923535", "div_line": "#30", "temp_start": "#4897d4", "temp_mid": "#5474e8", "temp_end": "#ff40b6",
	"cpu_start": "#50f095", "cpu_mid": "#f2e266", "cpu_end": "#fa1e1e", "free_start": "#223014", "free_mid": "#b5e685", "free_end": "#dcff85",
	"cached_start": "#0b1a29", "cached_mid": "#74e6fc", "cached_end": "#26c5ff", "available_start": "#292107", "available_mid": "#ffd77a",
	"available_end": "#ffb814", "used_start": "#3b1f1c", "used_mid": "#d9626d", "used_end": "#ff4769", "download_start": "#231a63",
	"download_mid": "#4f43a3", "download_end": "#b0a9de", "upload_start": "#510554", "upload_mid": "#7d4180", "upload_end": "#dcafde",
	"process_start": "#80d0a3", "process_mid": "#dcd179", "process_end": "#d45454",
}

// bpytopTheme maps a bpytop theme's keys onto the slots: its box colours
// on the boxes, its gradients on the graphs, its selection on the
// cursor, its title colour on the focused box (the selection's is too
// near some box colours: bpytop's own proc_box); secondary text halfway
// between its text and its background; the status colours the Controller's, for a light or a
// dark background. A key it leaves out falls back as bpytop does.
func bpytopTheme(name, about string, keys map[string]string) *Theme {
	k := map[string]string{}
	for key, v := range keys {
		k[key] = v
	}
	if _, ok := k["graph_text"]; !ok && k["inactive_fg"] != "" {
		k["graph_text"] = k["inactive_fg"]
	}
	if _, ok := k["meter_bg"]; !ok && k["inactive_fg"] != "" {
		k["meter_bg"] = k["inactive_fg"]
	}
	if _, ok := k["process_start"]; !ok && k["cpu_start"] != "" {
		k["process_start"], k["process_mid"], k["process_end"] = k["cpu_start"], k["cpu_mid"], k["cpu_end"]
	}
	for key, v := range bpytopDefault {
		if _, ok := k[key]; !ok {
			k[key] = v
		}
	}
	h := func(key string) Shade { return Hex(k[key]) }
	t := &Theme{Name: name, About: about}
	fg, bg := h("main_fg"), h("main_bg")
	muted := h("graph_text")
	if fg.set && bg.set {
		muted = blend(fg, bg, 0.45, false)
	}
	status := statusDark
	if bg.set && luminance(bg.rgb) > 0.5 {
		status = statusLight
	}
	bright := func(name string) Shade { // a gradient's end, its brightest in most themes
		if s := h(name + "_end"); s.set {
			return s
		}
		return h(name + "_mid")
	}
	t.slots = [numSlots]Shade{
		ColorDefault: fg, ColorMuted: muted, ColorAccent: h("hi_fg"),
		ColorOK: status[0], ColorWarn: status[1], ColorDanger: status[2], ColorInfo: status[3],
		ColorTitle: h("title"), ColorKey: h("hi_fg"), ColorFocus: h("title"),
		ColorBoxCPU: h("cpu_box"), ColorBoxMem: h("mem_box"), ColorBoxNet: h("net_box"), ColorBoxProc: h("proc_box"),
		ColorMeterBG: h("meter_bg"), ColorSelected: h("selected_fg"), ColorSelectedBG: h("selected_bg"),
		ColorDownload: bright("download"), ColorUpload: bright("upload"), ColorBackground: bg,
	}
	g := func(name string) [3]Shade { return [3]Shade{h(name + "_start"), h(name + "_mid"), h(name + "_end")} }
	t.grads = [numGradients][3]Shade{
		GradCPU: g("cpu"), GradUsed: g("used"), GradCached: g("cached"), GradDownload: g("download"), GradUpload: g("upload"), GradProcess: g("process"),
	}
	return t
}

// bpytopFile is one of bpytop's theme files (themes_bpytop.go).
type bpytopFile struct {
	author string
	keys   map[string]string
}

// themes is every theme by name: Janus's first, then bpytop's built-in
// one, the 16-colour one, and bpytop's theme files by name.
var themes = func() []*Theme {
	out := []*Theme{ThemeJanus, bpytopTheme("bpytop", "bpytop's default theme, by aristocratos", bpytopDefault), ThemeTTY}
	var names []string
	for n := range bpytopThemes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		about := "bpytop's " + n + " theme"
		if a := bpytopThemes[n].author; a != "" {
			about += ", by " + a
		}
		out = append(out, bpytopTheme(n, about, bpytopThemes[n].keys))
	}
	return out
}()

// Themes is every theme, Janus's first.
func Themes() []*Theme { return themes }

// ThemeNamed is the theme called name, nil if none is.
func ThemeNamed(name string) *Theme {
	for _, t := range themes {
		if strings.EqualFold(t.Name, name) {
			return t
		}
	}
	return nil
}
