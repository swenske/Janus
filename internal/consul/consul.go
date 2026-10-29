// Package consul manages the consul extension (docs/consul.md): the
// Consul agent, run with the operator's own configuration - checked by
// `consul validate`, saved on STATE, copied where the agent janusd
// supervises reads it (/run/janus/consul: the agent never reads STATE,
// which holds the node's keys) with the files it names (TLS certificates
// and keys...), and the agent restarted to apply it.
//
// janusd adds a fragment of its own, loaded after the operator's
// configuration: the data directory (in /run - nothing the agent keeps
// there needs to outlive a reboot when everything else comes from the
// configuration), a node ID kept on STATE, so the node stays the same
// member of its cluster across reboots, and the layer a Controller
// manages, if one does (managed.go) - with which janusd registers the
// node's own services in the agent (register.go).
package consul

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/modcfg"
	"github.com/swenske/Janus/internal/oomadj"
)

// Where the agent and its files are - vars, so tests can move them.
var (
	Binary = "/usr/local/sbin/consul"
	// RunDir is the agent's own: its configuration, files and data.
	RunDir = "/run/janus/consul"
	// HTTPAddr is the agent's HTTP API, for the status - its default.
	HTTPAddr = "127.0.0.1:8500"
)

const (
	// ServiceID is the extension's service.
	ServiceID = "consul"

	fragmentFile = "janus.json"
	nodeIDFile   = "consul-node-id"
	// filesDir is where the configuration's files are, under RunDir and,
	// as saved, under modcfg.Dir.
	filesDir      = "files"
	savedFilesDir = "consul-files"
	maxFiles      = 32
	maxFileBytes  = 1 << 20
)

var fileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// FilesDir is where the agent finds the files given with its
// configuration.
func FilesDir() string { return filepath.Join(RunDir, filesDir) }

// Services is what Manager needs of janusd's extension supervisor.
type Services interface {
	StartService(id string) error
	StopService(id string) error
	RestartService(id string) error
	State(id string) (extensions.ServiceState, error)
}

// Manager manages the agent's configuration and reads its state.
type Manager struct {
	svc Services
	mod *modcfg.Module
	// Main is the node's main IPv4 address, "" if none: the one the
	// agent advertises when the managed layer names none.
	Main func() string

	mu sync.Mutex // an operator's apply, the managed layer's
}

func New(svc Services) *Manager {
	return &Manager{svc: svc, mod: &modcfg.Module{
		Name: "consul", Binary: Binary, File: "consul.hcl", RunDir: RunDir,
		CheckArgs: func(path string) []string {
			return []string{"validate", path, filepath.Join(RunDir, fragmentFile)}
		},
	}}
}

// Available reports whether Consul is in the image.
func (m *Manager) Available() bool { return m.mod.Available() }

// Saved is the saved configuration (isDefault: none), and the names of
// the files saved with it.
func (m *Manager) Saved() (config string, isDefault bool, files []string, err error) {
	config, isDefault, err = m.mod.Saved()
	if err != nil {
		return "", false, nil, err
	}
	files, err = savedFileNames()
	return config, isDefault, files, err
}

func savedFileNames() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(modcfg.Dir, savedFilesDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && fileNamePattern.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Boot writes janusd's fragment - creating the node ID the first time -
// and puts the saved configuration and its files where the agent reads
// them; the service waits for the configuration.
func (m *Manager) Boot() error {
	if !m.Available() {
		return nil
	}
	if err := os.MkdirAll(RunDir, 0o700); err != nil {
		return err
	}
	managed, err := LoadManaged()
	if err != nil {
		log.Printf("consul: the managed layer: %v - the agent runs without it", err)
		managed = nil
	}
	if _, err := m.writeFragment(managed); err != nil {
		return err
	}
	names, err := savedFileNames()
	if err != nil {
		return err
	}
	files := map[string][]byte{}
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(modcfg.Dir, savedFilesDir, n))
		if err != nil {
			return err
		}
		files[n] = data
	}
	if err := writeRunFiles(files); err != nil {
		return err
	}
	if err := m.mod.Boot(); err != nil {
		return err
	}
	_, isDefault, err := m.mod.Saved()
	if err != nil || !isDefault || managed == nil {
		return err
	}
	return writeIfChanged(m.mod.RunPath(), []byte(placeholder), 0o644)
}

func (m *Manager) main() string {
	if m.Main == nil {
		return ""
	}
	return m.Main()
}

// writeFragment writes janusd's part of the configuration with the
// managed layer managed (nil: none) and the CA it names; changed: the
// agent doesn't read what it did.
func (m *Manager) writeFragment(managed *Managed) (changed bool, err error) {
	data, err := fragment(managed, m.main())
	if err != nil {
		return false, err
	}
	ca := managedCAPath()
	if managed.IsSet() && managed.CAPEM != "" {
		if err := os.MkdirAll(filepath.Dir(ca), 0o700); err != nil {
			return false, err
		}
		c, err := writeIfChangedReport(ca, []byte(managed.CAPEM), 0o644)
		if err != nil {
			return false, err
		}
		changed = c
	} else if err := os.Remove(ca); err == nil {
		changed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	c, err := writeIfChangedReport(filepath.Join(RunDir, fragmentFile), data, 0o600)
	return changed || c, err
}

// writeIfChanged replaces path with data unless it holds it already.
func writeIfChanged(path string, data []byte, perm os.FileMode) error {
	_, err := writeIfChangedReport(path, data, perm)
	return err
}

func writeIfChangedReport(path string, data []byte, perm os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		if st, err := os.Stat(path); err == nil && st.Mode().Perm() == perm {
			return false, nil
		}
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return false, err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// nodeID is the node's Consul node ID: random, made once, kept on STATE.
func nodeID() (string, error) {
	path := filepath.Join(modcfg.Dir, nodeIDFile)
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); uuidPattern.MatchString(id) {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	if err := os.MkdirAll(modcfg.Dir, 0o755); err != nil {
		return "", err
	}
	if err := writeSynced(path, []byte(id+"\n"), 0o644); err != nil {
		return "", err
	}
	return id, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Check has Consul check config (with janusd's fragment) and checks the
// files' names and sizes.
func (m *Manager) Check(config string, files map[string][]byte) ([]string, error) {
	var errs []string
	if len(files) > maxFiles {
		errs = append(errs, fmt.Sprintf("over %d files", maxFiles))
	}
	for _, n := range sortedNames(files) {
		if !fileNamePattern.MatchString(n) {
			errs = append(errs, fmt.Sprintf("file name %q: letters, digits, '.', '-', '_'", n))
		}
		if len(files[n]) > maxFileBytes {
			errs = append(errs, fmt.Sprintf("file %s is over %d bytes", n, maxFileBytes))
		}
	}
	if len(errs) > 0 {
		return errs, errors.New("invalid files")
	}
	if err := os.MkdirAll(RunDir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(RunDir, fragmentFile)); err != nil {
		managed, err := LoadManaged()
		if err != nil {
			return nil, err
		}
		if _, err := m.writeFragment(managed); err != nil {
			return nil, err
		}
	}
	return m.mod.Check(config)
}

// Apply checks, saves and applies a configuration with its files: an
// empty value keeps a saved file, a saved file not given is removed. The
// agent restarts with it (or starts). An empty configuration removes it
// all, and stops the agent - unless a Controller manages a layer: the
// agent then goes on with that alone.
func (m *Manager) Apply(config string, files map[string][]byte) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	files, err := mergeFiles(files)
	if err != nil {
		return nil, err
	}
	empty := strings.TrimSpace(config) == ""
	if empty {
		files = nil
	} else if errs, err := m.Check(config, files); err != nil {
		return errs, err
	}
	if err := saveFiles(files); err != nil {
		return nil, err
	}
	if err := writeRunFiles(files); err != nil {
		return nil, err
	}
	if err := m.mod.Save(config); err != nil {
		return nil, err
	}
	if empty {
		managed, err := LoadManaged()
		if err != nil {
			return nil, err
		}
		if managed == nil {
			return nil, m.svc.StopService(ServiceID)
		}
		if err := writeIfChanged(m.mod.RunPath(), []byte(placeholder), 0o644); err != nil {
			return nil, err
		}
	}
	return nil, m.restart()
}

// restart has the agent read its configuration again: restarted, or
// started.
func (m *Manager) restart() error {
	st, err := m.svc.State(ServiceID)
	if err != nil {
		return err
	}
	switch st.State {
	case "running", "restarting":
		return m.svc.RestartService(ServiceID)
	case "stopped":
		return m.svc.StartService(ServiceID)
	}
	return nil // waiting for the configuration it now has
}

// Managed is the saved managed layer, nil: none.
func (m *Manager) Managed() (*Managed, error) { return LoadManaged() }

// SetManaged checks, saves and applies the managed layer next (nil or
// unset: none) - keepEncrypt and keepToken keep the saved secrets when
// next leaves them empty. The agent restarts when what it reads changed
// (restarted); without an operator's configuration it runs on the layer
// alone, and removing the layer then stops it.
func (m *Manager) SetManaged(next *Managed, keepEncrypt, keepToken bool) (errs []string, restarted bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := LoadManaged()
	if err != nil {
		cur = nil // unreadable: replaced
	}
	_, noOperator, err := m.mod.Saved()
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(RunDir, 0o700); err != nil {
		return nil, false, err
	}
	if !next.IsSet() {
		if cur == nil {
			return nil, false, saveManaged(nil)
		}
		if err := saveManaged(nil); err != nil {
			return nil, false, err
		}
		if _, err := m.writeFragment(nil); err != nil {
			return nil, false, err
		}
		if noOperator {
			if err := os.Remove(m.mod.RunPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, false, err
			}
			return nil, false, m.svc.StopService(ServiceID)
		}
		return nil, true, m.restart()
	}
	n := *next
	n.RetryJoin = slices.Clone(n.RetryJoin)
	if cur != nil {
		if n.Encrypt == "" && keepEncrypt {
			n.Encrypt = cur.Encrypt
		}
		if n.AgentToken == "" && keepToken {
			n.AgentToken = cur.AgentToken
		}
	}
	if errs := n.Validate(); len(errs) > 0 {
		return errs, false, errors.New("invalid managed layer")
	}
	if errs, err := m.checkManaged(&n, noOperator); err != nil {
		return errs, false, err
	}
	if err := saveManaged(&n); err != nil {
		return nil, false, err
	}
	changed, err := m.writeFragment(&n)
	if err != nil {
		return nil, false, err
	}
	if noOperator {
		if err := writeIfChanged(m.mod.RunPath(), []byte(placeholder), 0o644); err != nil {
			return nil, false, err
		}
	}
	st, err := m.svc.State(ServiceID)
	if err != nil {
		return nil, false, err
	}
	if !changed && st.State == "running" {
		return nil, false, nil
	}
	return nil, true, m.restart()
}

// checkManaged has consul validate the operator's configuration (or the
// placeholder) with janusd's fragment as managed would make it.
func (m *Manager) checkManaged(managed *Managed, noOperator bool) ([]string, error) {
	data, err := fragment(managed, m.main())
	if err != nil {
		return nil, err
	}
	frag, err := os.CreateTemp(RunDir, ".check-*-"+fragmentFile)
	if err != nil {
		return nil, err
	}
	defer os.Remove(frag.Name())
	if _, err := frag.Write(data); err != nil {
		frag.Close()
		return nil, err
	}
	if err := frag.Close(); err != nil {
		return nil, err
	}
	operator := m.mod.RunPath()
	if noOperator {
		f, err := os.CreateTemp(RunDir, ".check-*-consul.hcl")
		if err != nil {
			return nil, err
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(placeholder); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		operator = f.Name()
	}
	cmd := exec.Command(Binary, "validate", operator, frag.Name())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := oomadj.Run(cmd, oomadj.Default); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, fmt.Errorf("run consul: %w", err)
		}
		var lines []string
		for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
			l = strings.ReplaceAll(strings.ReplaceAll(l, frag.Name(), fragmentFile), operator, "consul.hcl")
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		if len(lines) == 0 {
			lines = []string{fmt.Sprintf("consul refused the managed layer (%v)", err)}
		}
		return lines, errors.New("invalid managed layer")
	}
	return nil, nil
}

// Refresh renders the managed layer again - the node's main address
// changed, the one the agent advertises when the layer names none - and
// restarts the agent if what it reads changed.
func (m *Manager) Refresh() error {
	if !m.Available() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	managed, err := LoadManaged()
	if err != nil || managed == nil || managed.AdvertiseAddr != "" {
		return err
	}
	changed, err := m.writeFragment(managed)
	if err != nil || !changed {
		return err
	}
	log.Printf("consul: the node's main address is now %q - the agent restarts to advertise it", m.main())
	return m.restart()
}

// Address is the node's address in Consul: the managed layer's, else
// its main IPv4 address.
func (m *Manager) Address() string {
	if managed, err := LoadManaged(); err == nil && managed != nil && managed.AdvertiseAddr != "" {
		return managed.AdvertiseAddr
	}
	return m.main()
}

// Token is the managed layer's agent token, "" without one.
func (m *Manager) Token() string {
	managed, err := LoadManaged()
	if err != nil || managed == nil {
		return ""
	}
	return managed.AgentToken
}

// mergeFiles fills the empty values in with the saved files.
func mergeFiles(files map[string][]byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	for n, data := range files {
		if len(data) == 0 {
			saved, err := os.ReadFile(filepath.Join(modcfg.Dir, savedFilesDir, n))
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil, fmt.Errorf("file %s is empty and none is saved under that name", n)
				}
				return nil, err
			}
			data = saved
		}
		out[n] = data
	}
	return out, nil
}

// saveFiles makes files the saved ones (on STATE, 0600: keys among them).
func saveFiles(files map[string][]byte) error {
	return replaceDir(filepath.Join(modcfg.Dir, savedFilesDir), files, true)
}

// writeRunFiles makes files the agent's.
func writeRunFiles(files map[string][]byte) error {
	return replaceDir(FilesDir(), files, false)
}

// replaceDir makes dir hold exactly files.
func replaceDir(dir string, files map[string][]byte, sync bool) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, keep := files[e.Name()]; !keep {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	for _, n := range sortedNames(files) {
		path := filepath.Join(dir, n)
		if sync {
			if err := writeSynced(path, files[n], 0o600); err != nil {
				return err
			}
		} else if err := os.WriteFile(path, files[n], 0o600); err != nil {
			return err
		}
	}
	return nil
}

// writeSynced replaces path with data durably.
func writeSynced(path string, data []byte, perm os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func sortedNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Agent is what the agent says of itself.
type Agent struct {
	NodeName   string
	NodeID     string
	Datacenter string
	Server     bool
	Version    string
	Leader     string
	Members    []Member
	// Services are the agent's services, the operator's and janusd's
	// (IDs janus:*); ServicesError says why they couldn't be read.
	Services      []AgentService
	ServicesError string
}

// Member is a member of the agent's LAN gossip pool.
type Member struct {
	Name       string
	Address    string
	Status     string // "alive", "leaving", "left", "failed"
	Role       string // "server", "client"
	Datacenter string
}

// AgentService is a service of the agent's, with its checks' worst
// state.
type AgentService struct {
	ID, Name, Address string
	Port              int
	Tags              []string
	Meta              map[string]string
	// Status: "passing", "warning", "critical", "maintenance"; "" without
	// a check.
	Status string
	Checks []AgentCheck
}

// AgentCheck is a check of a service's.
type AgentCheck struct {
	ID, Name, Status, Output string
}

// maxCheckOutput bounds what a check said, as the status reports it.
const maxCheckOutput = 512

// Managed reports whether janusd registered it.
func (s AgentService) Managed() bool { return strings.HasPrefix(s.ID, IDPrefix) }

var memberStatus = map[int]string{0: "none", 1: "alive", 2: "leaving", 3: "left", 4: "failed"}

// Agent asks the agent's HTTP API (HTTPAddr) about itself, its
// cluster's leader, its members and its services - with the managed
// layer's token, if there's one.
func (m *Manager) Agent(ctx context.Context) (*Agent, error) {
	c := &agentClient{token: m.Token()}
	var self struct {
		Config struct {
			NodeName, NodeID, Datacenter, Version string
			Server                                bool
		}
	}
	if err := c.do(ctx, http.MethodGet, "/v1/agent/self", nil, &self); err != nil {
		return nil, err
	}
	a := &Agent{NodeName: self.Config.NodeName, NodeID: self.Config.NodeID, Datacenter: self.Config.Datacenter,
		Server: self.Config.Server, Version: self.Config.Version}
	_ = c.do(ctx, http.MethodGet, "/v1/status/leader", nil, &a.Leader)
	var members []struct {
		Name   string
		Addr   string
		Port   int
		Status int
		Tags   map[string]string
	}
	if err := c.do(ctx, http.MethodGet, "/v1/agent/members", nil, &members); err == nil {
		for _, mem := range members {
			role := mem.Tags["role"]
			switch role {
			case "consul":
				role = "server"
			case "node":
				role = "client"
			}
			a.Members = append(a.Members, Member{Name: mem.Name, Address: fmt.Sprintf("%s:%d", mem.Addr, mem.Port),
				Status: memberStatus[mem.Status], Role: role, Datacenter: mem.Tags["dc"]})
		}
		slices.SortFunc(a.Members, func(x, y Member) int { return strings.Compare(x.Name, y.Name) })
	}
	services, err := c.services(ctx)
	if err != nil {
		a.ServicesError = err.Error()
		return a, nil
	}
	checks, err := c.checks(ctx)
	if err != nil {
		a.ServicesError = err.Error()
	}
	for _, s := range services {
		var cs []AgentCheck
		for _, c := range checks {
			if c.ServiceID != s.ID {
				continue
			}
			out := c.Output
			if c.CheckID == maintenanceCheck(s.ID) && c.Notes != "" {
				out = c.Notes
			}
			if len(out) > maxCheckOutput {
				out = strings.ToValidUTF8(out[:maxCheckOutput], "") + "…"
			}
			cs = append(cs, AgentCheck{ID: c.CheckID, Name: c.Name, Status: c.Status, Output: out})
		}
		slices.SortFunc(cs, func(x, y AgentCheck) int { return strings.Compare(x.ID, y.ID) })
		a.Services = append(a.Services, AgentService{ID: s.ID, Name: s.Service, Address: s.Address, Port: s.Port,
			Tags: s.Tags, Meta: s.Meta, Status: worstStatus(checks, s.ID), Checks: cs})
	}
	slices.SortFunc(a.Services, func(x, y AgentService) int { return strings.Compare(x.ID, y.ID) })
	return a, nil
}

// worstStatus is the worst state of service id's checks: maintenance
// first, then critical, warning, passing.
func worstStatus(checks map[string]agentCheck, id string) string {
	rank := map[string]int{"passing": 1, "warning": 2, "critical": 3, "maintenance": 4}
	worst := ""
	for _, c := range checks {
		if c.ServiceID != id {
			continue
		}
		st := c.Status
		if c.CheckID == maintenanceCheck(id) {
			st = "maintenance"
		}
		if rank[st] > rank[worst] {
			worst = st
		}
	}
	return worst
}

// maintenanceCheck is the check Consul adds to a service in
// maintenance.
func maintenanceCheck(id string) string { return "_service_maintenance:" + id }

// agentClient asks the local agent's HTTP API, with token if any.
type agentClient struct {
	token string
	// addr is the agent's HTTP API, HTTPAddr if empty (tests).
	addr string
}

var httpClient = &http.Client{Timeout: 5 * time.Second}

// do sends body (JSON, nil: none) and decodes the answer into into
// (nil: ignored).
func (c *agentClient) do(ctx context.Context, method, path string, body, into any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	addr := cmpOr(c.addr, HTTPAddr)
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, rd)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("X-Consul-Token", c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("the agent's HTTP API (%s): %w", addr, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode == http.StatusForbidden {
		if c.token == "" {
			return fmt.Errorf("the agent's HTTP API refuses anonymous reads (ACLs): %s", strings.TrimSpace(string(data)))
		}
		return fmt.Errorf("the agent's HTTP API refuses the managed token: %s", strings.TrimSpace(string(data)))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the agent's HTTP API: %s %s: %s: %s", method, strings.SplitN(path, "?", 2)[0], resp.Status, strings.TrimSpace(string(data)))
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(data, into)
}

// agentService is a service as /v1/agent/services lists it.
type agentService struct {
	ID      string
	Service string
	Tags    []string
	Meta    map[string]string
	Address string
	Port    int
}

type agentCheck struct {
	CheckID   string
	Name      string
	ServiceID string
	Status    string
	Output    string
	Notes     string
}

func (c *agentClient) services(ctx context.Context) (map[string]agentService, error) {
	out := map[string]agentService{}
	return out, c.do(ctx, http.MethodGet, "/v1/agent/services", nil, &out)
}

func (c *agentClient) checks(ctx context.Context) (map[string]agentCheck, error) {
	out := map[string]agentCheck{}
	return out, c.do(ctx, http.MethodGet, "/v1/agent/checks", nil, &out)
}
