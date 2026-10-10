package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/termui"
)

// The dashboard's options, bpytop-style: M opens a menu of them - the
// theme and how it's drawn, the defaults of the fleet's trend and of the
// processes - each changed with ← and → and seen at once, saved when the
// menu closes in tui.json beside the contexts (janusctl.json).

// tuiSettings are what the menu sets.
type tuiSettings struct {
	Theme       string `json:"theme"`
	Background  bool   `json:"background"` // paint the theme's background
	Colors      string `json:"colors"`     // auto, truecolor, 256, 16
	Graphs      string `json:"graphs"`     // braille, block, tty
	Rounded     bool   `json:"rounded"`
	Interval    string `json:"interval"`
	TrendMetric string `json:"trend_metric"` // a trendMetrics key, or "off"
	TrendWindow string `json:"trend_window"`
	TrendScale  string `json:"trend_scale"`
	ProcSort    string `json:"process_sort"`
	ProcReverse bool   `json:"process_reverse"` // smallest first
}

func defaultSettings() tuiSettings {
	return tuiSettings{Theme: termui.ThemeJanus.Name, Background: true, Colors: "auto", Graphs: "braille", Rounded: true,
		Interval: "2s", TrendMetric: trendMetrics[0].key, TrendWindow: fmtWindow(trendWindows[trendDefaultWindow]), TrendScale: scaleLinear.key(), ProcSort: procSorts[0]}
}

// settingsPath is tui.json beside the contexts' file.
func settingsPath() string { return filepath.Join(filepath.Dir(configPath()), "tui.json") }

// loadSettings reads tui.json over the defaults: a value it doesn't
// know is left at its default, a missing file is the defaults.
func loadSettings(path string) (tuiSettings, error) {
	s := defaultSettings()
	data, err := os.ReadFile(path) //nolint:gosec // G304: janusctl's own settings file
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return defaultSettings(), fmt.Errorf("%s: %w", path, err)
	}
	return s.valid(), nil
}

// valid is s with every unknown value back to its default.
func (s tuiSettings) valid() tuiSettings {
	d := defaultSettings()
	if termui.ThemeNamed(s.Theme) == nil {
		s.Theme = d.Theme
	}
	if !oneOf(s.Colors, colorChoices) {
		s.Colors = d.Colors
	}
	if !oneOf(s.Graphs, graphChoices) {
		s.Graphs = d.Graphs
	}
	if iv, err := time.ParseDuration(s.Interval); err != nil || iv < time.Second || iv > 30*time.Second {
		s.Interval = d.Interval
	}
	if _, ok := trendMetricIndex(s.TrendMetric); !ok && s.TrendMetric != "off" {
		s.TrendMetric = d.TrendMetric
	}
	if trendWindowIndex(s.TrendWindow) < 0 {
		s.TrendWindow = d.TrendWindow
	}
	if _, ok := scaleNamed(s.TrendScale); !ok {
		s.TrendScale = d.TrendScale
	}
	if !oneOf(s.ProcSort, procSorts) {
		s.ProcSort = d.ProcSort
	}
	return s
}

// save writes s to path atomically, readable by its owner only.
func (s tuiSettings) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var (
	colorChoices = []string{"auto", "truecolor", "256", "16"}
	graphChoices = []string{"braille", "block", "tty"}
)

func oneOf(v string, choices []string) bool {
	for _, c := range choices {
		if v == c {
			return true
		}
	}
	return false
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// applySettings makes the dashboard draw and behave as a.settings say.
func (a *app) applySettings() {
	s := a.settings
	depth := a.detected
	switch s.Colors {
	case "truecolor":
		depth = termui.DepthTrue
	case "256":
		depth = termui.Depth256
	case "16":
		depth = termui.Depth16
	}
	a.graphs = termui.GraphBraille
	switch s.Graphs {
	case "block":
		a.graphs = termui.GraphBlock
	case "tty":
		a.graphs = termui.GraphTTY
	}
	tty := a.graphs == termui.GraphTTY
	a.palette = termui.Palette{Depth: depth, Theme: termui.ThemeNamed(s.Theme), Background: s.Background, Square: !s.Rounded || tty, Console: tty}
	a.repaint = true
}

// captureSettings brings a.settings up to what the keys changed since:
// the interval, the fleet's trend, the processes' order on a node.
func (a *app) captureSettings() {
	a.settings.Interval = time.Duration(a.interval.Load()).String()
	if fs := a.fleet; fs != nil {
		a.settings.TrendMetric = "off"
		if !fs.trendOff {
			a.settings.TrendMetric = trendMetrics[fs.metric].key
		}
		a.settings.TrendWindow = fmtWindow(trendWindows[fs.window])
		a.settings.TrendScale = fs.scale.key()
	}
	if n, ok := a.screen.(*nodeScreen); ok {
		a.settings.ProcSort, a.settings.ProcReverse = procSorts[n.procSort], !n.procDesc
	}
}

// setupFleet gives a fleet screen the saved trend.
func (a *app) setupFleet(fs *fleetScreen) {
	if i, ok := trendMetricIndex(a.settings.TrendMetric); ok {
		fs.metric, fs.trendOff = i, false
	} else {
		fs.trendOff = a.settings.TrendMetric == "off"
	}
	if i := trendWindowIndex(a.settings.TrendWindow); i >= 0 {
		fs.window = i
	}
	if sc, ok := scaleNamed(a.settings.TrendScale); ok && (sc != scaleFull || trendMetrics[fs.metric].pct) {
		fs.scale = sc
	}
}

// newNode is a node screen with the saved order of the processes.
func (a *app) newNode(s *sampler, fromFleet bool) *nodeScreen {
	n := newNodeScreen(s, fromFleet)
	for i, p := range procSorts {
		if p == a.settings.ProcSort {
			n.procSort = i
		}
	}
	n.procDesc = !a.settings.ProcReverse
	return n
}

// menuItem is one option: its values, the current one, what choosing
// one does, and what it means.
type menuItem struct {
	name   string
	values func(a *app) []string
	get    func(a *app) string
	show   func(a *app, v string) string // how a value reads; nil: as it is
	set    func(a *app, v string)
	help   func(a *app) []string
}

var menuItems = []menuItem{
	{name: "Theme",
		values: func(*app) []string {
			var out []string
			for _, t := range termui.Themes() {
				out = append(out, t.Name)
			}
			return out
		},
		get: func(a *app) string { return a.settings.Theme },
		set: func(a *app, v string) { a.settings.Theme = v; a.applySettings() },
		help: func(a *app) []string {
			t := termui.ThemeNamed(a.settings.Theme)
			bg := "dark"
			if t.Light() {
				bg = "light"
			}
			lines := []string{t.About + ".", ""}
			if t == termui.ThemeJanus {
				return append(lines, "bpytop's 16 themes and tty are the others:", "← and → show each at once.")
			}
			if t == termui.ThemeTTY {
				return append(lines, "On the Linux console, choose the tty graph", "symbols too: its font has no braille.")
			}
			return append(lines, "Made for a "+bg+" background: with the theme's", "background off, your terminal's should be "+bg+" too.")
		}},
	{name: "Theme background",
		values: func(*app) []string { return []string{"on", "off"} },
		get:    func(a *app) string { return onOff(a.settings.Background) },
		set:    func(a *app, v string) { a.settings.Background = v == "on"; a.applySettings() },
		help: func(*app) []string {
			return []string{"Paint the theme's background behind everything.", "", "Off keeps your terminal's own (its transparency):", "the theme's colours are then drawn on it."}
		}},
	{name: "Colours",
		values: func(*app) []string { return colorChoices },
		get:    func(a *app) string { return a.settings.Colors },
		show: func(a *app, v string) string {
			if v != "auto" {
				return v
			}
			return "auto (" + depthName(a.detected) + ")"
		},
		set: func(a *app, v string) { a.settings.Colors = v; a.applySettings() },
		help: func(*app) []string {
			return []string{"How many colours your terminal takes.", "", "auto reads COLORTERM and TERM (and NO_COLOR);", "a theme's colours are brought to the nearest of", "256 or 16 on a terminal that takes no more."}
		}},
	{name: "Graph symbols",
		values: func(*app) []string { return graphChoices },
		get:    func(a *app) string { return a.settings.Graphs },
		set:    func(a *app, v string) { a.settings.Graphs = v; a.applySettings() },
		help: func(*app) []string {
			return []string{"What the graphs are drawn with.", "", "braille: two values a cell, four heights a row.", "block: a value a cell, eight heights (▁ to █).", "tty: the Linux console's font - half blocks,", "shades, square corners, nothing it lacks."}
		}},
	{name: "Rounded corners",
		values: func(*app) []string { return []string{"on", "off"} },
		get:    func(a *app) string { return onOff(a.settings.Rounded) },
		set:    func(a *app, v string) { a.settings.Rounded = v == "on"; a.applySettings() },
		help: func(*app) []string {
			return []string{"Round the boxes' corners (╭╮), or square them (┌┐)", "for a font without the rounded ones."}
		}},
	{name: "Refresh",
		values: func(*app) []string {
			var out []string
			for _, d := range tuiIntervals {
				out = append(out, d.String())
			}
			return out
		},
		get: func(a *app) string { return time.Duration(a.interval.Load()).String() },
		set: func(a *app, v string) {
			if d, err := time.ParseDuration(v); err == nil {
				a.interval.Store(int64(d))
				a.settings.Interval = v
				a.kickAll()
			}
		},
		help: func(*app) []string {
			return []string{"How often the nodes are asked - + and - too.", "", "-interval sets it for one run."}
		}},
	{name: "Fleet trend",
		values: func(*app) []string {
			out := []string{}
			for _, m := range trendMetrics {
				out = append(out, m.key)
			}
			return append(out, "off")
		},
		get: func(a *app) string { return a.settings.TrendMetric },
		show: func(_ *app, v string) string {
			if i, ok := trendMetricIndex(v); ok {
				return trendMetrics[i].name
			}
			return "hidden"
		},
		set: func(a *app, v string) {
			a.settings.TrendMetric = v
			if a.fleet != nil {
				a.setupFleet(a.fleet)
			}
		},
		help: func(*app) []string {
			return []string{"What each line of the fleet ends with - m too.", "Every node against the same scale."}
		}},
	{name: "Trend window",
		values: func(*app) []string {
			var out []string
			for _, w := range trendWindows {
				out = append(out, fmtWindow(w))
			}
			return out
		},
		get: func(a *app) string { return a.settings.TrendWindow },
		set: func(a *app, v string) {
			a.settings.TrendWindow = v
			if a.fleet != nil {
				a.setupFleet(a.fleet)
			}
		},
		help: func(*app) []string { return []string{"How far back the trend goes - w too."} }},
	{name: "Trend scale",
		values: func(a *app) []string {
			out := []string{scaleLinear.key(), scaleLog.key()}
			if i, ok := trendMetricIndex(a.settings.TrendMetric); ok && trendMetrics[i].pct {
				out = append(out, scaleFull.key())
			}
			return out
		},
		get: func(a *app) string { return a.settings.TrendScale },
		show: func(_ *app, v string) string {
			sc, _ := scaleNamed(v)
			return sc.String()
		},
		set: func(a *app, v string) {
			a.settings.TrendScale = v
			if a.fleet != nil {
				a.setupFleet(a.fleet)
			}
		},
		help: func(*app) []string {
			return []string{"The trend's scale, the same for every node - y too.", "", "linear: up to the fleet's largest value.", "logarithmic: a quiet node beside a busy one.", "0 to 100 %: CPU and memory."}
		}},
	{name: "Processes by",
		values: func(*app) []string { return procSorts },
		get:    func(a *app) string { return a.settings.ProcSort },
		set: func(a *app, v string) {
			a.settings.ProcSort = v
			if n, ok := a.screen.(*nodeScreen); ok {
				for i, p := range procSorts {
					if p == v {
						n.procSort = i
					}
				}
			}
		},
		help: func(*app) []string {
			return []string{"How a node's processes are sorted - s too, with", "the processes focused."}
		}},
	{name: "Process order",
		values: func(*app) []string { return []string{"largest first", "smallest first"} },
		get: func(a *app) string {
			if a.settings.ProcReverse {
				return "smallest first"
			}
			return "largest first"
		},
		set: func(a *app, v string) {
			a.settings.ProcReverse = v == "smallest first"
			if n, ok := a.screen.(*nodeScreen); ok {
				n.procDesc = !a.settings.ProcReverse
			}
		},
		help: func(*app) []string { return []string{"Reverses the processes' order - r too."} }},
}

func depthName(d termui.Depth) string {
	switch d {
	case termui.DepthTrue:
		return "truecolor"
	case termui.Depth256:
		return "256"
	case termui.Depth16:
		return "16"
	}
	return "none"
}

// optionsMenu is the menu over the screen.
type optionsMenu struct{ cursor int }

// useSettings reads the saved settings: a flag given for this run wins
// (the interval, the theme).
func (a *app) useSettings(path string, intervalGiven bool, theme string) {
	a.settingsPath = path
	s, err := loadSettings(path)
	if err != nil {
		a.say("settings: "+err.Error(), termui.ColorWarn)
	}
	if theme != "" {
		s.Theme = termui.ThemeNamed(theme).Name
	}
	if d, err := time.ParseDuration(s.Interval); err == nil && !intervalGiven {
		a.interval.Store(int64(d))
	}
	a.settings, a.saved = s, s
	a.applySettings()
}

func (a *app) openMenu() {
	a.captureSettings()
	a.menu = &optionsMenu{}
}

// closeMenu saves the settings when they differ from the saved ones -
// what the keys changed since included.
func (a *app) closeMenu() {
	a.menu = nil
	a.captureSettings()
	if a.settings == a.saved || a.settingsPath == "" {
		return
	}
	if err := a.settings.save(a.settingsPath); err != nil {
		a.say("settings not saved: "+err.Error(), termui.ColorDanger)
		return
	}
	a.saved = a.settings
	a.say("settings saved", termui.ColorOK)
}

func (m *optionsMenu) key(a *app, k string) {
	it := menuItems[m.cursor]
	step := func(dir int) {
		vals := it.values(a)
		i := 0
		for j, v := range vals {
			if v == it.get(a) {
				i = j
			}
		}
		it.set(a, vals[(i+dir+len(vals))%len(vals)])
	}
	switch k {
	case termui.KeyUp, termui.KeyBackTab:
		m.cursor = (m.cursor + len(menuItems) - 1) % len(menuItems)
	case termui.KeyDown, termui.KeyTab:
		m.cursor = (m.cursor + 1) % len(menuItems)
	case termui.KeyLeft:
		step(-1)
	case termui.KeyRight, termui.KeyEnter, " ":
		step(1)
	case termui.KeyEsc, "M", "q":
		a.closeMenu()
	}
}

// render draws the menu in the middle of the screen: the options, then
// what the chosen one means, then the keys.
func (m *optionsMenu) render(a *app, f *termui.Frame) {
	nameW, valW := 0, 0
	for _, it := range menuItems {
		nameW = max(nameW, len(it.name))
		for _, v := range it.values(a) {
			if it.show != nil {
				v = it.show(a, v)
			}
			valW = max(valW, len([]rune(v)))
		}
	}
	helpH, helpW := 0, 0
	for _, it := range menuItems {
		helpH = max(helpH, len(it.help(a)))
		for _, l := range it.help(a) {
			helpW = max(helpW, len([]rune(l)))
		}
	}
	for _, t := range termui.Themes() { // as wide whatever the theme chosen
		helpW = max(helpW, len([]rune(t.About))+1)
	}
	w := min(max(nameW+valW+12, helpW+6), f.W-4)
	h := min(len(menuItems)+helpH+7, f.H-2)
	r := termui.Rect{X: (f.W - w) / 2, Y: (f.H - h) / 2, W: w, H: h}
	f.Fill(r, ' ', termui.Style{})
	in := f.Box(r, "Options", styleFocus, styleTitle)
	y := in.Y + 1
	for i, it := range menuItems {
		if y >= in.Y+in.H {
			break
		}
		v := it.get(a)
		if it.show != nil {
			v = it.show(a, v)
		}
		name, val := termui.Pad(it.name, nameW), "← "+v+" →"
		st, key := termui.Style{}, styleKey
		if i == m.cursor {
			st = styleCursor
			st.Bold = true
			key = st
			f.Fill(termui.Rect{X: in.X + 1, Y: y, W: in.W - 2, H: 1}, ' ', st)
		}
		x := in.X + 2
		x += f.Text(x, y, name, st, in.W-4) + 3
		if x < in.X+in.W-2 {
			f.Text(x, y, termui.Truncate(val, in.X+in.W-2-x), key, in.X+in.W-2-x)
		}
		y++
	}
	y++
	f.Set(in.X-1, y, '├', styleFocus)
	for x := in.X; x < in.X+in.W; x++ {
		f.Set(x, y, '─', styleFocus)
	}
	f.Set(in.X+in.W, y, '┤', styleFocus)
	y += 2
	for _, l := range menuItems[m.cursor].help(a) {
		if y >= in.Y+in.H-1 {
			break
		}
		f.Text(in.X+2, y, termui.Truncate(l, in.W-4), termui.Style{}, in.W-4)
		y++
	}
	keys := "↑↓ choose  ←→ change  Esc close"
	if a.settings != a.saved {
		keys = "↑↓ choose  ←→ change  Esc save and close"
	}
	f.Text(in.X+2, in.Y+in.H-1, termui.Truncate(keys, in.W-4), styleMuted, in.W-4)
}

// themeHelp is -theme's list, for its error.
func themeHelp() string {
	var names []string
	for _, t := range termui.Themes() {
		names = append(names, t.Name)
	}
	return strings.Join(names, ", ")
}
