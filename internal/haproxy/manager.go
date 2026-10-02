// Package haproxy manages a single supervised HAProxy process: validating
// and applying config (via `haproxy -c`), starting it, and reloading it
// seamlessly (`-sf <old pid>`) when the config changes. It deliberately
// does not reimplement HAProxy's own metrics - see docs/architecture.md:
// HAProxy's built-in Prometheus exporter stays the source of truth for
// HAProxy metrics, this package only proxies the runtime (stats) socket
// for HAProxyService's other RPCs (ShowInfo, ServerSetState, ...).
package haproxy

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/swenske/Janus/internal/events"
)

// Manager supervises one HAProxy process instance.
type Manager struct {
	BinaryPath      string
	ConfigPath      string
	PidPath         string
	StatsSocketPath string

	// Output, if set, also receives HAProxy's stdout/stderr (alongside
	// janusd's own), for SystemService.Logs.
	Output io.Writer

	// CertStoreDir, if set, keeps the certificates CertificateUpload
	// loads at runtime across reloads and restarts (certstore.go).
	CertStoreDir string
	certMu       sync.Mutex

	// DrainDelay is how long Stop waits between declaring HAProxy not
	// serving and closing its listeners: what follows Serving and Changed
	// (keepalived's track file, BIRD's haproxy_* protocols) moves the
	// traffic to another node meanwhile, and this node still answers
	// whatever arrives before it has moved. Zero: no wait.
	DrainDelay time.Duration

	// Env, if set, adds to HAProxy's environment - for `haproxy -c` too:
	// a configuration can use these variables. Read at each start and
	// validation.
	Env func() []string

	// txMu serializes runtime certificate transactions: HAProxy has one
	// open at a time.
	txMu sync.Mutex

	mu  sync.Mutex
	cur *process
	// serving: a process runs that janusd isn't stopping. During a soft
	// stop the stats socket still answers, but the listeners are closed.
	serving atomic.Bool

	changeMu sync.Mutex
	changed  chan struct{}

	counters Counters
}

// Counters are what the Manager has done since janusd started, for the
// node's exporter.
type Counters struct {
	Starts          atomic.Uint64 // processes started, reloads included
	Reloads         atomic.Uint64 // seamless reloads (-sf)
	UnexpectedExits atomic.Uint64 // a process that exited without being stopped or replaced
	ApplyAccepted   atomic.Uint64
	ApplyRejected   atomic.Uint64
	LastApplyUnix   atomic.Int64 // last accepted Apply
}

// Counters returns the Manager's counters.
func (m *Manager) Counters() *Counters { return &m.counters }

// process is one started haproxy; done closes once it has exited and
// been reaped.
type process struct {
	cmd      *exec.Cmd
	started  time.Time
	done     chan struct{}
	err      error
	stopping bool // set by Stop, under Manager.mu
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func NewManager(binaryPath, configPath, pidPath, statsSocketPath string) *Manager {
	return &Manager{
		BinaryPath:      binaryPath,
		ConfigPath:      configPath,
		PidPath:         pidPath,
		StatsSocketPath: statsSocketPath,
	}
}

// Validate runs `haproxy -c -f <tmpfile>` against cfg without touching the
// running process or ConfigPath. Returns (true, nil) if valid, or
// (false, <haproxy's own error lines>) otherwise.
func (m *Manager) Validate(cfg []byte) (bool, []string) {
	tmp, err := os.CreateTemp("", "janus-validate-*.cfg")
	if err != nil {
		return false, []string{err.Error()}
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(cfg); err != nil {
		tmp.Close()
		return false, []string{err.Error()}
	}
	tmp.Close()

	check := exec.Command(m.BinaryPath, "-c", "-f", tmp.Name())
	check.Env = m.env()
	out, err := check.CombinedOutput()
	if err != nil {
		errs := splitNonEmptyLines(string(out))
		if len(errs) == 0 {
			// haproxy exited nonzero but printed nothing this run captured
			// (e.g. it never even started - a bad BinaryPath, a denied
			// exec) - report *why* the command failed rather than
			// silently returning an empty error list, which callers
			// (janusctl, the dashboard's ApplyConfig relay) would
			// otherwise render as "rejected" with no explanation at all.
			errs = []string{err.Error()}
		}
		return false, errs
	}
	return true, nil
}

// Apply validates cfg, writes it to ConfigPath on success, and
// starts/reloads the process. Returns the validation errors (config
// untouched, process untouched) if cfg is invalid.
func (m *Manager) Apply(cfg []byte) ([]string, error) {
	if ok, errs := m.Validate(cfg); !ok {
		m.counters.ApplyRejected.Add(1)
		return errs, fmt.Errorf("invalid config")
	}
	m.counters.ApplyAccepted.Add(1)
	m.counters.LastApplyUnix.Store(time.Now().Unix())
	if err := os.WriteFile(m.ConfigPath, cfg, 0o644); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	// ConfigPath may be the Phase 3 cont'd persistent STATE partition
	// (see rootfs/init/main.go's mountState) - force it to the
	// underlying block device now, same reasoning as cmd/janusd's
	// PKI bootstrap: don't let an applied config's durability depend on
	// some later, unrelated sync happening to occur first.
	syscall.Sync()
	return nil, m.startOrReload(true)
}

// Reload re-applies whatever is currently on disk at ConfigPath - used
// when the config file hasn't changed but a restart is still wanted (or
// as the second half of Apply). It returns once the new process answers
// on the stats socket, ready for runtime commands.
func (m *Manager) Reload() error {
	return m.startOrReload(true)
}

// Boot starts HAProxy when janusd starts. Unlike Reload, a first start
// doesn't wait for the process to answer: janusd goes on to its PKI
// meanwhile.
func (m *Manager) Boot() error {
	return m.startOrReload(false)
}

// startOrReload starts haproxy if it isn't running yet, or performs a
// seamless reload (-sf <old pid>) if it already is. Foreground, no -D:
// the caller (janusd, itself supervised by rootfs/init) is
// responsible for treating this as a managed child process, not letting
// HAProxy detach on its own.
func (m *Manager) startOrReload(wait bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	args := []string{"-f", m.ConfigPath}
	reload := false
	if old := m.previousPID(); old > 0 {
		args = append(args, "-sf", strconv.Itoa(old))
		reload = true
	}

	cmd := exec.Command(m.BinaryPath, args...)
	cmd.Env = m.env()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if m.Output != nil {
		cmd.Stdout = io.MultiWriter(os.Stdout, m.Output)
		cmd.Stderr = io.MultiWriter(os.Stderr, m.Output)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start haproxy: %w", err)
	}
	m.counters.Starts.Add(1)
	if reload {
		m.counters.Reloads.Add(1)
	}
	// Every started process is waited on, including the ones a later
	// seamless reload (-sf) replaces - otherwise each would stay a zombie
	// under janusd for as long as it runs.
	p := &process{cmd: cmd, started: time.Now(), done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done) // before taking mu: Stop holds it while waiting on done
		m.mu.Lock()
		reason := "exited on its own"
		switch {
		case p.stopping:
			reason = "stopped"
		case m.cur != p:
			reason = "replaced by a reload"
		default:
			m.counters.UnexpectedExits.Add(1)
		}
		m.mu.Unlock()
		if reason == "exited on its own" {
			m.notifyChanged() // the health checks needn't wait for their next poll
		}
		exit := ""
		if p.err != nil {
			exit = p.err.Error()
		}
		events.Publish("haproxy.exited", map[string]any{"pid": cmd.Process.Pid, "reason": reason, "exit": exit})
	}()
	m.cur = p
	if !m.serving.Swap(true) {
		m.notifyChanged()
	}
	// HAProxy itself only writes its -p pid file in daemon or
	// master-worker mode, never in the foreground mode used here - so
	// janusd writes it, for a restarted janusd to find the running
	// haproxy and take it over with -sf.
	if err := os.WriteFile(m.PidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		log.Printf("haproxy: write %s: %v", m.PidPath, err)
	}
	events.Publish("haproxy.started", map[string]any{"pid": cmd.Process.Pid, "args": args})
	if reload || wait {
		m.waitAnswering(p)
	}
	m.restoreCerts(p)
	return nil
}

// env is HAProxy's environment: janusd's, plus Env's.
func (m *Manager) env() []string {
	if m.Env == nil {
		return nil // inherit
	}
	return append(os.Environ(), m.Env()...)
}

// StartedAt is when the current HAProxy process started - zero if none
// runs. A file created since then isn't loaded in it.
func (m *Manager) StartedAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || m.cur.exited() {
		return time.Time{}
	}
	return m.cur.started
}

// takeoverTimeout bounds how long a start or reload waits for the new
// process to answer on the stats socket.
const takeoverTimeout = 5 * time.Second

// waitAnswering waits until p is the process answering on the stats
// socket (or it exits, or takeoverTimeout passes). Right after a reload
// the old process can still answer: runtime commands sent then would
// reach it - a certificate staged in the old process and committed in
// the new one fails with "No ongoing transaction", seen on the CI runner.
// Always after a reload; after a first start, unless janusd is booting
// (Boot): it goes on to its PKI meanwhile. Called with mu held.
func (m *Manager) waitAnswering(p *process) {
	deadline := time.Now().Add(takeoverTimeout)
	for time.Now().Before(deadline) && !p.exited() {
		if out, err := m.statsCommand("show info"); err == nil && parseShowInfo(string(out)).Pid == p.cmd.Process.Pid {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// previousPID is the haproxy a new one must take over from (-sf): the one
// this Manager started, or - right after janusd restarted - the one
// recorded in PidPath, if that pid really is still a haproxy process
// (never signal a pid the kernel has since handed to something else).
// Called with mu held.
func (m *Manager) previousPID() int {
	if m.cur != nil && !m.cur.exited() {
		return m.cur.cmd.Process.Pid
	}
	data, err := os.ReadFile(m.PidPath)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil || strings.TrimSpace(string(comm)) != filepath.Base(m.BinaryPath) {
		return 0
	}
	return pid
}

// Serving reports whether janusd runs HAProxy and isn't stopping it:
// false from the moment a soft stop begins, while the old process may
// still answer on its stats socket. It doesn't take the lock Stop holds
// while it waits.
func (m *Manager) Serving() bool { return m.serving.Load() }

// Changed returns a channel closed the next time Serving changes or the
// current process exits on its own: a cue for HAProxy's health checks
// to look again now rather than at their next poll. Take it before
// checking, so a change in between isn't missed.
func (m *Manager) Changed() <-chan struct{} {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	if m.changed == nil {
		m.changed = make(chan struct{})
	}
	return m.changed
}

func (m *Manager) notifyChanged() {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	if m.changed != nil {
		close(m.changed)
		m.changed = nil
	}
}

// Running reports whether the most recently started haproxy is still
// running - the one serving traffic, ignoring any older process still
// finishing its connections after a reload.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur != nil && !m.cur.exited()
}

// Stop soft-stops haproxy (SIGUSR1: stop accepting, finish in-flight
// connections), escalating to SIGTERM if it hasn't exited after timeout.
// It first declares HAProxy not serving and waits DrainDelay, for the
// traffic to leave this node before the listeners close.
func (m *Manager) Stop(timeout time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || m.cur.exited() {
		return nil
	}
	p := m.cur
	p.stopping = true
	m.serving.Store(false)
	m.notifyChanged()
	if m.DrainDelay > 0 {
		time.Sleep(m.DrainDelay)
	}
	if err := p.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		return fmt.Errorf("signal haproxy: %w", err)
	}
	select {
	case <-p.done:
	case <-time.After(timeout):
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("haproxy (pid %d) didn't exit after SIGTERM", p.cmd.Process.Pid)
		}
	}
	// A stale pid would make the next start pass "-sf <dead pid>".
	_ = os.Remove(m.PidPath)
	return nil
}

// Info is the subset of `show info` this package parses.
type Info struct {
	Version               string
	UptimeSeconds         uint64
	CurrentConnections    uint32
	MaxConnections        uint32
	CumulativeConnections uint64
	CumulativeRequests    uint64
	ConnectionRate        uint32
	SessionRate           uint32
	IdlePercent           uint32
	Pid                   int // the process answering
}

// ShowInfo runs the stats socket's "show info" command.
func (m *Manager) ShowInfo() (*Info, error) {
	out, err := m.statsCommand("show info")
	if err != nil {
		return nil, err
	}
	return parseShowInfo(string(out)), nil
}

// parseShowInfo reads "show info"'s "Key: value" lines. Key names are
// HAProxy's own, case included ("Maxconn", not "MaxConn").
func parseShowInfo(out string) *Info {
	info := &Info{}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		u64 := func() uint64 { v, _ := strconv.ParseUint(value, 10, 64); return v }
		u32 := func() uint32 { v, _ := strconv.ParseUint(value, 10, 32); return uint32(v) }
		switch key {
		case "Version":
			info.Version = value
		case "Pid":
			info.Pid = int(u64())
		case "Uptime_sec":
			info.UptimeSeconds = u64()
		case "CurrConns":
			info.CurrentConnections = u32()
		case "Maxconn":
			info.MaxConnections = u32()
		case "CumConns":
			info.CumulativeConnections = u64()
		case "CumReq":
			info.CumulativeRequests = u64()
		case "ConnRate":
			info.ConnectionRate = u32()
		case "SessRate":
			info.SessionRate = u32()
		case "Idle_pct":
			info.IdlePercent = u32()
		}
	}
	return info
}

// ShowStat runs the stats socket's "show stat" command, returning the
// raw CSV HAProxy produces (see janus.v1alpha1.HAProxyStatsResponse -
// this package deliberately doesn't parse it structurally yet).
func (m *Manager) ShowStat() ([]byte, error) {
	return m.statsCommand("show stat")
}

// SetServerState runs the stats socket's "set server <backend>/<server>
// state <ready|drain|maint>" command.
func (m *Manager) SetServerState(backend, server, state string) error {
	b, err := cliToken("backend", backend)
	if err != nil {
		return err
	}
	s, err := cliToken("server", server)
	if err != nil {
		return err
	}
	st, err := cliToken("state", state)
	if err != nil {
		return err
	}
	return mustEmpty(m.statsCommand(fmt.Sprintf("set server %s/%s state %s", b, s, st)))
}

func (m *Manager) statsCommand(cmd string) ([]byte, error) {
	conn, err := net.DialTimeout("unix", m.StatsSocketPath, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial stats socket: %w", err)
	}
	defer conn.Close()

	// A malformed multi-line payload (runtime_certs.go's "set ssl cert
	// ... <<\n<bundle>" - found empirically missing its own trailing
	// newline once, from a test harness bug, not this code) leaves
	// HAProxy waiting for the rest of a line that never arrives: without
	// a deadline, io.Copy below blocks forever, well past any caller's
	// own timeout giving up on the RPC - a leaked goroutine per bad
	// request, not just a slow one.
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, fmt.Errorf("set stats socket deadline: %w", err)
	}

	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return nil, fmt.Errorf("write stats command: %w", err)
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, conn); err != nil {
		return nil, fmt.Errorf("read stats response: %w", err)
	}
	return buf.Bytes(), nil
}

func splitNonEmptyLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
