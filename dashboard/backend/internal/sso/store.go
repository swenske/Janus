package sso

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const settingsFile = "sso.json"

// MaxProviders bounds the sign-in page's buttons.
const MaxProviders = 8

// Sealer seals the client secrets: the master key.
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

func secretPurpose(id string) string { return "sso-client-secret:" + id }

// ErrNoProvider is an ID no provider has.
var ErrNoProvider = errors.New("no such single sign-on provider")

type stored struct {
	Provider
	SecretSealed []byte `json:"secret_sealed,omitempty"`
}

type fileContents struct {
	Providers []stored `json:"providers"`
}

// Store is the providers' settings.
type Store struct {
	path string
	key  Sealer

	mu        sync.Mutex
	providers []stored
}

// Open loads dataDir's providers.
func Open(dataDir string, key Sealer) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, settingsFile), key: key}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var c fileContents
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	s.providers = c.Providers
	return s, nil
}

// saveLocked writes sso.json whole: tmp, fsync, rename. Called with s.mu
// held.
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(fileContents{Providers: s.providers}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
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
	return os.Rename(tmp, s.path)
}

// View is a provider as the page sees it: its secret only said to exist.
type View struct {
	Provider
	HasSecret bool `json:"has_secret"`
}

// List is every provider, by ID.
func (s *Store) List() []View {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []View{}
	for _, p := range s.providers {
		out = append(out, View{Provider: clone(p.Provider), HasSecret: len(p.SecretSealed) > 0})
	}
	slices.SortFunc(out, func(a, b View) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Get is one provider.
func (s *Store) Get(id string) (Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexLocked(id)
	if i < 0 {
		return Provider{}, false
	}
	return clone(s.providers[i].Provider), true
}

// Button is a provider on the sign-in page.
type Button struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Buttons are the enabled providers, by name.
func (s *Store) Buttons() []Button {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Button{}
	for _, p := range s.providers {
		if p.Enabled {
			out = append(out, Button{p.ID, p.Name})
		}
	}
	slices.SortFunc(out, func(a, b Button) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Secret is id's client secret, opened ("" for a public client).
func (s *Store) Secret(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexLocked(id)
	if i < 0 {
		return "", ErrNoProvider
	}
	if len(s.providers[i].SecretSealed) == 0 {
		return "", nil
	}
	b, err := s.key.Open(s.providers[i].SecretSealed, secretPurpose(id))
	if err != nil {
		return "", fmt.Errorf("open the client secret: %w", err)
	}
	return string(b), nil
}

func (s *Store) indexLocked(id string) int {
	return slices.IndexFunc(s.providers, func(p stored) bool { return p.ID == id })
}

// Put saves p - a new provider, or one's new settings -, and the client
// secret when secret isn't nil ("": none, a public client). It answers
// the saved provider, its Version moved on.
func (s *Store) Put(p Provider, secret *string) (Provider, error) {
	if err := p.Validate(); err != nil {
		return Provider{}, err
	}
	var sealed []byte
	if secret != nil && *secret != "" {
		if len(*secret) > 1024 || strings.TrimSpace(*secret) != *secret {
			return Provider{}, errors.New("client_secret: up to 1024 characters, no leading or trailing space")
		}
		var err error
		if sealed, err = s.key.Seal([]byte(*secret), secretPurpose(p.ID)); err != nil {
			return Provider{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := slices.Clone(s.providers)
	i := s.indexLocked(p.ID)
	next := stored{Provider: clone(p)}
	if i < 0 {
		if len(s.providers) >= MaxProviders {
			return Provider{}, fmt.Errorf("%d providers at most", MaxProviders)
		}
		next.Version = 1
		if secret != nil {
			next.SecretSealed = sealed
		}
		s.providers = append(s.providers, next)
		i = len(s.providers) - 1
	} else {
		next.Version = s.providers[i].Version + 1
		next.SecretSealed = s.providers[i].SecretSealed
		if secret != nil {
			next.SecretSealed = sealed
		}
		s.providers[i] = next
	}
	if s.providers[i].DisablePKCE && len(s.providers[i].SecretSealed) == 0 {
		s.providers = old
		return Provider{}, errors.New("a public client (no secret) is bound to its sign-ins by PKCE alone: keep PKCE on")
	}
	if err := s.saveLocked(); err != nil {
		s.providers = old
		return Provider{}, err
	}
	return clone(s.providers[i].Provider), nil
}

// Delete removes a provider and its secret.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexLocked(id)
	if i < 0 {
		return ErrNoProvider
	}
	old := slices.Clone(s.providers)
	s.providers = slices.Delete(s.providers, i, i+1)
	if err := s.saveLocked(); err != nil {
		s.providers = old
		return err
	}
	return nil
}

// clone is p without slices or maps shared with the store's copy.
func clone(p Provider) Provider {
	p.Scopes = slices.Clone(p.Scopes)
	p.ACR = slices.Clone(p.ACR)
	p.AMR = slices.Clone(p.AMR)
	rules := make([]Rule, len(p.Rules))
	for i, r := range p.Rules {
		r.Domains = slices.Clone(r.Domains)
		if r.Selector != nil {
			sel := make(map[string]string, len(r.Selector))
			for k, v := range r.Selector {
				sel[k] = v
			}
			r.Selector = sel
		}
		rules[i] = r
	}
	p.Rules = rules
	return p
}
