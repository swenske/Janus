package machines

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/autoscale"
	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// Auto-scaling modes.
const (
	// AutoscaleOn resizes the machine as its load says.
	AutoscaleOn = "on"
	// AutoscaleObserve only writes in the journal what it would do -
	// the size it would have given, simulated.
	AutoscaleObserve = "observe"
	// AutoscaleOff does neither: the machine goes back to its nominal
	// size, and keeps its room to grow - turning it on again needs no
	// restart.
	AutoscaleOff = "off"
)

// Autoscale is a machine's auto-scaling policy (docs/guide/auto-scaling.md):
// between its nominal size (Spec.VCPUs, Spec.MemoryMiB) and these
// maxima, while it runs. The virtual machine is started with room for
// the maxima: setting them, or changing them, restarts it once.
type Autoscale struct {
	Mode         string `json:"mode"`
	MaxVCPUs     int    `json:"max_vcpus"`
	MaxMemoryMiB int    `json:"max_memory_mib"`

	// The thresholds - 0: internal/autoscale's default.
	CPUUpIdlePercent           int `json:"cpu_up_idle_percent,omitempty"`
	CPUUpBusyPercent           int `json:"cpu_up_busy_percent,omitempty"`
	CPUUpAfterSeconds          int `json:"cpu_up_after_seconds,omitempty"`
	CPUDownIdlePercent         int `json:"cpu_down_idle_percent,omitempty"`
	MemoryUpAvailablePercent   int `json:"memory_up_available_percent,omitempty"`
	MemoryUpAfterSeconds       int `json:"memory_up_after_seconds,omitempty"`
	MemoryDownAvailablePercent int `json:"memory_down_available_percent,omitempty"`
	ReturnAfterSeconds         int `json:"return_after_seconds,omitempty"`
	CooldownSeconds            int `json:"cooldown_seconds,omitempty"`
	MaxStepsPerHour            int `json:"max_steps_per_hour,omitempty"`

	// Sysctl: the node's kernel parameters follow - its suggestions
	// applied on trial and confirmed by the Controller.
	Sysctl bool `json:"sysctl,omitempty"`
}

// Thresholds is the policy's, its zeros the defaults.
func (a *Autoscale) Thresholds() autoscale.Thresholds {
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	return autoscale.Thresholds{
		CPUUpIdlePercent:           a.CPUUpIdlePercent,
		CPUUpBusyPercent:           a.CPUUpBusyPercent,
		CPUUpAfter:                 sec(a.CPUUpAfterSeconds),
		CPUDownIdlePercent:         a.CPUDownIdlePercent,
		MemoryUpAvailablePercent:   a.MemoryUpAvailablePercent,
		MemoryUpAfter:              sec(a.MemoryUpAfterSeconds),
		MemoryDownAvailablePercent: a.MemoryDownAvailablePercent,
		ReturnAfter:                sec(a.ReturnAfterSeconds),
		Cooldown:                   sec(a.CooldownSeconds),
		MaxStepsPerHour:            a.MaxStepsPerHour,
	}.WithDefaults()
}

// Check says what's wrong with the policy for a machine of spec's size.
func (a *Autoscale) Check(spec Spec) error {
	switch a.Mode {
	case AutoscaleOn, AutoscaleObserve, AutoscaleOff:
	default:
		return fmt.Errorf("autoscale.mode %q: on, observe or off", a.Mode)
	}
	if a.MaxVCPUs < spec.VCPUs || a.MaxVCPUs > 64 {
		return fmt.Errorf("autoscale.max_vcpus: from its vCPUs (%d) to 64", spec.VCPUs)
	}
	if a.MaxMemoryMiB < spec.MemoryMiB || a.MaxMemoryMiB > 262144 {
		return fmt.Errorf("autoscale.max_memory_mib: from its memory (%d MiB) to 262144", spec.MemoryMiB)
	}
	if a.MaxVCPUs == spec.VCPUs && a.MaxMemoryMiB == spec.MemoryMiB {
		return errors.New("autoscale: its maxima are its size - nothing to grow to")
	}
	for _, s := range []int{a.CPUUpAfterSeconds, a.MemoryUpAfterSeconds, a.ReturnAfterSeconds, a.CooldownSeconds} {
		if s < 0 {
			return errors.New("autoscale: durations are positive")
		}
	}
	if err := a.Thresholds().Validate(); err != nil {
		return fmt.Errorf("autoscale.%v", err)
	}
	return nil
}

// Hardware is what the virtual machine is made of: the spec's size, and
// the maxima of its auto-scaling policy, if it has one.
func (s Spec) Hardware() hypervisor.Hardware {
	hw := hypervisor.Hardware{VCPUs: s.VCPUs, MemoryMiB: s.MemoryMiB}
	if s.Autoscale != nil {
		hw.MaxVCPUs, hw.MaxMemoryMiB = s.Autoscale.MaxVCPUs, s.Autoscale.MaxMemoryMiB
	}
	return hw
}

// Size is what the virtual machine has now, as its hypervisor says.
type Size struct {
	VCPUs     int `json:"vcpus"`
	MemoryMiB int `json:"memory_mib"`
	// The most it can have without a restart (MaxMemoryMiB 0: no
	// ceiling of the hypervisor's own), and what memory changes by while
	// it runs (0: it doesn't).
	MaxVCPUs      int `json:"max_vcpus"`
	MaxMemoryMiB  int `json:"max_memory_mib"`
	MemoryStepMiB int `json:"memory_step_mib,omitempty"`
}

// AutoscaleState is what auto-scaling remembers of a machine, and its
// last word.
type AutoscaleState struct {
	// Mode is the one it ran in: a new one starts over (observation's
	// simulated size isn't the machine's).
	Mode string `json:"mode"`
	autoscale.State
	// Status: what it does now - watching, why it waits, what it just
	// did -, Warnings what no step fixes.
	Status   string    `json:"status,omitempty"`
	Warnings []string  `json:"warnings,omitempty"`
	At       time.Time `json:"at,omitempty"`
	// Sysctl: the kernel parameters auto-scaling changed on the node,
	// by name - put back when the machine is at its size again, unless
	// someone changed them since.
	Sysctl map[string]SysctlChange `json:"sysctl,omitempty"`
}

// SysctlChange is a kernel parameter auto-scaling changed: what it was,
// what it became, the node's rule that suggested it, when.
type SysctlChange struct {
	Before string    `json:"before"`
	After  string    `json:"after"`
	Rule   string    `json:"rule"`
	At     time.Time `json:"at"`
}

// JournalEntry is one line of a machine's auto-scaling journal: a step
// taken (or, observing, one that would have been), why, and how it went.
type JournalEntry struct {
	Time time.Time `json:"time"`
	Mode string    `json:"mode"`
	// Action: "up", "down", "sysctl", "mode"...
	Action   string         `json:"action"`
	Resource string         `json:"resource,omitempty"`
	From     autoscale.Size `json:"from"`
	To       autoscale.Size `json:"to"`
	Reason   string         `json:"reason"`
	// Error: the step failed - why.
	Error string `json:"error,omitempty"`
}

// journalKeep is how many entries a journal keeps: it's cut back to
// that when twice as long.
const journalKeep = 1000

const journalFile = "autoscale.jsonl"

// AppendJournal adds an entry to machine id's auto-scaling journal.
func (s *Store) AppendJournal(id string, e JournalEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.machines[id]; !ok {
		return fmt.Errorf("no such machine %q", id)
	}
	path := filepath.Join(s.dir, id, journalFile)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	entries, err := readJournal(path)
	if err != nil || len(entries) <= 2*journalKeep {
		return err
	}
	var b bytes.Buffer
	for _, e := range entries[len(entries)-journalKeep:] {
		raw, _ := json.Marshal(e)
		b.Write(append(raw, '\n'))
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Journal is machine id's newest n journal entries, oldest first.
func (s *Store) Journal(id string, n int) ([]JournalEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := readJournal(filepath.Join(s.dir, id, journalFile))
	if errors.Is(err, os.ErrNotExist) {
		return []JournalEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	if n > 0 && len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	return entries, nil
}

func readJournal(path string) ([]JournalEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []JournalEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e JournalEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
