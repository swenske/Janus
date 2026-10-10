package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/termui"
)

// janusctl tui is a live dashboard in the terminal: the node's CPU,
// memory, network, HAProxy, processes, services, events and logs, drawn
// every interval; with several nodes, the fleet first. It draws with
// internal/termui on /dev/tty in raw mode (as the picker does), on the
// alternate screen, and restores the terminal however it ends - never
// log.Fatal while raw. -once prints one frame as text instead, for
// scripts and tests.

// tuiOptions is what the dashboard knows about who runs it.
type tuiOptions struct {
	context string    // the context's name, "" with a node's own certificate
	user    string    // the account (the certificate's CN)
	role    string    // os:admin, os:operator, os:reader - "" unknown: everything is offered, the node decides
	expires time.Time // when the certificate ends; zero unknown
}

// tuiIntervals are the refresh periods + and - step through.
var tuiIntervals = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}

const tuiMinWidth, tuiMinHeight = 80, 24

// tuiFlags are janusctl tui's flags, and which ones were given.
type tuiFlags struct {
	interval time.Duration
	once     bool
	theme    string
	given    map[string]bool
}

func parseTUIFlags(args []string, handling flag.ErrorHandling) (tuiFlags, error) {
	var tf tuiFlags
	fs := flag.NewFlagSet("tui", handling)
	if handling == flag.ContinueOnError {
		fs.SetOutput(io.Discard)
	}
	fs.DurationVar(&tf.interval, "interval", 2*time.Second, "how often the nodes are asked (1s to 30s)")
	fs.BoolVar(&tf.once, "once", false, "print one frame as text and exit")
	fs.StringVar(&tf.theme, "theme", "", "the colour theme for this run (-theme list names them; M chooses and saves one)")
	if err := fs.Parse(args); err != nil {
		return tf, err
	}
	tf.given = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { tf.given[f.Name] = true })
	return tf, nil
}

// tuiThemesOnly answers what -theme needs no node for, before janusctl
// reaches one: -theme list prints the themes (true: done), an unknown
// name is refused. Anything else is left to runTUI.
func tuiThemesOnly(args []string, out io.Writer) bool {
	tf, err := parseTUIFlags(args, flag.ContinueOnError)
	switch {
	case err != nil:
		return false
	case tf.theme == "list":
		for _, t := range termui.Themes() {
			_, _ = fmt.Fprintf(out, "%-18s %s\n", t.Name, t.About)
		}
		return true
	case tf.theme != "" && termui.ThemeNamed(tf.theme) == nil:
		log.Fatalf("janusctl tui: no theme %q - %s", tf.theme, themeHelp())
	}
	return false
}

func runTUI(args []string, targets []tuiTarget, opts tuiOptions) {
	tf, _ := parseTUIFlags(args, flag.ExitOnError)
	interval, once, theme, given := &tf.interval, &tf.once, &tf.theme, tf.given
	if *interval < time.Second || *interval > 30*time.Second {
		log.Fatal("janusctl tui: -interval is between 1s and 30s")
	}
	if tuiThemesOnly(args, os.Stdout) {
		return
	}
	if *once {
		w, h := tuiMinWidth+40, tuiMinHeight+16
		if isTerminal(os.Stdout) {
			if tw, th, err := term.GetSize(int(os.Stdout.Fd())); err == nil && tw >= tuiMinWidth && th >= tuiMinHeight {
				w, h = tw, th
			}
		}
		f, err := snapshot(context.Background(), targets, opts, w, h)
		if err != nil {
			log.Fatalf("janusctl tui: %v", err)
		}
		if _, err := io.WriteString(os.Stdout, strings.Join(f.Lines(), "\n")+"\n"); err != nil {
			os.Exit(1) // a closed pipe: nothing to say
		}
		return
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stderr) || os.Getenv("TERM") == "dumb" {
		log.Fatal("janusctl tui: needs a terminal (janusctl tui -once prints one frame)")
	}
	a := newApp(targets, opts, *interval)
	a.useSettings(settingsPath(), given["interval"], *theme)
	t, err := openTerminal()
	if err != nil {
		log.Fatalf("janusctl tui: %v", err)
	}
	err = a.run(t)
	t.close()
	if err != nil {
		log.Fatalf("janusctl tui: %v", err)
	}
}

// roleFromTLS is the role a client certificate carries (its first os:*
// organization), "" when it has none.
func roleFromTLS(cfg *tls.Config) string {
	if cfg == nil || len(cfg.Certificates) == 0 || len(cfg.Certificates[0].Certificate) == 0 {
		return ""
	}
	return roleFromDER(cfg.Certificates[0].Certificate[0])
}

func roleFromDER(der []byte) string {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return ""
	}
	for _, o := range cert.Subject.Organization {
		if strings.HasPrefix(o, "os:") {
			return o
		}
	}
	return ""
}

// roleFromCertFile reads the role out of a PEM certificate file (the
// first CERTIFICATE block).
func roleFromCertFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return ""
		}
		if block.Type == "CERTIFICATE" {
			return roleFromDER(block.Bytes)
		}
	}
}

// roleFromAsRoles is the first of -as-roles, "" without any.
func roleFromAsRoles(asRoles string) string {
	first, _, _ := strings.Cut(asRoles, ",")
	return strings.TrimSpace(first)
}

// fleetTargets is every node of the context as a dashboard target, each
// dialled with the context's certificate and the node's own CA when
// its sampler first asks; and the certificate's role.
func fleetTargets(name string, ctx *cliContext, nodes []ctxNode) ([]tuiTarget, string, error) {
	var targets []tuiTarget
	role := ""
	for _, n := range nodes {
		tlsConfig, err := nodeTLS(name, ctx, n)
		if err != nil {
			return nil, "", err
		}
		role = firstOr(role, roleFromTLS(tlsConfig))
		address := n.Address
		targets = append(targets, tuiTarget{name: n.Name, address: address, dial: func() (*grpc.ClientConn, error) { return dialTLS(address, tlsConfig) }})
	}
	return targets, role, nil
}

// roleAllows reports whether role may call something that needs one of
// need: with an unknown role, everything is offered and the node
// answers.
func roleAllows(role string, need []string) bool {
	if role == "" {
		return true
	}
	for _, n := range need {
		if n == role {
			return true
		}
	}
	return false
}

var (
	rolesOperators = []string{pki.RoleAdmin, pki.RoleOperator}
	rolesAdmins    = []string{pki.RoleAdmin}
)

// terminal is /dev/tty in raw mode on the alternate screen.
type terminal struct {
	tty    *os.File
	state  *term.State
	keys   chan string
	resize chan os.Signal
	quit   chan os.Signal
	once   sync.Once
}

func openTerminal() (*terminal, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	state, err := term.MakeRaw(int(tty.Fd()))
	if err != nil {
		_ = tty.Close()
		return nil, err
	}
	t := &terminal{tty: tty, state: state, keys: make(chan string, 16), resize: make(chan os.Signal, 1), quit: make(chan os.Signal, 1)}
	// Alternate screen, cursor hidden, no autowrap (a miscounted width
	// would scroll), cleared.
	_, _ = io.WriteString(tty, "\x1b[?1049h\x1b[?25l\x1b[?7l\x1b[2J")
	signal.Notify(t.resize, syscall.SIGWINCH)
	signal.Notify(t.quit, syscall.SIGTERM, syscall.SIGHUP)
	go t.readKeys()
	return t, nil
}

func (t *terminal) readKeys() {
	buf := make([]byte, 256)
	for {
		n, err := t.tty.Read(buf)
		if err != nil {
			close(t.keys)
			return
		}
		for _, k := range termui.SplitKeys(string(buf[:n])) {
			t.keys <- k
		}
	}
}

func (t *terminal) size() (int, int) {
	w, h, err := term.GetSize(int(t.tty.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

// draw writes one frame's changes, as one synchronized update where
// the terminal supports it.
func (t *terminal) draw(s string) {
	if s == "" {
		return
	}
	_, _ = io.WriteString(t.tty, "\x1b[?2026h"+s+"\x1b[?2026l")
}

func (t *terminal) clear() { _, _ = io.WriteString(t.tty, "\x1b[2J") }

// close gives the terminal back: once, whatever path leads here.
func (t *terminal) close() {
	t.once.Do(func() {
		signal.Stop(t.resize)
		signal.Stop(t.quit)
		_, _ = io.WriteString(t.tty, "\x1b[0m\x1b[2J\x1b[?7h\x1b[?25h\x1b[?1049l")
		_ = term.Restore(int(t.tty.Fd()), t.state)
		_ = t.tty.Close()
	})
}

// screen is what fills the frame above the footer: the fleet or a node.
type screen interface {
	open(a *app)
	close(a *app)
	key(a *app, k string)
	render(a *app, f *termui.Frame)
	hints(a *app) []string
}

// statusLine is the last thing the dashboard said, shown for a while.
type statusLine struct {
	text string
	tone termui.Color
	at   time.Time
}

// app is the dashboard: its samplers, the current screen, overlays.
type app struct {
	targets  []tuiTarget
	samplers []*sampler
	opts     tuiOptions
	interval atomic.Int64
	paused   atomic.Bool
	pausedAt time.Time // when p paused the samplers
	wake     chan struct{}
	actions  chan actionResult
	palette  termui.Palette
	detected termui.Depth        // what the terminal says it takes
	graphs   termui.GraphSymbols // what graphs are drawn with
	repaint  bool                // the palette changed: every row again
	settings tuiSettings
	saved    tuiSettings // what tui.json holds
	// settingsPath is where the menu saves; "" never (tests, -once).
	settingsPath string
	now          func() time.Time

	screen screen
	fleet  *fleetScreen
	modal  *modal
	menu   *optionsMenu
	help   bool
	status statusLine
	cancel context.CancelFunc
}

func newApp(targets []tuiTarget, opts tuiOptions, interval time.Duration) *app {
	a := &app{targets: targets, opts: opts, wake: make(chan struct{}, 1), actions: make(chan actionResult, 4), detected: termui.DetectDepth(os.Getenv), now: time.Now, settings: defaultSettings(), saved: defaultSettings()}
	a.interval.Store(int64(interval))
	a.applySettings()
	for _, t := range targets {
		a.samplers = append(a.samplers, newSampler(t, a.wake, &a.interval, &a.paused))
	}
	return a
}

// run draws and reads keys until the user leaves or the terminal goes.
// The terminal is given back before a panic is let through, so its
// trace is readable.
func (a *app) run(t *terminal) (err error) {
	defer func() {
		if r := recover(); r != nil {
			t.close()
			panic(r)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	defer cancel()
	for _, s := range a.samplers {
		go s.run(ctx)
	}
	if len(a.samplers) > 1 {
		a.fleet = newFleetScreen()
		a.setupFleet(a.fleet)
		a.show(a.fleet)
	} else {
		a.show(a.newNode(a.samplers[0], false))
	}
	var prev *termui.Frame
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		w, h := t.size()
		f := a.render(w, h)
		if a.repaint {
			prev, a.repaint = nil, false
		}
		t.draw(f.Render(prev, a.palette))
		prev = f
		select {
		case k, ok := <-t.keys:
			if !ok {
				return nil
			}
			if a.key(k) {
				return nil
			}
		case <-t.resize:
			prev = nil
			t.clear()
		case <-t.quit:
			return nil
		case <-a.wake:
		case res := <-a.actions:
			a.finish(res)
		case <-tick.C:
		}
	}
}

// show makes s the current screen.
func (a *app) show(s screen) {
	if a.screen != nil {
		a.screen.close(a)
	}
	a.screen = s
	s.open(a)
}

// say puts a message in the footer.
func (a *app) say(text string, tone termui.Color) {
	a.status = statusLine{text: text, tone: tone, at: a.now()}
}

// key handles one key; true means leave.
func (a *app) key(k string) bool {
	k = termui.Normalize(k)
	if a.menu != nil {
		a.menu.key(a, k)
		return false
	}
	if a.modal != nil {
		a.modal.key(a, k)
		return false
	}
	if a.help {
		a.help = false
		return false
	}
	switch k {
	case "q", termui.KeyCtrlC:
		return true
	case "?":
		a.help = true
	case "M":
		a.openMenu()
	case "p":
		a.paused.Store(!a.paused.Load())
		if a.paused.Load() {
			a.pausedAt = a.now()
			a.say("paused - p resumes", termui.ColorWarn)
		} else {
			a.say("resumed", termui.ColorOK)
			a.kickAll()
		}
	case "+", "=":
		a.stepInterval(1)
	case "-", "_":
		a.stepInterval(-1)
	default:
		a.screen.key(a, k)
	}
	return false
}

func (a *app) stepInterval(dir int) {
	cur := time.Duration(a.interval.Load())
	i := 0
	for j, d := range tuiIntervals {
		if d <= cur {
			i = j
		}
	}
	i = min(max(i+dir, 0), len(tuiIntervals)-1)
	a.interval.Store(int64(tuiIntervals[i]))
	a.say("every "+tuiIntervals[i].String(), termui.ColorDefault)
	a.kickAll()
}

func (a *app) kickAll() {
	for _, s := range a.samplers {
		s.Kick()
	}
}

// render is the whole frame: the screen above a footer, then the help
// or a modal over it.
func (a *app) render(w, h int) *termui.Frame {
	f := termui.NewFrame(w, h)
	f.Graph = a.graphs
	if w < tuiMinWidth || h < tuiMinHeight {
		msg := fmt.Sprintf("janusctl tui needs %dx%d (this terminal is %dx%d)", tuiMinWidth, tuiMinHeight, w, h)
		f.Text(max((w-len(msg))/2, 0), h/2, msg, termui.Style{FG: termui.ColorWarn}, 0)
		return f
	}
	a.screen.render(a, f)
	a.renderFooter(f)
	if a.help {
		a.renderHelp(f)
	}
	if a.modal != nil {
		a.modal.render(f)
	}
	if a.menu != nil {
		a.menu.render(a, f)
	}
	return f
}

// renderFooter is the last row: the keys, then the interval, the
// certificate's end and the last message.
func (a *app) renderFooter(f *termui.Frame) {
	y := f.H - 1
	muted, key := styleMuted, styleKey
	right := "every " + time.Duration(a.interval.Load()).String()
	if a.paused.Load() {
		right = "paused"
	}
	if !a.opts.expires.IsZero() {
		left := time.Until(a.opts.expires)
		right += "  cert ends " + a.opts.expires.Local().Format("15:04")
		if left < 10*time.Minute {
			right += " !"
		}
	}
	x2 := f.W - 1
	x2 -= f.TextRight(0, x2, y, right, muted) + 1
	if a.status.text != "" && a.now().Sub(a.status.at) < 6*time.Second {
		f.Text(1, y, termui.Truncate(a.status.text, x2-1), termui.Style{FG: a.status.tone, Bold: a.status.tone != termui.ColorDefault}, x2-1)
		return
	}
	x := 1
	for _, hint := range a.screen.hints(a) {
		k, desc, _ := strings.Cut(hint, " ")
		if x+len(k)+len(desc)+3 > x2 {
			break
		}
		x += f.Text(x, y, k, key, 0)
		x += f.Text(x, y, " "+desc, muted, 0) + 2
	}
}

var tuiHelp = []string{
	"q  Ctrl-C      leave",
	"?              this help",
	"M              options: theme, colours, graphs, defaults",
	"Tab  Shift-Tab focus the next / previous panel",
	"↑ ↓ PgUp PgDn  move in the focused panel",
	"Enter          open the node / act on the server",
	"Esc            back to the fleet, close a dialog",
	"1-9            show or hide a panel (7 events, 8 logs)",
	"s  r           sort the processes or the nodes / reverse",
	"m  w  y        fleet trend: metric, time window, scale",
	"e  l           events / logs in the tail (l again: janusd ↔ haproxy)",
	"p              pause",
	"+  -           refresh faster / slower",
	"R  B  J  C     reload HAProxy, reboot, restart janusd, confirm a trial",
}

func (a *app) renderHelp(f *termui.Frame) {
	w := 0
	for _, l := range tuiHelp {
		w = max(w, len([]rune(l)))
	}
	w += 4
	h := len(tuiHelp) + 2
	r := termui.Rect{X: (f.W - w) / 2, Y: (f.H - h) / 2, W: w, H: h}
	f.Fill(r, ' ', termui.Style{})
	in := f.Box(r, "Keys", styleFocus, styleTitle)
	for i, l := range tuiHelp {
		k, desc, _ := strings.Cut(l, "  ")
		x := f.Text(in.X+1, in.Y+i, k, styleKey, 0)
		f.Text(in.X+1+x+2, in.Y+i, strings.TrimSpace(desc), termui.Style{}, 0)
	}
}

// snapshot is -once: each node asked twice a second apart (rates need
// two samples), the tails read once, one frame drawn at w×h.
func snapshot(ctx context.Context, targets []tuiTarget, opts tuiOptions, w, h int) (*termui.Frame, error) {
	a := newApp(targets, opts, time.Second)
	a.palette = termui.Palette{Depth: termui.DepthNone}
	for _, s := range a.samplers {
		conn, err := s.connection()
		if err != nil {
			s.rounds++
			continue
		}
		s.full.Store(len(targets) == 1)
		s.round(ctx)
		if s.cur != nil && s.cur.sys != nil {
			time.Sleep(time.Second)
			s.round(ctx)
		}
		_ = conn
	}
	if len(a.samplers) == 1 {
		s := a.samplers[0]
		v := s.view()
		if !v.reachable {
			return nil, errors.New(v.name + ": " + firstOr(v.err, "no answer"))
		}
		n := newNodeScreen(s, false)
		if conn, err := s.connection(); err == nil {
			readOnce(ctx, conn, n.events, n.logs, n.logID)
		}
		a.screen = n
	} else {
		a.fleet = newFleetScreen()
		a.screen = a.fleet
	}
	return a.render(w, h), nil
}

// readOnce fills the tails for a snapshot: the events held, the last
// lines of a log.
func readOnce(ctx context.Context, conn *grpc.ClientConn, events, logs *tail, logID string) {
	sys := janusv1alpha1.NewSystemServiceClient(conn)
	ectx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	if stream, err := sys.Events(ectx, &janusv1alpha1.EventsRequest{}); err == nil {
		if err := readEvents(stream, events); status.Code(err) == codes.PermissionDenied {
			events.setErr("events: " + status.Convert(err).Message())
		}
	}
	lctx, lcancel := context.WithTimeout(ctx, tuiFetchTimeout)
	defer lcancel()
	stream, err := sys.Logs(lctx, &janusv1alpha1.LogsRequest{Id: logID, TailLines: 40})
	if err == nil {
		err = readLogs(stream, logs)
	}
	switch status.Code(err) {
	case codes.OK:
	case codes.PermissionDenied:
		logs.setErr("logs are for operators: " + status.Convert(err).Message())
	default:
		if !errors.Is(err, io.EOF) {
			logs.setErr("logs: " + status.Convert(err).Message())
		}
	}
}

func firstOr(s, alt string) string {
	if s != "" {
		return s
	}
	return alt
}

// Formatting shared by the screens.

func fmtPct(v float64) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", v)
}

func fmtRate(bytesPerSecond float64) string {
	if math.IsNaN(bytesPerSecond) {
		return "-"
	}
	return humanBytes(uint64(bytesPerSecond)) + "/s"
}

func fmtFloat(v float64, digits int) string {
	if math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.*f", digits, v)
}

// humanDuration is a duration in its two largest units: 3d 4h, 4h 12m,
// 12m 03s, 45s.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d.Seconds())
	days, s := s/86400, s%86400
	hours, s := s/3600, s%3600
	mins, secs := s/60, s%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %02dm", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dm %02ds", mins, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

// toneOf is the colour of a state word, as janusctl's tables colour them.
func toneOf(state string) termui.Style {
	s := strings.ToUpper(strings.TrimSpace(state))
	switch {
	case s == "", strings.HasPrefix(s, "NO CHECK"):
		return termui.Style{FG: termui.ColorMuted}
	case strings.HasPrefix(s, "UP"), s == "OPEN", goodState.MatchString(state):
		return termui.Style{FG: termui.ColorOK}
	case strings.HasPrefix(s, "DOWN"), s == "STOP", badState.MatchString(state):
		return termui.Style{FG: termui.ColorDanger}
	case strings.HasPrefix(s, "MAINT"), strings.HasPrefix(s, "DRAIN"), s == "NOLB", s == "FULL", waitState.MatchString(state):
		return termui.Style{FG: termui.ColorWarn}
	}
	return termui.Style{}
}
