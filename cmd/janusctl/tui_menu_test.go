package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/termui"
)

func TestTUISettingsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus", "tui.json")
	s, err := loadSettings(path)
	if err != nil || s != defaultSettings() {
		t.Fatalf("no file: %+v, %v - want the defaults", s, err)
	}
	s.Theme, s.Background, s.Graphs, s.Rounded, s.TrendMetric, s.ProcReverse = "nord", false, "tty", false, "off", true
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("tui.json: %v, %v - want 0600", fi.Mode(), err)
	}
	if got, err := loadSettings(path); err != nil || got != s {
		t.Errorf("read back %+v, %v - want %+v", got, err, s)
	}
	// What it doesn't know goes back to its default; the rest stays.
	if err := os.WriteFile(path, []byte(`{"theme":"nope","colors":"8","graphs":"ascii","interval":"1h","trend_metric":"x","trend_window":"2m","trend_scale":"cubic","process_sort":"name","rounded":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadSettings(path)
	want := defaultSettings()
	want.Rounded = false
	if err != nil || got != want {
		t.Errorf("unknown values: %+v, %v - want %+v", got, err, want)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadSettings(path); err == nil || got != defaultSettings() {
		t.Errorf("a broken file: %+v, %v - want the defaults and an error", got, err)
	}
}

func TestOptionsMenu(t *testing.T) {
	a := testApp(t)
	trendFleet(t, a)
	a.settingsPath = filepath.Join(t.TempDir(), "tui.json")
	a.detected = termui.DepthTrue
	a.applySettings()
	a.render(120, 40)
	a.key("M")
	if a.menu == nil {
		t.Fatal("M opens the menu")
	}
	golden(t, "tui-menu-120x40.txt", a.render(120, 40))
	// ← → change the theme and the screen follows at once.
	a.repaint = false
	a.key(termui.KeyRight)
	if a.settings.Theme != "bpytop" || a.palette.Theme != termui.ThemeNamed("bpytop") || !a.repaint {
		t.Errorf("→ on Theme: %q, palette %v, repaint %v", a.settings.Theme, a.palette.Theme.Name, a.repaint)
	}
	a.key(termui.KeyLeft)
	a.key(termui.KeyLeft)
	if a.settings.Theme != termui.Themes()[len(termui.Themes())-1].Name {
		t.Errorf("← from the first theme goes round: %q", a.settings.Theme)
	}
	frame := strings.Join(a.render(120, 40).Lines(), "\n")
	if !strings.Contains(frame, "bpytop's whiteout theme, by aristocratos.") || !strings.Contains(frame, "Made for a light background") {
		t.Errorf("the chosen theme's description:\n%s", frame)
	}
	// Down to the background, the colours, the graphs, the corners.
	a.key(termui.KeyDown)
	a.key(termui.KeyRight)
	if a.settings.Background || a.palette.Background {
		t.Error("→ on Theme background turns it off")
	}
	a.key(termui.KeyDown)
	a.key(termui.KeyRight)
	if a.settings.Colors != "truecolor" || a.palette.Depth != termui.DepthTrue {
		t.Errorf("colours: %q, depth %v", a.settings.Colors, a.palette.Depth)
	}
	a.key(termui.KeyRight)
	if a.palette.Depth != termui.Depth256 {
		t.Errorf("256: depth %v", a.palette.Depth)
	}
	a.key(termui.KeyDown)
	a.key(termui.KeyRight)
	if f := a.render(120, 40); a.graphs != termui.GraphBlock || f.Graph != termui.GraphBlock {
		t.Errorf("block graphs: %v", a.graphs)
	}
	a.key(termui.KeyRight)
	if !a.palette.Console || !a.palette.Square {
		t.Errorf("tty graphs: the console's glyphs and square corners: %+v", a.palette)
	}
	a.key(termui.KeyLeft)
	a.key(termui.KeyDown)
	a.key(termui.KeyEnter)
	if a.settings.Rounded || !a.palette.Square {
		t.Errorf("Enter on Rounded corners squares them: %+v", a.palette)
	}
	// The trend's defaults reach the fleet at once.
	for range 2 {
		a.key(termui.KeyDown)
	}
	a.key(termui.KeyRight)
	if a.settings.TrendMetric != "req" || a.fleet.metric != 1 {
		t.Errorf("Fleet trend →: %q, fleet metric %d", a.settings.TrendMetric, a.fleet.metric)
	}
	// Esc saves what changed.
	a.key(termui.KeyEsc)
	if a.menu != nil || a.status.text != "settings saved" {
		t.Fatalf("Esc: menu %v, said %q", a.menu != nil, a.status.text)
	}
	saved, err := loadSettings(a.settingsPath)
	if err != nil || saved != a.settings || saved.Theme != "whiteout" || saved.Colors != "256" || saved.Rounded {
		t.Errorf("saved %+v, %v - the app has %+v", saved, err, a.settings)
	}
	// Keys changed meanwhile are saved with the rest next time.
	a.key("w")
	a.key("M")
	a.key(termui.KeyEsc)
	if saved, _ := loadSettings(a.settingsPath); saved.TrendWindow != "15m" {
		t.Errorf("w then the menu: window %q saved", saved.TrendWindow)
	}
	// Nothing changed: nothing written, nothing said.
	a.status = statusLine{}
	a.key("M")
	a.key("q")
	if a.menu != nil || a.status.text != "" {
		t.Errorf("q closes the menu, quietly when nothing changed: %q", a.status.text)
	}
	// The help says where the menu is.
	a.key("?")
	if !strings.Contains(strings.Join(a.render(120, 40).Lines(), "\n"), "options: theme, colours, graphs, defaults") {
		t.Error("the help lists M")
	}
}

func TestUseSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui.json")
	s := defaultSettings()
	s.Theme, s.Interval, s.TrendMetric, s.ProcSort, s.ProcReverse = "dracula", "10s", "cpu", "memory", true
	if err := s.save(path); err != nil {
		t.Fatal(err)
	}
	a := testApp(t)
	a.useSettings(path, false, "")
	if a.palette.Theme.Name != "dracula" || time.Duration(a.interval.Load()) != 10*time.Second {
		t.Errorf("saved: theme %s, interval %v", a.palette.Theme.Name, time.Duration(a.interval.Load()))
	}
	// The flags given for this run win.
	a = testApp(t)
	a.useSettings(path, true, "Nord")
	if a.palette.Theme.Name != "nord" || time.Duration(a.interval.Load()) != 2*time.Second {
		t.Errorf("-theme Nord -interval 2s: theme %s, interval %v", a.palette.Theme.Name, time.Duration(a.interval.Load()))
	}
	// New screens start as saved.
	fs := newFleetScreen()
	a.setupFleet(fs)
	if fs.metric != 4 || fs.trendOff {
		t.Errorf("fleet trend: metric %d, off %v", fs.metric, fs.trendOff)
	}
	n := a.newNode(testSampler(t, a, "lgslbpub01"), false)
	if procSorts[n.procSort] != "memory" || n.procDesc {
		t.Errorf("processes: by %s, descending %v", procSorts[n.procSort], n.procDesc)
	}
	// A broken file: the defaults, said in the footer.
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	a = testApp(t)
	a.useSettings(path, false, "")
	if a.palette.Theme != termui.ThemeJanus || !strings.HasPrefix(a.status.text, "settings: ") {
		t.Errorf("broken file: theme %v, said %q", a.palette.Theme.Name, a.status.text)
	}
}

func TestThemeCompletion(t *testing.T) {
	c := themeCandidates()
	if len(c) != len(termui.Themes())+1 || c[0].value != "janus" || c[len(c)-1].value != "list" {
		t.Errorf("theme candidates: %v", c)
	}
}
