// Package vrrp manages the keepalived extension (docs/vrrp.md): its
// keepalived.conf - checked by keepalived itself, saved on STATE, put
// where the keepalived service janusd supervises reads it, reloaded with
// SIGHUP - its VRRP instances' state, from keepalived's own JSON dump,
// and the HAProxy health file a keepalived.conf can track, so a node
// whose HAProxy stops answering gives its virtual IPs up.
package vrrp

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/modcfg"
)

// Where keepalived and its files are - vars, so tests can move them.
var (
	Binary = "/usr/local/sbin/keepalived"
	// RunDir is keepalived's own: its configuration, pid files and the
	// HAProxy health file (the extension's manifest names these paths).
	RunDir = "/run/janus/keepalived"
	// JSONPath is where keepalived writes its JSON dump.
	JSONPath = "/tmp/keepalived.json"
)

// ServiceID is the extension's service.
const ServiceID = "keepalived"

// HealthFile is the file janusd keeps for keepalived.conf's track_file:
// "0" while HAProxy answers on its stats socket, "1" when it doesn't.
func HealthFile() string { return filepath.Join(RunDir, "haproxy-health") }

// Services is what Manager needs of janusd's extension supervisor.
type Services interface {
	Signal(id string, sig syscall.Signal) error
	StartService(id string) error
	StopService(id string) error
	State(id string) (extensions.ServiceState, error)
}

// Manager manages keepalived's configuration and reads its state.
type Manager struct {
	svc Services
	mod *modcfg.Module

	mu        sync.Mutex
	jsonSig   syscall.Signal
	lastState time.Time
	cached    []Instance
}

func New(svc Services) *Manager {
	return &Manager{svc: svc, mod: &modcfg.Module{
		Name: "keepalived", Binary: Binary, File: "keepalived.conf", RunDir: RunDir,
		CheckArgs: func(path string) []string { return []string{"--config-test", "--use-file=" + path} },
	}}
}

// Available reports whether keepalived is in the image.
func (m *Manager) Available() bool { return m.mod.Available() }

// Saved is the saved keepalived.conf; isDefault means there's none.
func (m *Manager) Saved() (string, bool, error) { return m.mod.Saved() }

// Check refuses what a Janus node can't do (Unsupported), then has
// keepalived check config.
func (m *Manager) Check(config string) ([]string, error) {
	if errs := Unsupported(config); len(errs) > 0 {
		return errs, errors.New("invalid configuration")
	}
	return m.mod.Check(config)
}

// unsupported are the keywords keepalived knows that a Janus node can't
// honour, and why. The kernel has neither macvlan nor ipvlan, and
// keepalived may change no kernel parameter (SELinux): a VMAC sets
// net.ipv4.conf.all.rp_filter to 0, which the CIS benchmark forbids, and
// disable_local_igmp - the VMACs' companion - writes one too. This check
// is for a clear message; SELinux is what keeps keepalived off the
// kernel's parameters, whatever its configuration says.
var unsupported = map[string]string{
	"use_vmac": "Janus has no VMAC interfaces: the kernel has no macvlan, and a VMAC needs " +
		"net.ipv4.conf.all.rp_filter at 0, which the CIS benchmark forbids (control 3.3.1.12)",
	"use_ipvlan":         "Janus has no IPVLAN interfaces: the kernel has no ipvlan",
	"disable_local_igmp": "keepalived can't change kernel parameters on a Janus node (net.ipv4.igmp_link_local_mcast_reports here) - and it's for VMAC interfaces, which Janus doesn't have",
}

// Unsupported lists config's lines that use a keyword a Janus node
// can't honour, in keepalived's own message format.
func Unsupported(config string) []string {
	var errs []string
	for i, line := range strings.Split(config, "\n") {
		for _, word := range strings.Fields(stripComment(line)) {
			word = strings.Trim(word, "{}")
			if why, ok := unsupported[word]; ok {
				errs = append(errs, fmt.Sprintf("(keepalived.conf: Line %d) %s isn't supported on Janus - %s", i+1, word, why))
			}
		}
	}
	return errs
}

// stripComment cuts line at its first '#' or '!' outside double quotes -
// keepalived's comments.
func stripComment(line string) string {
	quoted := false
	for i, r := range line {
		switch {
		case r == '"':
			quoted = !quoted
		case !quoted && (r == '#' || r == '!'):
			return line[:i]
		}
	}
	return line
}

// Boot puts the saved configuration where keepalived reads it - the
// service waits for that file - and writes the health file before
// keepalived starts.
func (m *Manager) Boot() error {
	if !m.Available() {
		return nil
	}
	if err := os.MkdirAll(RunDir, 0o755); err != nil {
		return err
	}
	if err := writeHealth(false); err != nil {
		return err
	}
	return m.mod.Boot()
}

// Apply checks, saves and applies config: keepalived reloads it (or
// starts with it). An empty config removes it, and stops keepalived.
func (m *Manager) Apply(config string) ([]string, error) {
	if strings.TrimSpace(config) != "" {
		if errs, err := m.Check(config); err != nil {
			return errs, err
		}
	}
	if err := m.mod.Save(config); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.cached, m.lastState = nil, time.Time{}
	m.mu.Unlock()
	if strings.TrimSpace(config) == "" {
		return nil, m.svc.StopService(ServiceID)
	}
	st, err := m.svc.State(ServiceID)
	if err != nil {
		return nil, err
	}
	switch st.State {
	case "running":
		return nil, m.svc.Signal(ServiceID, syscall.SIGHUP)
	case "stopped":
		return nil, m.svc.StartService(ServiceID)
	}
	return nil, nil // waiting for the file it now has, or restarting: it reads it next
}

// Instance is one VRRP instance's state.
type Instance struct {
	Name              string
	State             string // "MASTER", "BACKUP", "FAULT", "INIT", "STOP"
	Interface         string
	VRID              int
	Priority          int // configured
	EffectivePriority int // after tracking
	VirtualIPs        []string
	LastTransition    time.Time
	BecameMaster      uint64
}

// stateNames maps keepalived's numeric states (vrrp.h).
var stateNames = map[int]string{0: "INIT", 1: "BACKUP", 2: "MASTER", 3: "FAULT", 97: "DELETED", 98: "STOP"}

// Status asks keepalived for its JSON dump and returns its instances.
// Answers are cached a few seconds - a dump is a signal and a file.
func (m *Manager) Status() ([]Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.lastState) < 3*time.Second {
		return m.cached, nil
	}
	sig, err := m.jsonSignal()
	if err != nil {
		return nil, err
	}
	before := time.Now()
	old, _ := os.Stat(JSONPath)
	if err := m.svc.Signal(ServiceID, sig); err != nil {
		return nil, err
	}
	deadline := before.Add(3 * time.Second)
	for {
		if fi, err := os.Stat(JSONPath); err == nil && fi.Size() > 0 && (old == nil || fi.ModTime().After(old.ModTime()) || !os.SameFile(fi, old)) {
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("keepalived didn't write its state")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The dump is written by keepalived's VRRP process; give it a moment
	// to finish.
	var data []byte
	for i := 0; i < 20; i++ {
		data, err = os.ReadFile(JSONPath)
		if err == nil && json.Valid(data) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	instances, err := ParseJSON(data)
	if err != nil {
		return nil, err
	}
	m.cached, m.lastState = instances, time.Now()
	return instances, nil
}

// jsonSignal is the signal keepalived dumps its state on - it depends on
// the C library, so keepalived is asked. Called with mu held.
func (m *Manager) jsonSignal() (syscall.Signal, error) {
	if m.jsonSig != 0 {
		return m.jsonSig, nil
	}
	out, err := exec.Command(Binary, "--signum=JSON").Output()
	if err != nil {
		return 0, fmt.Errorf("keepalived --signum=JSON: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("keepalived --signum=JSON: %q", out)
	}
	m.jsonSig = syscall.Signal(n)
	return m.jsonSig, nil
}

// ParseJSON reads keepalived's JSON dump.
func ParseJSON(data []byte) ([]Instance, error) {
	var dump []struct {
		Data struct {
			Iname             string   `json:"iname"`
			IfpIfname         string   `json:"ifp_ifname"`
			State             int      `json:"state"`
			Vrid              int      `json:"vrid"`
			BasePriority      int      `json:"base_priority"`
			EffectivePriority int      `json:"effective_priority"`
			LastTransition    float64  `json:"last_transition"`
			Vips              []string `json:"vips"`
		} `json:"data"`
		Stats struct {
			BecomeMaster uint64 `json:"become_master"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		return nil, fmt.Errorf("keepalived's JSON: %w", err)
	}
	out := make([]Instance, 0, len(dump))
	for _, d := range dump {
		state, ok := stateNames[d.Data.State]
		if !ok {
			state = strconv.Itoa(d.Data.State)
		}
		in := Instance{
			Name: d.Data.Iname, State: state, Interface: d.Data.IfpIfname, VRID: d.Data.Vrid,
			Priority: d.Data.BasePriority, EffectivePriority: d.Data.EffectivePriority,
			BecameMaster: d.Stats.BecomeMaster,
		}
		if d.Data.LastTransition > 0 {
			sec := int64(d.Data.LastTransition)
			in.LastTransition = time.Unix(sec, int64((d.Data.LastTransition-float64(sec))*1e9))
		}
		for _, v := range d.Data.Vips {
			// "10.9.0.100/24 dev v0 scope global set": the address.
			if f := strings.Fields(v); len(f) > 0 {
				in.VirtualIPs = append(in.VirtualIPs, f[0])
			}
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// KeepHealth keeps HealthFile current - checked every interval with
// healthy, and at once whenever the channel changed returns is closed
// (nil: polling only) - until stop is closed.
func KeepHealth(healthy func() bool, changed func() <-chan struct{}, interval time.Duration, stop <-chan struct{}) {
	last := -1
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		var wake <-chan struct{}
		if changed != nil {
			wake = changed() // before checking: a change in between still wakes us
		}
		h := 0
		if healthy() {
			h = 1
		}
		if h != last {
			if err := writeHealth(h == 1); err != nil {
				log.Printf("vrrp: %v", err)
			} else {
				if last != -1 {
					log.Printf("vrrp: HAProxy %s - %s", map[int]string{1: "answers again", 0: "doesn't answer"}[h], HealthFile())
				}
				last = h
			}
		}
		select {
		case <-stop:
			return
		case <-t.C:
		case <-wake:
		}
	}
}

// writeHealth writes the file in place, not by renaming a new one over
// it: keepalived follows the file it opened (inotify), and would miss a
// new inode. The moment it's empty reads as 0 - healthy - so an update
// never shows a fault that isn't there.
func writeHealth(healthy bool) error {
	v := "1\n"
	if healthy {
		v = "0\n"
	}
	return os.WriteFile(HealthFile(), []byte(v), 0o644)
}
