// Package logforward is where the Controller has logs sent
// (internal/logship): its own - its log and its audit - and the nodes',
// through fleet profiles: each a label selector and the log targets of
// the nodes it picks, which the Controller applies to them and keeps
// applied.
//
// Kept in <data-dir>/logforward/state.json, sealed whole with the master
// key: the targets' API keys, passwords and client keys are in it.
package logforward

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"

	"github.com/swenske/Janus/dashboard/backend/internal/labels"
	"github.com/swenske/Janus/internal/logship"
)

// ControllerSources are what the Controller's own targets may take: its
// log and its audit (by default, both).
var ControllerSources = []string{logship.SourceController, logship.SourceAudit}

// Profile is the log targets of the nodes a selector picks.
type Profile struct {
	Name string `json:"name"`
	// Selector: the labels a node must have; none: every node.
	Selector map[string]string `json:"selector,omitempty"`
	Config   logship.Config    `json:"config"`
}

// Settings are where the logs go.
type Settings struct {
	// Controller is the Controller's own targets.
	Controller logship.Config `json:"controller"`
	// Profiles, in order: a node takes the first that picks it.
	Profiles []Profile `json:"profiles,omitempty"`
}

var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// Validate checks s and fills the defaults in.
func (s *Settings) Validate() error {
	for i := range s.Controller.Targets {
		t := &s.Controller.Targets[i]
		if len(t.Sources) == 0 {
			t.Sources = slices.Clone(ControllerSources)
		}
		for _, src := range t.Sources {
			if !slices.Contains(ControllerSources, src) {
				return fmt.Errorf("the Controller's target %s: source %q - the Controller has its log (controller) and its audit (audit)", t.Name, src)
			}
		}
	}
	if err := s.Controller.Validate(); err != nil {
		return fmt.Errorf("the Controller's targets: %w", err)
	}
	if s.Controller.AccessLogsOnDisk != "" || s.Controller.ManagedBy != "" {
		return errors.New("the Controller's targets: access_logs_on_disk and managed_by are a node's")
	}
	if len(s.Profiles) > 32 {
		return errors.New("32 profiles at most")
	}
	seen := map[string]bool{}
	for i := range s.Profiles {
		p := &s.Profiles[i]
		if !profileName.MatchString(p.Name) {
			return fmt.Errorf("profile %q: a name of lowercase letters, digits, '.', '_', '-'", p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("two profiles named %q", p.Name)
		}
		seen[p.Name] = true
		if err := labels.Check(p.Selector); err != nil {
			return fmt.Errorf("profile %s: %w", p.Name, err)
		}
		for _, t := range p.Config.Targets {
			if slices.Contains(t.Sources, logship.SourceController) {
				return fmt.Errorf("profile %s, target %s: a node has no %q source", p.Name, t.Name, logship.SourceController)
			}
		}
		p.Config.ManagedBy = ""
		if err := p.Config.Validate(); err != nil {
			return fmt.Errorf("profile %s: %w", p.Name, err)
		}
	}
	return nil
}

// Redacted is s without its secrets.
func (s Settings) Redacted() Settings {
	out := Settings{Controller: s.Controller.Redacted()}
	for _, p := range s.Profiles {
		p.Config = p.Config.Redacted()
		out.Profiles = append(out.Profiles, p)
	}
	return out
}

// MergeSecrets gives s the secrets saved has, target by target - the
// Controller's by name, a profile's by its name and the target's.
func (s *Settings) MergeSecrets(saved Settings) {
	s.Controller.MergeSecrets(saved.Controller)
	for i := range s.Profiles {
		for _, sp := range saved.Profiles {
			if sp.Name == s.Profiles[i].Name {
				s.Profiles[i].Config.MergeSecrets(sp.Config)
			}
		}
	}
}

// ProfileFor is the first profile that picks a node with labels l.
func (s Settings) ProfileFor(l map[string]string) (Profile, bool) {
	for _, p := range s.Profiles {
		if labels.Match(p.Selector, l) {
			return p, true
		}
	}
	return Profile{}, false
}

// ManagedBy is what a node given profile p by the Controller id carries
// as its configuration's managed_by: who, which profile, and a digest of
// the profile's configuration, secrets included - a node whose managed_by
// differs is given it again.
func ManagedBy(controllerID string, p Profile) string {
	cfg := p.Config
	cfg.ManagedBy = ""
	b, _ := json.Marshal(cfg)
	sum := sha256.Sum256(b)
	return "janus-controller " + short(controllerID) + " profile " + p.Name + " " + hex.EncodeToString(sum[:4])
}

// ManagedByPrefix is how a node's managed_by starts when this
// Controller set it.
func ManagedByPrefix(controllerID string) string {
	return "janus-controller " + short(controllerID) + " "
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Sealer seals and opens secrets with the master key.
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

const purpose = "log-forwarding"

// Store keeps the settings.
type Store struct {
	dir string
	key Sealer

	mu  sync.Mutex
	set Settings
}

type state struct {
	Sealed []byte `json:"sealed"`
}

// OpenStore reads the settings kept in dir.
func OpenStore(dir string, key Sealer) (*Store, error) {
	s := &Store{dir: dir, key: key}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("logforward state: %w", err)
	}
	plain, err := key.Open(st.Sealed, purpose)
	if err != nil {
		return nil, fmt.Errorf("logforward state: %w", err)
	}
	if err := json.Unmarshal(plain, &s.set); err != nil {
		return nil, fmt.Errorf("logforward state: %w", err)
	}
	return s, nil
}

// Settings are the settings kept, secrets included.
func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.set)
	var out Settings
	_ = json.Unmarshal(b, &out)
	return out
}

// Set replaces the settings: a target sent without a secret keeps the
// one saved.
func (s *Store) Set(next Settings) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next.MergeSecrets(s.set)
	if err := next.Validate(); err != nil {
		return Settings{}, err
	}
	plain, err := json.Marshal(next)
	if err != nil {
		return Settings{}, err
	}
	sealed, err := s.key.Seal(plain, purpose)
	if err != nil {
		return Settings{}, err
	}
	b, err := json.Marshal(state{Sealed: sealed})
	if err != nil {
		return Settings{}, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Settings{}, err
	}
	tmp := filepath.Join(s.dir, "state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return Settings{}, err
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, "state.json")); err != nil {
		return Settings{}, err
	}
	s.set = next
	return next, nil
}
