package termui

import (
	"strings"
	"testing"
)

func TestHex(t *testing.T) {
	for in, want := range map[string]RGB{"#d8643c": {216, 100, 60}, "#cc": {204, 204, 204}, "#ff ": {255, 255, 255}, "00bcd4": {0, 188, 212}} {
		if s := Hex(in); !s.set || s.rgb != want {
			t.Errorf("Hex(%q) = %+v, want %v", in, s, want)
		}
	}
	for _, in := range []string{"", "#", "#abc", "#zzzzzz", "red"} {
		if Hex(in).set {
			t.Errorf("Hex(%q) is a colour", in)
		}
	}
}

func TestNearest(t *testing.T) {
	for c, want := range map[RGB]int{{0, 0, 0}: 16, {255, 255, 255}: 231, {216, 100, 60}: 167, {128, 128, 128}: 244, {95, 135, 175}: 67} {
		if got := nearest256(c); got != want {
			t.Errorf("nearest256(%v) = %d, want %d", c, got, want)
		}
	}
	for c, want := range map[RGB]int{{0, 0, 0}: 0, {250, 10, 10}: 9, {0, 180, 0}: 2, {40, 42, 54}: 0, {248, 248, 242}: 15} {
		if got := nearest16(c); got != want {
			t.Errorf("nearest16(%v) = %d, want %d", c, got, want)
		}
	}
}

func TestThemeSGR(t *testing.T) {
	dracula := ThemeNamed("dracula")
	if dracula == nil {
		t.Fatal("no dracula theme")
	}
	p := Palette{Depth: DepthTrue, Theme: dracula}
	// Its box colours, title, selection.
	if s := p.SGR(Style{FG: ColorBoxCPU}); s != "\x1b[38;2;189;147;249m" {
		t.Errorf("cpu box = %q", s)
	}
	if s := p.SGR(Style{FG: ColorSelected, BG: ColorSelectedBG}); s != "\x1b[38;2;248;248;242;48;2;255;121;198m" {
		t.Errorf("selection = %q", s)
	}
	// Without its background, plain text is the terminal's own...
	if s := p.SGR(Style{}); s != "" {
		t.Errorf("plain, background off = %q", s)
	}
	// ...with it, the theme's text on the theme's background everywhere.
	p.Background = true
	if s := p.SGR(Style{}); s != "\x1b[38;2;248;248;242;48;2;40;42;54m" {
		t.Errorf("plain, background on = %q", s)
	}
	if s := p.SGR(Style{FG: ColorOK, Bold: true}); s != "\x1b[1;38;2;102;187;106;48;2;40;42;54m" {
		t.Errorf("ok on the background = %q", s)
	}
	// Smaller terminals get the nearest colours.
	p.Depth = Depth256
	if s := p.SGR(Style{FG: ColorBoxCPU}); s != "\x1b[38;5;141;48;5;236m" {
		t.Errorf("cpu box, 256 colours = %q", s)
	}
	p.Depth = Depth16
	if s := p.SGR(Style{FG: ColorDanger}); s != "\x1b[91;40m" {
		t.Errorf("danger, 16 colours = %q", s)
	}
	p.Depth = DepthNone
	if s := p.SGR(Style{FG: ColorDanger, Bold: true}); s != "" {
		t.Errorf("no colours = %q", s)
	}
	// Janus's has no selection colour: the row is reversed, as always.
	if s := (Palette{Depth: DepthTrue}).SGR(Style{FG: ColorSelected, BG: ColorSelectedBG, Bold: true}); s != "\x1b[1;7m" {
		t.Errorf("janus selection = %q", s)
	}
	// The terminal's 16 colours, whatever its depth.
	if s := (Palette{Depth: DepthTrue, Theme: ThemeTTY}).SGR(Style{FG: ColorBoxNet}); s != "\x1b[35m" {
		t.Errorf("tty, truecolor = %q", s)
	}
}

func TestThemeStatusOnLight(t *testing.T) {
	light, dark := ThemeNamed("whiteout"), ThemeNamed("nord")
	if !light.Light() || dark.Light() {
		t.Fatalf("whiteout light %v, nord light %v", light.Light(), dark.Light())
	}
	// Readable status colours on a white background: the Controller's light ones.
	if s := (Palette{Depth: DepthTrue, Theme: light}).SGR(Style{FG: ColorOK}); s != "\x1b[38;2;39;107;43m" {
		t.Errorf("ok on whiteout = %q", s)
	}
}

func TestGradient(t *testing.T) {
	th := bpytopTheme("t", "", map[string]string{"cpu_start": "#000000", "cpu_mid": "#808080", "cpu_end": "#ffffff", "used_start": "#ff0000", "used_mid": "", "used_end": "#0000ff"})
	at := func(c Color) RGB {
		g, step, _ := c.gradient()
		return th.gradientStep(g, step).rgb
	}
	near := func(a, b RGB) bool {
		d := func(x, y uint8) bool { return int(x)-int(y) <= 6 && int(y)-int(x) <= 6 }
		return d(a.R, b.R) && d(a.G, b.G) && d(a.B, b.B)
	}
	// Three colours: through the middle one (32 steps: near it).
	for frac, want := range map[float64]RGB{0: {0, 0, 0}, 0.5: {128, 128, 128}, 1: {255, 255, 255}} {
		if got := at(GradCPU.At(frac)); !near(got, want) {
			t.Errorf("cpu at %v = %v, want %v", frac, got, want)
		}
	}
	// Two: straight from one to the other.
	for frac, want := range map[float64]RGB{0: {255, 0, 0}, 0.5: {128, 0, 128}, 1: {0, 0, 255}} {
		if got := at(GradUsed.At(frac)); !near(got, want) {
			t.Errorf("used at %v = %v, want %v", frac, got, want)
		}
	}
	// Janus's are flat: its graphs look as they always did.
	for _, f := range []float64{0, 0.3, 1} {
		if s := (Palette{Depth: Depth256}).SGR(Style{FG: GradCPU.At(f)}); s != "\x1b[38;5;166m" {
			t.Errorf("janus cpu at %v = %q", f, s)
		}
	}
}

func TestThemes(t *testing.T) {
	names := map[string]bool{}
	for i, th := range Themes() {
		if names[th.Name] {
			t.Errorf("two themes named %s", th.Name)
		}
		names[th.Name] = true
		if i == 0 && th != ThemeJanus {
			t.Error("Janus's theme comes first")
		}
		if th.About == "" {
			t.Errorf("%s says nothing about itself", th.Name)
		}
		for _, c := range []Color{ColorTitle, ColorKey, ColorFocus, ColorBoxCPU, ColorOK, ColorDanger} {
			if !th.slot(c).set {
				t.Errorf("%s leaves slot %d to the terminal", th.Name, c)
			}
		}
	}
	// Janus, bpytop's built-in, tty, and bpytop's 15 files.
	if len(names) != 18 || !names["bpytop"] || !names["tty"] || !names["dracula"] || !names["gruvbox_dark_v2"] {
		t.Errorf("themes: %v", names)
	}
	if ThemeNamed("Nord") == nil || ThemeNamed("nope") != nil {
		t.Error("ThemeNamed: case-insensitive, nil for none")
	}
}

func TestRenderPaintsAndSwapsGlyphs(t *testing.T) {
	f := NewFrame(3, 2)
	f.Box(Rect{0, 0, 3, 2}, "", Style{}, Style{})
	plain := f.Render(nil, Palette{Depth: DepthTrue})
	if !strings.Contains(plain, "╭─╮") {
		t.Errorf("rounded by default: %q", plain)
	}
	sq := f.Render(nil, Palette{Depth: DepthTrue, Square: true})
	if !strings.Contains(sq, "┌─┐") || !strings.Contains(sq, "└─┘") {
		t.Errorf("square corners: %q", sq)
	}
	f.Text(1, 1, "…", Style{}, 0)
	if got := f.Render(nil, Palette{Depth: DepthTrue, Console: true}); !strings.Contains(got, "~") || strings.Contains(got, "╭") {
		t.Errorf("console glyphs: %q", got)
	}
	// A painted background starts every row, plain cells included.
	painted := f.Render(nil, Palette{Depth: DepthTrue, Theme: ThemeNamed("nord"), Background: true})
	if strings.Count(painted, "48;2;46;52;64") < 2 {
		t.Errorf("nord's background on each row: %q", painted)
	}
}
