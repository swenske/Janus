// Package bgp manages the bird extension (docs/bgp.md): its bird.conf -
// checked by BIRD itself, saved on STATE, put where the bird service
// janusd supervises reads it, reloaded over BIRD's control socket - its
// protocols' state, read over that socket, and the HAProxy gate: the
// protocols named haproxy_* are kept down while this node's HAProxy
// doesn't answer (or the cluster member is drained), so the routes they
// carry (an anycast address) leave the network until it answers again.
//
// The gate holds in the copy of bird.conf BIRD reads too: while it is
// closed, that copy disables the haproxy_* protocols (HoldDown), so a
// BIRD that starts - at boot, after a crash - or is reconfigured never
// brings them up, not even for the moment before janusd's next look.
package bgp

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/modcfg"
)

// Where BIRD and its files are - vars, so tests can move them.
var (
	Binary = "/usr/local/sbin/bird"
	// RunDir is BIRD's own: its configuration, control socket and pid
	// file (the extension's manifest names these paths).
	RunDir = "/run/janus/bird"
)

// ServiceID is the extension's service.
const ServiceID = "bird"

// GatePrefix names the protocols janusd keeps down while HAProxy doesn't
// answer.
const GatePrefix = "haproxy_"

// SocketPath is BIRD's control socket.
func SocketPath() string { return filepath.Join(RunDir, "bird.ctl") }

// Services is what Manager needs of janusd's extension supervisor.
type Services interface {
	StartService(id string) error
	StopService(id string) error
	State(id string) (extensions.ServiceState, error)
}

// Manager manages BIRD's configuration, reads its state and gates the
// haproxy_* protocols.
type Manager struct {
	svc Services
	mod *modcfg.Module

	// mu guards the gate's state, the copy of bird.conf BIRD reads and
	// the commands that change BIRD's protocols.
	mu      sync.Mutex
	held    map[string]bool // haproxy_* protocols janusd keeps down
	healthy bool            // the gate is open
	// gated: the haproxy_* protocols the copy BIRD reads disables - the
	// last one written.
	gated []string
}

func New(svc Services) *Manager {
	m := &Manager{svc: svc, held: map[string]bool{}, healthy: true}
	m.mod = &modcfg.Module{
		Name: "bird", Binary: Binary, File: "bird.conf", RunDir: RunDir,
		CheckArgs: func(path string) []string { return []string{"-p", "-c", path} },
		// Called with mu held (Boot, Apply, the gate).
		Runtime: func(config string) string {
			m.gated = nil
			if !m.healthy {
				config, m.gated = HoldDown(config)
			}
			return WithTimeFormat(WithLog(config))
		},
	}
	return m
}

var (
	comments     = regexp.MustCompile(`(?s)/\*.*?\*/|#[^\n]*`)
	logStatement = regexp.MustCompile(`(?m)^\s*log\s`)
)

// WithLog appends "log stderr all;" to a configuration that has no log
// statement: BIRD would log to syslog, which a node doesn't have - on
// stderr, its messages are the service's log. Appended, so line numbers
// stay those of the operator's text.
func WithLog(config string) string {
	if logStatement.MatchString(comments.ReplaceAllString(config, "")) {
		return config
	}
	if !strings.HasSuffix(config, "\n") {
		config += "\n"
	}
	return config + "log stderr all; # added by janusd: bird.conf has no log statement\n"
}

var timeFormatStatement = regexp.MustCompile(`(?m)^\s*timeformat\s+protocol\b`)

// WithTimeFormat appends "timeformat protocol iso long;" to a
// configuration that doesn't choose the protocols' time format: BIRD's
// default prints a time of day, then a date alone after 20 hours, and
// janus_bgp_protocol_state_since_timestamp_seconds wants the moment
// itself ("2026-10-29 01:41:34"). Appended like WithLog's statement.
func WithTimeFormat(config string) string {
	if timeFormatStatement.MatchString(comments.ReplaceAllString(config, "")) {
		return config
	}
	if !strings.HasSuffix(config, "\n") {
		config += "\n"
	}
	return config + "timeformat protocol iso long; # added by janusd: exact times in show protocols\n"
}

// Available reports whether BIRD is in the image.
func (m *Manager) Available() bool { return m.mod.Available() }

// Saved is the saved bird.conf; isDefault means there's none.
func (m *Manager) Saved() (string, bool, error) { return m.mod.Saved() }

// Check has BIRD check config (bird -p) - and the copy it reads while
// the gate is closed, the haproxy_* protocols disabled in it.
func (m *Manager) Check(config string) ([]string, error) {
	if errs, err := m.mod.Check(config); err != nil {
		return errs, err
	}
	if held, names := HoldDown(config); len(names) > 0 {
		if errs, err := m.mod.Check(held); err != nil {
			return append([]string{"BIRD refuses this bird.conf with its haproxy_* protocols disabled, as janusd hands it over while HAProxy doesn't answer:"}, errs...), err
		}
	}
	return nil, nil
}

// Boot puts the saved configuration where BIRD reads it - the service
// waits for that file. open is the gate: HAProxy answers and the node
// isn't a drained cluster member. Closed, the copy disables the haproxy_*
// protocols: BIRD starts with them down, and KeepGate brings them up once
// the gate opens.
func (m *Manager) Boot(open bool) error {
	if !m.Available() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.healthy = open
	if err := m.writeRuntime(); err != nil {
		return err
	}
	if len(m.gated) > 0 {
		log.Printf("bgp: BIRD starts with %s down - HAProxy doesn't answer, or this cluster member is drained", strings.Join(m.gated, ", "))
	}
	return nil
}

// writeRuntime writes the copy of the saved bird.conf BIRD reads, as the
// gate is; the haproxy_* protocols it disables are held. Called with mu
// held.
func (m *Manager) writeRuntime() error {
	m.gated = nil
	if err := m.mod.Boot(); err != nil {
		return err
	}
	m.holdGated()
	return nil
}

// holdGated counts the haproxy_* protocols the copy BIRD reads disables
// among those janusd keeps down. Called with mu held.
func (m *Manager) holdGated() {
	for _, n := range m.gated {
		m.held[n] = true
	}
}

// Apply checks, saves and applies config: BIRD reconfigures (or starts
// with it). An empty config removes it, and stops BIRD. The copy BIRD
// reads is the gate's: while it is closed, a reconfiguration keeps the
// haproxy_* protocols down - new ones are added down.
func (m *Manager) Apply(config string) ([]string, error) {
	empty := strings.TrimSpace(config) == ""
	if !empty {
		if errs, err := m.Check(config); err != nil {
			return errs, err
		}
	}
	m.mu.Lock()
	m.gated = nil
	if err := m.mod.Save(config); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if empty {
		m.held = map[string]bool{}
		m.mu.Unlock()
		return nil, m.svc.StopService(ServiceID)
	}
	defer m.mu.Unlock()
	m.holdGated()
	st, err := m.svc.State(ServiceID)
	if err != nil {
		return nil, err
	}
	switch st.State {
	case "running":
		if _, err := Query("configure"); err != nil {
			return []string{err.Error()}, err
		}
		// BIRD's protocols are now as the copy says: the haproxy_* ones
		// down while the gate is closed, each other as bird.conf says
		// (BIRD 2 reconfigures a protocol in place only when its
		// "disabled" is what it runs with, else it restarts it so).
		m.held = map[string]bool{}
		m.holdGated()
		m.syncGate()
		return nil, nil
	case "stopped":
		return nil, m.svc.StartService(ServiceID)
	}
	return nil, nil // waiting for the file it now has, or restarting: it reads it next
}

// Line is one line of a control socket reply: BIRD's code and its text.
type Line struct {
	Code int
	Text string
}

// ErrBIRD is a command BIRD refused (codes 8000-9999).
type ErrBIRD struct{ Lines []Line }

func (e *ErrBIRD) Error() string {
	var texts []string
	for _, l := range e.Lines {
		texts = append(texts, l.Text)
	}
	return "bird: " + strings.Join(texts, "; ")
}

// Query sends one command over BIRD's control socket and returns its
// reply. A refused command is an *ErrBIRD.
func Query(cmd string) ([]Line, error) {
	if strings.ContainsAny(cmd, "\r\n") {
		return nil, errors.New("a BIRD command is one line")
	}
	conn, err := net.DialTimeout("unix", SocketPath(), 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", extensions.ErrNotRunning, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	if _, err := readReply(r); err != nil { // "0001 BIRD x.y ready."
		return nil, fmt.Errorf("bird's control socket: %w", err)
	}
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return nil, err
	}
	lines, err := readReply(r)
	if err != nil {
		return nil, fmt.Errorf("bird's control socket: %w", err)
	}
	if last := lines[len(lines)-1]; last.Code >= 8000 {
		return nil, &ErrBIRD{Lines: lines}
	}
	return lines, nil
}

// readReply reads until the reply's last line: four digits and a space.
// "1002-text" continues a reply with code 1002; " text" continues the
// previous line's code.
func readReply(r *bufio.Reader) ([]Line, error) {
	var lines []Line
	code := 0
	for {
		s, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		s = strings.TrimRight(s, "\n")
		if len(s) >= 5 && isDigits(s[:4]) && (s[4] == '-' || s[4] == ' ') {
			code, _ = strconv.Atoi(s[:4])
			lines = append(lines, Line{Code: code, Text: s[5:]})
			if s[4] == ' ' {
				return lines, nil
			}
			continue
		}
		if len(s) == 4 && isDigits(s) {
			code, _ = strconv.Atoi(s)
			return append(lines, Line{Code: code}), nil
		}
		lines = append(lines, Line{Code: code, Text: strings.TrimPrefix(s, " ")})
	}
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// Status is BIRD's state.
type Status struct {
	Version   string
	RouterID  string
	Protocols []Protocol
}

// Protocol is one protocol of bird.conf.
type Protocol struct {
	Name, Proto, Table string
	State              string // "up", "down", "start", "stop"
	Since              string // as BIRD prints it
	Info               string // e.g. "Established", "Active Socket: Connection refused"
	// BGP's.
	BGPState        string
	NeighborAddress string
	NeighborAS      uint32
	LocalAS         uint32
	LastError       string
	Channels        []Channel
	// HeldDown: janusd keeps it down - HAProxy doesn't answer.
	HeldDown bool
}

// Channel is a protocol's routes for one address family.
type Channel struct {
	Name      string // "ipv4", "ipv6"...
	State     string
	Imported  uint32
	Exported  uint32
	Preferred uint32
	// Counted: BIRD printed the channel's routes - not for a channel
	// that's down, whose counts above are then 0 by default.
	Counted bool
	// Filtered is set only when BIRD counts it: a channel with "import
	// keep filtered on".
	Filtered *uint32
	// Changes is BIRD's "Route change stats" table, one cell each - only
	// the cells it counts ("---" is left out).
	Changes []RouteChange
}

// RouteChange is one cell of a channel's "Route change stats": the
// updates or withdraws BIRD received, rejected, filtered, ignored or
// accepted, on import or export, since the channel came up.
type RouteChange struct {
	Direction string // "import", "export"
	Kind      string // "updates", "withdraws"
	Outcome   string // "received", "rejected", "filtered", "ignored", "accepted"
	Count     uint64
}

// Status reads BIRD's state over its control socket.
func (m *Manager) Status() (*Status, error) {
	lines, err := Query("show status")
	if err != nil {
		return nil, err
	}
	st := &Status{}
	for _, l := range lines {
		switch {
		case l.Code == 1000:
			st.Version = strings.TrimPrefix(l.Text, "BIRD ")
		case strings.HasPrefix(l.Text, "Router ID is "):
			st.RouterID = strings.TrimPrefix(l.Text, "Router ID is ")
		}
	}
	lines, err = Query("show protocols all")
	if err != nil {
		return nil, err
	}
	st.Protocols = ParseProtocols(lines)
	m.mu.Lock()
	for i := range st.Protocols {
		st.Protocols[i].HeldDown = m.held[st.Protocols[i].Name]
	}
	m.mu.Unlock()
	return st, nil
}

var (
	dateField = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	changeRow = regexp.MustCompile(`^(Import|Export) (updates|withdraws)$`)
)

// SinceTime is when the protocol last changed state, from Since: exact
// with janusd's "timeformat protocol iso long" (a date and a time of
// day), or a time of day alone (BIRD's default within 20 hours: today's,
// else yesterday's); ok is false for a date alone (BIRD's default past
// 20 hours - a day isn't a time) or anything else. BIRD prints its local
// time, which janusd shares (now's location).
func (p Protocol) SinceTime(now time.Time) (time.Time, bool) {
	loc := now.Location()
	for _, layout := range []string{"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, p.Since, loc); err == nil {
			return t, true
		}
	}
	for _, layout := range []string{"15:04:05.999999999", "15:04:05"} {
		clock, err := time.ParseInLocation(layout, p.Since, loc)
		if err != nil {
			continue
		}
		y, m, d := now.Date()
		t := time.Date(y, m, d, clock.Hour(), clock.Minute(), clock.Second(), clock.Nanosecond(), loc)
		if t.After(now.Add(time.Minute)) {
			t = t.AddDate(0, 0, -1)
		}
		return t, true
	}
	return time.Time{}, false
}

// ParseProtocols reads "show protocols" or "show protocols all".
func ParseProtocols(lines []Line) []Protocol {
	var out []Protocol
	var p *Protocol
	var ch *Channel
	var outcomes []string // the current channel's "Route change stats" columns
	for _, l := range lines {
		switch l.Code {
		case 1002: // Name Proto Table State Since Info
			f := strings.Fields(l.Text)
			if len(f) < 4 {
				continue
			}
			out = append(out, Protocol{Name: f[0], Proto: f[1], Table: f[2], State: f[3]})
			p, ch, outcomes = &out[len(out)-1], nil, nil
			rest := f[4:]
			if len(rest) > 0 {
				p.Since = rest[0]
				rest = rest[1:]
				// An older date prints as "YYYY-MM-DD hh:mm:ss".
				if dateField.MatchString(p.Since) && len(rest) > 0 && strings.Contains(rest[0], ":") {
					p.Since += " " + rest[0]
					rest = rest[1:]
				}
			}
			p.Info = strings.Join(rest, " ")
		case 1006:
			if p == nil {
				continue
			}
			key, value, _ := strings.Cut(strings.TrimSpace(l.Text), ":")
			value = strings.TrimSpace(value)
			switch {
			case strings.HasPrefix(key, "Channel "):
				p.Channels = append(p.Channels, Channel{Name: strings.TrimPrefix(key, "Channel ")})
				ch, outcomes = &p.Channels[len(p.Channels)-1], nil
			case key == "BGP state":
				p.BGPState = value
			case key == "Neighbor address":
				p.NeighborAddress = value
			case key == "Neighbor AS":
				p.NeighborAS = parseUint32(value)
			case key == "Local AS":
				p.LocalAS = parseUint32(value)
			case key == "Last error":
				p.LastError = value
			case key == "State" && ch != nil:
				ch.State = value
			case key == "Routes" && ch != nil:
				// "2 imported, 0 exported, 2 preferred" - and "1 filtered"
				// after imported with "import keep filtered on".
				ch.Counted = true
				for _, part := range strings.Split(value, ",") {
					n, what, _ := strings.Cut(strings.TrimSpace(part), " ")
					switch what {
					case "imported":
						ch.Imported = parseUint32(n)
					case "exported":
						ch.Exported = parseUint32(n)
					case "preferred":
						ch.Preferred = parseUint32(n)
					case "filtered":
						f := parseUint32(n)
						ch.Filtered = &f
					}
				}
			case key == "Route change stats" && ch != nil:
				// The table's header: "received rejected filtered ignored accepted".
				outcomes = strings.Fields(value)
			case ch != nil && outcomes != nil && changeRow.MatchString(key):
				// "Import updates:  3  0  1  0  2" - "---" where BIRD
				// doesn't count.
				dir, kind, _ := strings.Cut(strings.ToLower(key), " ")
				for i, v := range strings.Fields(value) {
					n, err := strconv.ParseUint(v, 10, 64)
					if err != nil || i >= len(outcomes) {
						continue
					}
					ch.Changes = append(ch.Changes, RouteChange{Direction: dir, Kind: kind, Outcome: outcomes[i], Count: n})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func parseUint32(s string) uint32 {
	n, _ := strconv.ParseUint(strings.Fields(s + " ")[0], 10, 32)
	return uint32(n)
}

// HAProxyHealthy is what the gate last saw.
func (m *Manager) HAProxyHealthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.healthy
}

// Held lists the protocols janusd keeps down.
func (m *Manager) Held() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for n := range m.held {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// KeepGate keeps the haproxy_* protocols down while healthy says HAProxy
// doesn't answer - checked every interval, and at once whenever the
// channel changed returns is closed (nil: polling only) - until stop is
// closed.
func (m *Manager) KeepGate(healthy func() bool, changed func() <-chan struct{}, interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		var wake <-chan struct{}
		if changed != nil {
			wake = changed() // before checking: a change in between still wakes us
		}
		h := healthy()
		m.mu.Lock()
		if h != m.healthy {
			log.Printf("bgp: HAProxy %s", map[bool]string{true: "answers again - the haproxy_* protocols come back", false: "doesn't answer - the haproxy_* protocols go down"}[h])
			m.healthy = h
			m.follow()
		}
		m.syncGate()
		m.mu.Unlock()
		select {
		case <-stop:
			return
		case <-t.C:
		case <-wake:
		}
	}
}

// follow puts a change of the gate in the copy of bird.conf BIRD reads,
// so that a BIRD started or reconfigured from now on has its haproxy_*
// protocols as the gate says. Opened, BIRD reconfigures from that copy:
// the protocols it had down - disabled in the copy it started with, or
// by syncGate - come up as bird.conf says (one bird.conf itself disables
// stays down). syncGate does the rest - and all of it, the CLI way, when
// the copy can't be written or BIRD doesn't reconfigure. Called with mu
// held.
func (m *Manager) follow() {
	if err := m.writeRuntime(); err != nil {
		log.Printf("bgp: %v - BIRD's copy of bird.conf doesn't follow HAProxy's health", err)
		return
	}
	if !m.healthy {
		return
	}
	if _, err := Query("configure"); err != nil {
		// Not running yet, it may still have read the copy before this
		// one: syncGate enables the held ones once BIRD answers.
		if !errors.Is(err, extensions.ErrNotRunning) {
			log.Printf("bgp: configure: %v", err)
		}
		return
	}
	m.held = map[string]bool{}
}

// syncGate disables the haproxy_* protocols that aren't down while
// HAProxy doesn't answer, and enables those janusd disabled once it
// answers. A protocol bird.conf itself disables stays as it is. Called
// with mu held.
func (m *Manager) syncGate() {
	lines, err := Query("show protocols")
	if err != nil {
		return // not running: nothing to gate
	}
	protocols := ParseProtocols(lines)
	exists := map[string]bool{}
	for _, p := range protocols {
		exists[p.Name] = true
	}
	for n := range m.held {
		if !exists[n] { // gone from bird.conf
			delete(m.held, n)
		}
	}
	for _, p := range protocols {
		if !strings.HasPrefix(p.Name, GatePrefix) {
			continue
		}
		switch {
		case !m.healthy && p.State != "down":
			if _, err := Query("disable " + p.Name); err != nil {
				log.Printf("bgp: disable %s: %v", p.Name, err)
				continue
			}
			if !m.held[p.Name] {
				log.Printf("bgp: %s held down - HAProxy doesn't answer", p.Name)
			}
			m.held[p.Name] = true
		case m.healthy && m.held[p.Name]:
			if _, err := Query("enable " + p.Name); err != nil {
				log.Printf("bgp: enable %s: %v", p.Name, err)
				continue
			}
			log.Printf("bgp: %s back up - HAProxy answers", p.Name)
			delete(m.held, p.Name)
		}
	}
}
