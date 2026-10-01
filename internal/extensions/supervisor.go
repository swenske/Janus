package extensions

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/swenske/Janus/internal/events"
)

// Timing is the supervision timing: the backoff between restarts of a
// service that exits grows from MinBackoff to MaxBackoff while it keeps
// failing, and resets once it has run StableAfter. A stopped service gets
// StopTimeout to exit after SIGTERM, before SIGKILL. A service waiting for
// its WaitFor paths checks again every WaitPoll.
type Timing struct {
	MinBackoff, MaxBackoff, StableAfter, StopTimeout, WaitPoll time.Duration
}

// DefaultTiming is what NewManager uses.
var DefaultTiming = Timing{MinBackoff: time.Second, MaxBackoff: 30 * time.Second, StableAfter: 10 * time.Second, StopTimeout: 10 * time.Second, WaitPoll: 2 * time.Second}

// Manager runs the services of every extension in the image. Services
// start with the Manager and are restarted whenever they exit, until
// stopped through the API.
type Manager struct {
	manifests []Manifest
	services  map[string]*service
	order     []string
}

// ServiceState is a service's state, for the API.
type ServiceState struct {
	ID          string
	Extension   string
	Description string
	State       string // "running", "stopped", "restarting", "waiting" (for a WaitFor path), "disabled" (by its settings)
	Restarts    int
	LastError   string
}

type service struct {
	timing Timing
	ext    string
	def    Service
	logs   io.Writer

	mu       sync.Mutex
	started  bool // Start ran
	disabled bool // Configure turned it off: Start leaves it stopped
	want     bool
	cmd      *exec.Cmd
	exited   chan struct{} // closed when the current process exits
	restarts int
	lastErr  string
	waiting  string // the WaitFor path it waits for
	wake     chan struct{}
}

// NewManager prepares a Manager for manifests. logs returns where each
// service's output goes, by service id.
func NewManager(manifests []Manifest, logs func(id string) io.Writer) *Manager {
	return NewManagerWithTiming(manifests, logs, DefaultTiming)
}

// NewManagerWithTiming is NewManager with other timing (tests).
func NewManagerWithTiming(manifests []Manifest, logs func(id string) io.Writer, timing Timing) *Manager {
	m := &Manager{manifests: manifests, services: map[string]*service{}}
	for _, man := range manifests {
		for _, def := range man.Services {
			m.services[def.ID] = &service{timing: timing, ext: man.Name, def: def, logs: logs(def.ID), wake: make(chan struct{}, 1)}
			m.order = append(m.order, def.ID)
		}
	}
	return m
}

// Manifests returns the extensions in the image.
func (m *Manager) Manifests() []Manifest { return m.manifests }

// ServiceIDs returns every service, in manifest order.
func (m *Manager) ServiceIDs() []string { return append([]string(nil), m.order...) }

// Has reports whether id is an extension service.
func (m *Manager) Has(id string) bool { _, ok := m.services[id]; return ok }

// Start launches every service and supervises it from then on.
func (m *Manager) Start() {
	for _, id := range m.order {
		s := m.services[id]
		s.mu.Lock()
		s.started = true
		s.want = !s.disabled
		s.mu.Unlock()
		go s.run()
	}
}

// ErrDisabled is StartService on a service its settings turned off.
var ErrDisabled = errors.New("the service is disabled by its settings")

// StartService starts a stopped service.
func (m *Manager) StartService(id string) error {
	s, ok := m.services[id]
	if !ok {
		return errNoService
	}
	s.mu.Lock()
	if s.disabled {
		s.mu.Unlock()
		return ErrDisabled
	}
	s.want = true
	s.mu.Unlock()
	s.poke()
	return nil
}

// StopService stops a service and keeps it stopped: SIGTERM, then
// SIGKILL after StopTimeout.
func (m *Manager) StopService(id string) error {
	s, ok := m.services[id]
	if !ok {
		return errNoService
	}
	s.mu.Lock()
	s.want = false
	cmd, exited := s.cmd, s.exited
	s.mu.Unlock()
	s.poke()
	if cmd == nil {
		return nil
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(s.timing.StopTimeout):
		_ = cmd.Process.Kill()
		<-exited
	}
	return nil
}

// StopAll stops every service, concurrently.
func (m *Manager) StopAll() {
	var wg sync.WaitGroup
	for _, id := range m.order {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.StopService(id)
		}()
	}
	wg.Wait()
}

// Configure sets the arguments a service runs with and whether it runs
// at all - its settings, kept by the caller. Before Start, it's how the
// service starts (or doesn't); after, a running service is restarted
// with the new arguments, or stopped.
func (m *Manager) Configure(id string, args []string, enabled bool) error {
	s, ok := m.services[id]
	if !ok {
		return errNoService
	}
	s.mu.Lock()
	changed := !slices.Equal(s.def.Args, args)
	s.def.Args = slices.Clone(args)
	s.disabled = !enabled
	started := s.started
	s.mu.Unlock()
	if !started {
		return nil
	}
	if !enabled || changed {
		if err := m.StopService(id); err != nil {
			return err
		}
	}
	if enabled {
		return m.StartService(id)
	}
	return nil
}

// RestartService stops then starts a service.
func (m *Manager) RestartService(id string) error {
	if err := m.StopService(id); err != nil {
		return err
	}
	return m.StartService(id)
}

// State reports a service's state.
func (m *Manager) State(id string) (ServiceState, error) {
	s, ok := m.services[id]
	if !ok {
		return ServiceState{}, errNoService
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := ServiceState{ID: id, Extension: s.ext, Description: s.def.Description, Restarts: s.restarts, LastError: s.lastErr}
	switch {
	case s.cmd != nil:
		st.State = "running"
	case s.want && s.waiting != "":
		st.State = "waiting"
	case s.want:
		st.State = "restarting"
	case s.disabled:
		st.State = "disabled"
	default:
		st.State = "stopped"
	}
	return st, nil
}

// ErrNotRunning is Signal on a service that isn't running.
var ErrNotRunning = errors.New("the service isn't running")

// Signal sends sig to a running service's process (its own, not the
// children it may have) - a reload, a status dump.
func (m *Manager) Signal(id string, sig syscall.Signal) error {
	s, ok := m.services[id]
	if !ok {
		return errNoService
	}
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd == nil {
		return ErrNotRunning
	}
	return cmd.Process.Signal(sig)
}

// IsNoService reports whether err means an unknown service.
func IsNoService(err error) bool { return errors.Is(err, errNoService) }

func (s *service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run supervises the service forever.
func (s *service) run() {
	t := s.timing
	backoff := t.MinBackoff
	for {
		// Wait to be wanted.
		for {
			s.mu.Lock()
			want := s.want
			s.mu.Unlock()
			if want {
				break
			}
			<-s.wake
		}
		if !s.ready() {
			s.sleep(cmp.Or(t.WaitPoll, time.Second))
			continue
		}

		s.mu.Lock()
		args := slices.Clone(s.def.Args) // Configure may change them
		s.mu.Unlock()
		cmd := exec.Command(s.def.Path, args...)
		cmd.Stdout, cmd.Stderr = s.logs, s.logs
		started := time.Now()
		if err := cmd.Start(); err != nil {
			s.mu.Lock()
			s.lastErr = err.Error()
			s.restarts++
			s.mu.Unlock()
			log.Printf("extensions: start %s: %v", s.def.ID, err)
			s.sleep(backoff)
			backoff = min(backoff*2, t.MaxBackoff)
			continue
		}
		exited := make(chan struct{})
		s.mu.Lock()
		s.cmd, s.exited = cmd, exited
		s.mu.Unlock()
		log.Printf("extensions: started %s (%s, pid %d)", s.def.ID, s.ext, cmd.Process.Pid)
		events.Publish("service.started", map[string]string{"id": s.def.ID, "extension": s.ext})

		err := cmd.Wait()
		s.mu.Lock()
		s.cmd = nil
		want := s.want
		if err != nil {
			s.lastErr = err.Error()
		} else {
			s.lastErr = "exited"
		}
		s.mu.Unlock()
		close(exited)

		if !want {
			log.Printf("extensions: stopped %s", s.def.ID)
			events.Publish("service.stopped", map[string]string{"id": s.def.ID, "extension": s.ext})
			backoff = t.MinBackoff
			continue
		}
		if time.Since(started) >= t.StableAfter {
			backoff = t.MinBackoff
		}
		s.mu.Lock()
		s.restarts++
		s.mu.Unlock()
		log.Printf("extensions: %s exited (%v) - restarting in %s", s.def.ID, errOrExit(err), backoff)
		events.Publish("service.exited", map[string]string{"id": s.def.ID, "extension": s.ext, "error": fmt.Sprint(errOrExit(err))})
		s.sleep(backoff)
		backoff = min(backoff*2, t.MaxBackoff)
	}
}

// ready reports whether every WaitFor path exists, logging when the
// service starts or stops waiting.
func (s *service) ready() bool {
	missing := ""
	for _, p := range s.def.WaitFor {
		if _, err := os.Stat(p); err != nil {
			missing = p
			break
		}
	}
	s.mu.Lock()
	was := s.waiting
	s.waiting = missing
	if missing != "" {
		s.lastErr = "waiting for " + missing
	}
	s.mu.Unlock()
	switch {
	case missing != "" && was != missing:
		log.Printf("extensions: %s waits for %s", s.def.ID, missing)
	case missing == "" && was != "":
		log.Printf("extensions: %s: %s is there", s.def.ID, was)
	}
	return missing == ""
}

// sleep waits d, or until a Start/Stop pokes the service.
func (s *service) sleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-s.wake:
	}
}

func errOrExit(err error) any {
	if err == nil {
		return "exit status 0"
	}
	return err
}
