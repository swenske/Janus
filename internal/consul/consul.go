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
// configuration) and a node ID kept on STATE, so the node stays the same
// member of its cluster across reboots.
package consul

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/modcfg"
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
	if err := writeFragment(); err != nil {
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
	return m.mod.Boot()
}

// writeFragment writes janusd's part of the configuration.
func writeFragment() error {
	id, err := nodeID()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(map[string]string{
		"data_dir": filepath.Join(RunDir, "data"),
		"node_id":  id,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(RunDir, fragmentFile), append(data, '\n'), 0o644)
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
		if err := writeFragment(); err != nil {
			return nil, err
		}
	}
	return m.mod.Check(config)
}

// Apply checks, saves and applies a configuration with its files: an
// empty value keeps a saved file, a saved file not given is removed. The
// agent restarts with it (or starts). An empty configuration removes it
// all, and stops the agent.
func (m *Manager) Apply(config string, files map[string][]byte) ([]string, error) {
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
		return nil, m.svc.StopService(ServiceID)
	}
	st, err := m.svc.State(ServiceID)
	if err != nil {
		return nil, err
	}
	switch st.State {
	case "running", "restarting":
		return nil, m.svc.RestartService(ServiceID)
	case "stopped":
		return nil, m.svc.StartService(ServiceID)
	}
	return nil, nil // waiting for the configuration it now has
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
}

// Member is a member of the agent's LAN gossip pool.
type Member struct {
	Name       string
	Address    string
	Status     string // "alive", "leaving", "left", "failed"
	Role       string // "server", "client"
	Datacenter string
}

var memberStatus = map[int]string{0: "none", 1: "alive", 2: "leaving", 3: "left", 4: "failed"}

// Agent asks the agent's HTTP API (HTTPAddr, without a token) about
// itself, its cluster's leader and its members.
func (m *Manager) Agent(ctx context.Context) (*Agent, error) {
	var self struct {
		Config struct {
			NodeName, NodeID, Datacenter, Version string
			Server                                bool
		}
	}
	if err := get(ctx, "/v1/agent/self", &self); err != nil {
		return nil, err
	}
	a := &Agent{NodeName: self.Config.NodeName, NodeID: self.Config.NodeID, Datacenter: self.Config.Datacenter,
		Server: self.Config.Server, Version: self.Config.Version}
	_ = get(ctx, "/v1/status/leader", &a.Leader)
	var members []struct {
		Name   string
		Addr   string
		Port   int
		Status int
		Tags   map[string]string
	}
	if err := get(ctx, "/v1/agent/members", &members); err == nil {
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
	return a, nil
}

var httpClient = &http.Client{Timeout: 3 * time.Second}

func get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+HTTPAddr+path, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("the agent's HTTP API (%s): %w", HTTPAddr, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the agent's HTTP API refuses anonymous reads (ACLs): %s", strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the agent's HTTP API: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, into)
}
