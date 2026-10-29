// Package haproxy manages a single supervised HAProxy process: validating
// and applying config (via `haproxy -c`), starting it, and reloading it
// seamlessly (`-sf <old pid>`) when the config changes. It deliberately
// does not reimplement HAProxy's own metrics - see docs/architecture.md:
// HAProxy's built-in Prometheus exporter stays the source of truth for
// HAProxy metrics, this package only proxies the runtime (stats) socket
// for HAProxyService's other RPCs (ShowInfo, ServerSetState, ...).
package haproxy

import (
	"bufio"
	"bytes"
	"errors"
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
	"github.com/swenske/Janus/internal/oomadj"
)

// Manager supervises one HAProxy process instance.
type Manager struct {
	BinaryPath      string
	ConfigPath      string
	PidPath         string
	StatsSocketPath string

	// Output, if set, also receives HAProxy's stdout/stderr, for
	// SystemService.Logs.
	Output io.Writer
	// Console is where HAProxy's output is shown as it comes - janusd's
	// own stdout when nil. On a node, a share of the console that never
	// waits (internal/consolelimit): access logs under load would hold
	// janusd back.
	Console io.Writer

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

	// StopWatch is called just before a stop for good: the function it
	// returns, polled afterwards, reports whether HAProxy pid still has a
	// client's connection (janusd's, cmd/janusd). Soft-stopped for good, a
	// HAProxy with a peers section waits for a successor to teach its
	// stick tables to - 6 s on 3.4.7, measured, and none comes: Stop ends
	// it once its clients are served. Nil, or nil returned: Stop waits for
	// the process as it is.
	StopWatch func(pid int) (clientsLeft func() bool)

	// Env, if set, adds to HAProxy's environment - for `haproxy -c` too:
	// a configuration can use these variables. Read at each start and
	// validation.
	Env func() []string

	// Policy, if set, is what a configuration's global section must and
	// may not say (globalcheck.go): checked by Validate before `haproxy
	// -c`, so by Apply too - never at boot, where the configuration on
	// STATE is what the node has.
	Policy *GlobalPolicy

	// ExtraConfigs, if set, are configuration files HAProxy reads after
	// ConfigPath (-f each) - the peers section janusd generates
	// (internal/cluster) -, for `haproxy -c` too: what ConfigPath says
	// may refer to them.
	ExtraConfigs func() []string
	// LocalPeer, if set, names HAProxy's local peer (-L): the cluster
	// member name, or the hostname of a node that is no member - what
	// the generated peers section calls this node.
	LocalPeer func() string

	// txMu serializes runtime certificate transactions: HAProxy has one
	// open at a time.
	txMu sync.Mutex

	// FilesDir holds HAProxy's own files (files.go): error pages, maps,
	// certificates the configuration references.
	FilesDir string
	fileMu   sync.Mutex

	mu  sync.Mutex
	cur *process
	// serving: a process runs that janusd isn't stopping. During a soft
	// stop the stats socket still answers, but the listeners are closed.
	serving atomic.Bool
	// RestartFirst, RestartMax and RestartSettled: the waits before
	// HAProxy starts again after exiting on its own (restart.go) - zero,
	// the defaults.
	RestartFirst, RestartMax, RestartSettled time.Duration

	// gen counts the starts and stops asked of the Manager (Boot, Reload,
	// Apply, Stop): a restart planned after an exit goes ahead only if
	// nobody asked for anything since. restartDelay is the wait before the
	// next one (restart.go). Both under mu.
	gen          uint64
	restartDelay time.Duration

	changeMu sync.Mutex
	changed  chan struct{}
	updated  chan struct{}

	counters Counters
}

// Counters are what the Manager has done since janusd started, for the
// node's exporter.
type Counters struct {
	Starts          atomic.Uint64 // processes started, reloads included
	Reloads         atomic.Uint64 // seamless reloads (-sf)
	UnexpectedExits atomic.Uint64 // a process that exited without being stopped or replaced
	Restarts        atomic.Uint64 // started again by janusd after one exited on its own
	ApplyAccepted   atomic.Uint64
	ApplyRejected   atomic.Uint64
	LastApplyUnix   atomic.Int64 // last accepted Apply
	// UntrackedListening: HAProxy processes that listen but aren't the
	// one janusd runs, seen at two looks in a row (WatchUntracked).
	UntrackedListening atomic.Int64
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
	failed   bool // exited before taking over (StartError), under Manager.mu
	lost     bool // died while a reload failed: its restart planned there, under Manager.mu
	output   *tailWriter
}

// StartError is a start or reload HAProxy refused although its
// configuration checks out - `haproxy -c` doesn't apply the process
// limits, for one (a "ulimit-n" above the node's): the new process exited
// before taking over. The previous process, if any, still serves.
type StartError struct {
	Output []string // what HAProxy printed
}

func (e *StartError) Error() string {
	why := "exited at once"
	for _, l := range e.Output {
		if strings.Contains(l, "[ALERT]") {
			why = l
			break
		}
	}
	return "haproxy refused to start: " + why
}

// tailWriter keeps the first lines a process prints - a refused start's
// reasons - and drops the rest.
type tailWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (t *tailWriter) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if room := t.limit - t.buf.Len(); room > 0 {
		t.buf.Write(b[:min(len(b), room)])
	}
	return len(b), nil
}

func (t *tailWriter) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return splitNonEmptyLines(t.buf.String())
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
	if err := m.Policy.Check(cfg); err != nil {
		return false, []string{err.Error()}
	}
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

	check := exec.Command(m.BinaryPath, append([]string{"-c"}, m.configArgs(tmp.Name())...)...)
	check.Env = m.env()
	var combined bytes.Buffer
	check.Stdout, check.Stderr = &combined, &combined
	// Not the serving HAProxy's score: a check of a configuration too big
	// for the node must not have the kernel kill the one serving.
	err = oomadj.Run(check, oomadj.Default)
	out := combined.Bytes()
	if err != nil {
		// HAProxy names the file it checked: the operator knows it as
		// haproxy.cfg, not as this temporary copy.
		errs := splitNonEmptyLines(strings.ReplaceAll(string(out), tmp.Name(), "haproxy.cfg"))
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
	previous, previousErr := os.ReadFile(m.ConfigPath)
	if err := os.WriteFile(m.ConfigPath, cfg, 0o644); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	// ConfigPath may be the Phase 3 cont'd persistent STATE partition
	// (see rootfs/init/main.go's mountState) - force it to the
	// underlying block device now, same reasoning as cmd/janusd's
	// PKI bootstrap: don't let an applied config's durability depend on
	// some later, unrelated sync happening to occur first.
	syscall.Sync()
	if err := m.startOrReload(true); err != nil {
		var se *StartError
		if !errors.As(err, &se) {
			return nil, err
		}
		// Checked, but HAProxy won't run it: the previous configuration
		// goes back, or the next boot would start HAProxy from this one -
		// and fail.
		var rerr error
		switch {
		case previousErr == nil:
			rerr = os.WriteFile(m.ConfigPath, previous, 0o644)
		case errors.Is(previousErr, os.ErrNotExist):
			rerr = os.Remove(m.ConfigPath)
		}
		if rerr != nil {
			log.Printf("haproxy: put the previous configuration back: %v", rerr)
		}
		syscall.Sync()
		m.counters.ApplyRejected.Add(1)
		return se.Output, err
	}
	m.counters.ApplyAccepted.Add(1)
	m.counters.LastApplyUnix.Store(time.Now().Unix())
	return nil, nil
}

// Reload re-applies whatever is currently on disk at ConfigPath - used
// when the config file hasn't changed but a restart is still wanted (or
// as the second half of Apply). It returns once the new process answers
// on the stats socket, ready for runtime commands - or a *StartError if it
// exited instead, the previous process still serving.
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
	m.gen++ // asked for: a restart planned meanwhile is moot
	return m.startLocked(wait)
}

// startLocked is startOrReload with mu held - also how a restart after an
// exit starts HAProxy again (restart.go), without counting as asked for.
func (m *Manager) startLocked(wait bool) error {
	args := m.configArgs(m.ConfigPath)
	prev := m.cur
	old := m.previousPID()
	reload := old > 0
	if reload {
		args = append(args, "-sf", strconv.Itoa(old))
	}

	cmd := exec.Command(m.BinaryPath, args...)
	cmd.Env = m.env()
	output := &tailWriter{limit: 16 << 10}
	console := m.Console
	if console == nil {
		console = os.Stdout
	}
	out := []io.Writer{console, output}
	if m.Output != nil {
		out = append(out, m.Output)
	}
	cmd.Stdout, cmd.Stderr = io.MultiWriter(out...), io.MultiWriter(out...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start haproxy: %w", err)
	}
	oomadj.Started(cmd, oomadj.HAProxy)
	m.counters.Starts.Add(1)
	if reload {
		m.counters.Reloads.Add(1)
	}
	// Every started process is waited on, including the ones a later
	// seamless reload (-sf) replaces - otherwise each would stay a zombie
	// under janusd for as long as it runs.
	p := &process{cmd: cmd, started: time.Now(), done: make(chan struct{}), output: output}
	go func() {
		p.err = cmd.Wait()
		close(p.done) // before taking mu: Stop holds it while waiting on done
		m.mu.Lock()
		reason := "exited on its own"
		var restartIn time.Duration
		switch {
		case p.stopping:
			reason = "stopped"
		case p.failed:
			reason = "refused to start"
		case p.lost:
			m.counters.UnexpectedExits.Add(1) // still exited on its own
		case m.cur != p:
			reason = "replaced by a reload"
		default:
			// Killed (the kernel's OOM killer, a signal) or crashed: it
			// starts again (restart.go). Serving stays true - not stopped
			// by janusd -, the health checks see the stats socket fail.
			m.counters.UnexpectedExits.Add(1)
			restartIn = m.planRestart(p)
		}
		m.mu.Unlock()
		if reason == "exited on its own" {
			m.notifyChanged() // the health checks needn't wait for their next poll
		}
		exit := ""
		if p.err != nil {
			exit = p.err.Error()
		}
		ev := map[string]any{"pid": cmd.Process.Pid, "reason": reason, "exit": exit}
		if restartIn > 0 {
			ev["restart_in_seconds"] = restartIn.Seconds()
			log.Printf("haproxy: pid %d exited on its own (%s) - starting it again in %s", cmd.Process.Pid, exit, restartIn)
		}
		events.Publish("haproxy.exited", ev)
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
	if (reload || wait) && !m.waitAnswering(p) && p.exited() {
		// It never took over: -sf wasn't sent, the previous process (if
		// any) still serves - it's the current one again.
		p.failed = true
		m.cur = prev
		if prev != nil && prev.exited() {
			// Unless it died meanwhile - the OOM killer may take it after
			// the new one: its exit is counted, its restart planned here.
			prev.lost = true
			m.cur = nil
		}
		if m.cur != nil {
			_ = os.WriteFile(m.PidPath, []byte(strconv.Itoa(old)+"\n"), 0o644)
		} else {
			// Nothing serves: HAProxy starts again (restart.go) - with the
			// configuration Apply puts back meanwhile.
			_ = os.Remove(m.PidPath)
			m.serving.Store(false)
			m.notifyChanged()
			m.planRestart(p)
		}
		return &StartError{Output: output.lines()}
	}
	m.restoreCerts(p)
	m.notifyUpdated()
	return nil
}

// configArgs are HAProxy's configuration arguments for cfg: it, the
// extra files, the local peer name.
func (m *Manager) configArgs(cfg string) []string {
	args := []string{"-f", cfg}
	if m.ExtraConfigs != nil {
		for _, p := range m.ExtraConfigs() {
			args = append(args, "-f", p)
		}
	}
	if m.LocalPeer != nil {
		if name := m.LocalPeer(); name != "" {
			args = append(args, "-L", name)
		}
	}
	return args
}

// env is HAProxy's environment: janusd's, plus Env's.
func (m *Manager) env() []string {
	if m.Env == nil {
		return nil // inherit
	}
	return append(os.Environ(), m.Env()...)
}

// Pid is the current HAProxy process's - 0 if none runs.
func (m *Manager) Pid() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil || m.cur.exited() {
		return 0
	}
	return m.cur.cmd.Process.Pid
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
// socket (true), or it exits, or takeoverTimeout passes. Right after a reload
// the old process can still answer: runtime commands sent then would
// reach it - a certificate staged in the old process and committed in
// the new one fails with "No ongoing transaction", seen on the CI runner.
// Always after a reload; after a first start, unless janusd is booting
// (Boot): it goes on to its PKI meanwhile. Called with mu held.
func (m *Manager) waitAnswering(p *process) bool {
	deadline := time.Now().Add(takeoverTimeout)
	for time.Now().Before(deadline) && !p.exited() {
		if out, err := m.statsCommand("show info"); err == nil && parseShowInfo(string(out)).Pid == p.cmd.Process.Pid {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
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

// Updated returns a channel closed the next time HAProxy runs another
// configuration (a start, a reload) or a map or an ACL changes at
// runtime (MapUpdate, ACLUpdate): a cue for what reads the
// configuration and its maps - the services registered in Consul - to
// read them again. Take it before reading.
func (m *Manager) Updated() <-chan struct{} {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	if m.updated == nil {
		m.updated = make(chan struct{})
	}
	return m.updated
}

func (m *Manager) notifyUpdated() {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	if m.updated != nil {
		close(m.updated)
		m.updated = nil
	}
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
// connections), escalating to SIGTERM if it hasn't exited after timeout
// - or once StopWatch says its clients are served. It first
// declares HAProxy not serving and waits DrainDelay, for the traffic to
// leave this node before the listeners close.
func (m *Manager) Stop(timeout time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gen++ // stopped: no restart after an exit either
	if m.cur == nil || m.cur.exited() {
		// Already gone (it exited on its own): stopped now, not serving.
		if m.serving.Swap(false) {
			m.notifyChanged()
		}
		return nil
	}
	p := m.cur
	p.stopping = true
	m.serving.Store(false)
	m.notifyChanged()
	if m.DrainDelay > 0 {
		time.Sleep(m.DrainDelay)
	}
	var clientsLeft func() bool
	if m.StopWatch != nil {
		clientsLeft = m.StopWatch(p.cmd.Process.Pid)
	}
	if err := p.cmd.Process.Signal(syscall.SIGUSR1); err != nil {
		return fmt.Errorf("signal haproxy: %w", err)
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	stopped := false
	for !stopped {
		select {
		case <-p.done:
			stopped = true
			continue
		case <-tick.C:
			if clientsLeft == nil || clientsLeft() {
				continue
			}
		case <-deadline:
		}
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
			stopped = true
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
	DroppedLogs           uint64
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
		case "DroppedLogs":
			info.DroppedLogs = u64()
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

// statsStream runs cmd and hands each line of the answer to line until
// it returns false or the answer ends - for answers a busy node makes
// huge ("show table", "show sess"), which statsCommand would read
// whole. Past maxBytes it stops too, and says so (truncated).
func (m *Manager) statsStream(cmd string, maxBytes int64, line func(string) bool) (truncated bool, err error) {
	conn, err := net.DialTimeout("unix", m.StatsSocketPath, 2*time.Second)
	if err != nil {
		return false, fmt.Errorf("dial stats socket: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return false, fmt.Errorf("set stats socket deadline: %w", err)
	}
	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return false, fmt.Errorf("write stats command: %w", err)
	}
	r := &io.LimitedReader{R: conn, N: maxBytes + 1}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if r.N <= 0 { // the last line may be cut: not handed over
			return true, nil
		}
		if !line(sc.Text()) {
			return false, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("read stats response: %w", err)
	}
	return r.N <= 0, nil
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
