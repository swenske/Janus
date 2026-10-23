// Package acmeorder is the Controller's side of the certificates it
// orders for its nodes (path CONTROLLER_ORDERED, docs/guide/
// letsencrypt-controller.md "Ordering for the nodes"): its ACME account
// and DNS providers, sealed with the master key, and what it did for
// each node's certificate. The node makes each key and its request,
// which the Controller orders from the CA the node's configuration names
// (cmd/janus-acme), and installs what comes back: the keys never leave
// the node.
package acmeorder

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme"
)

// Sealer seals and opens secrets with the master key.
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

const purpose = "acme-ordering"

// Settings are what an admin sets.
type Settings struct {
	// Email is the account's contact address, optional.
	Email string `json:"email,omitempty"`
	// AcceptTerms: the CA's terms of service accepted - nothing is
	// ordered until they are.
	AcceptTerms bool `json:"accept_terms"`
	// External account binding, for the CAs that require one.
	EABKeyID   string `json:"eab_key_id,omitempty"`
	EABHMACKey string `json:"eab_hmac_key,omitempty"` // secret
	// DNSProviders are what a node's dns-01 certificate names, by name.
	DNSProviders []DNSProvider `json:"dns_providers,omitempty"`
}

// DNSProvider is a DNS provider the Controller answers DNS-01 with: its
// settings are secrets.
type DNSProvider struct {
	Name                   string            `json:"name"`
	Type                   string            `json:"type"`
	Settings               map[string]string `json:"settings"`
	Resolvers              []string          `json:"resolvers,omitempty"`
	PropagationWaitSeconds uint32            `json:"propagation_wait_seconds,omitempty"`
}

// Provider is the provider named name.
func (s Settings) Provider(name string) (DNSProvider, bool) {
	for _, p := range s.DNSProviders {
		if p.Name == name {
			return p, true
		}
	}
	return DNSProvider{}, false
}

// Redacted is s without its secrets: the setting names kept, their
// values empty.
func (s Settings) Redacted() Settings {
	out := s.clone()
	out.EABHMACKey = ""
	for i := range out.DNSProviders {
		for k := range out.DNSProviders[i].Settings {
			out.DNSProviders[i].Settings[k] = ""
		}
	}
	return out
}

func (s Settings) clone() Settings {
	b, _ := json.Marshal(s)
	var out Settings
	_ = json.Unmarshal(b, &out)
	return out
}

// mergeSecrets fills what s leaves empty from saved: the EAB key for the
// same key ID, a provider's setting for the same provider and name.
func (s *Settings) mergeSecrets(saved Settings) {
	if s.EABHMACKey == "" && s.EABKeyID == saved.EABKeyID {
		s.EABHMACKey = saved.EABHMACKey
	}
	for i, p := range s.DNSProviders {
		old, ok := saved.Provider(p.Name)
		if !ok || old.Type != p.Type {
			continue
		}
		for k, v := range p.Settings {
			if v == "" {
				s.DNSProviders[i].Settings[k] = old.Settings[k]
			}
		}
	}
}

// Validate says what's wrong with s: its DNS providers are checked as a
// node checks its own.
func (s Settings) Validate() []string {
	var errs []string
	cfg := &janusv1alpha1.ACMEConfig{
		Account: &janusv1alpha1.ACMEAccount{Email: s.Email, EabKeyId: s.EABKeyID, EabHmacKey: s.EABHMACKey},
	}
	for _, p := range s.DNSProviders {
		cfg.DnsProviders = append(cfg.DnsProviders, &janusv1alpha1.ACMEDNSProvider{
			Name: p.Name, Type: p.Type, Settings: p.Settings, Resolvers: p.Resolvers, PropagationWaitSeconds: p.PropagationWaitSeconds,
		})
	}
	for _, e := range acme.Validate(cfg) {
		errs = append(errs, strings.TrimPrefix(e, "account: "))
	}
	return errs
}

// Order is what the Controller did for one node's certificate.
type Order struct {
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	Failures    int       `json:"failures,omitempty"`
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	Serial      string    `json:"serial,omitempty"`
	NotAfter    time.Time `json:"not_after,omitzero"`
}

// Failed counts a failure: the next attempt after the backoff.
func (o *Order) Failed(err string, now time.Time) {
	o.LastAttempt, o.LastError = now, err
	o.Failures++
	o.NextAttempt = now.Add(acme.Backoff(o.Failures))
}

// Succeeded clears the failures.
func (o *Order) Succeeded(serial string, notAfter, now time.Time) {
	o.LastAttempt, o.LastSuccess, o.LastError, o.Failures, o.NextAttempt = now, now, "", 0, time.Time{}
	o.Serial, o.NotAfter = serial, notAfter
}

// Store keeps the settings, the account key (both sealed) and the
// orders, in dir.
type Store struct {
	dir string
	key Sealer

	mu     sync.Mutex
	sealed sealedState
	orders map[string]*Order // by OrderKey
}

type sealedState struct {
	Settings   Settings `json:"settings"`
	AccountKey string   `json:"account_key"` // PEM
}

type stateFile struct {
	Sealed []byte `json:"sealed"`
}

// OrderKey is the key of node's certificate name.
func OrderKey(node, name string) string { return node + "/" + name }

// Open reads what dir holds; a new account key the first time.
func Open(dir string, key Sealer) (*Store, error) {
	s := &Store{dir: dir, key: key, orders: map[string]*Order{}}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		pemKey, err := acme.NewAccountKey()
		if err != nil {
			return nil, err
		}
		s.sealed.AccountKey = string(pemKey)
		if err := s.saveSealed(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		var st stateFile
		if err := json.Unmarshal(b, &st); err != nil {
			return nil, fmt.Errorf("acme ordering state: %w", err)
		}
		plain, err := key.Open(st.Sealed, purpose)
		if err != nil {
			return nil, fmt.Errorf("acme ordering state: %w", err)
		}
		if err := json.Unmarshal(plain, &s.sealed); err != nil {
			return nil, fmt.Errorf("acme ordering state: %w", err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "orders.json")); err == nil {
		if err := json.Unmarshal(b, &s.orders); err != nil {
			return nil, fmt.Errorf("acme orders: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

// Settings are the settings, secrets included.
func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealed.Settings.clone()
}

// AccountKey is the account's key, PEM.
func (s *Store) AccountKey() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []byte(s.sealed.AccountKey)
}

// Thumbprint is the account key's thumbprint.
func (s *Store) Thumbprint() string {
	t, _ := acme.AccountThumbprint(s.AccountKey())
	return t
}

// Set replaces the settings: secrets sent empty keep the saved ones.
func (s *Store) Set(next Settings) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next.Email = strings.TrimSpace(next.Email)
	next.mergeSecrets(s.sealed.Settings)
	if errs := next.Validate(); len(errs) > 0 {
		return Settings{}, errors.New(strings.Join(errs, "; "))
	}
	prev := s.sealed.Settings
	s.sealed.Settings = next
	if err := s.saveSealed(); err != nil {
		s.sealed.Settings = prev
		return Settings{}, err
	}
	return next.clone(), nil
}

// Order is node's certificate name's order record, a copy.
func (s *Store) Order(node, name string) Order {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.orders[OrderKey(node, name)]; o != nil {
		return *o
	}
	return Order{}
}

// SetOrder records o.
func (s *Store) SetOrder(node, name string, o Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[OrderKey(node, name)] = &o
	return s.saveOrders()
}

// Forget drops the orders of node's certificates not in keep.
func (s *Store) Forget(node string, keep []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k := range s.orders {
		n, name, _ := strings.Cut(k, "/")
		if n == node && !contains(keep, name) {
			delete(s.orders, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.saveOrders()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Nodes are the nodes with orders recorded.
func (s *Store) Nodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for k := range s.orders {
		n, _, _ := strings.Cut(k, "/")
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Store) saveSealed() error {
	plain, err := json.Marshal(s.sealed)
	if err != nil {
		return err
	}
	sealed, err := s.key.Seal(plain, purpose)
	if err != nil {
		return err
	}
	b, err := json.Marshal(stateFile{Sealed: sealed})
	if err != nil {
		return err
	}
	return writeDurable(filepath.Join(s.dir, "state.json"), b)
}

func (s *Store) saveOrders() error {
	b, err := json.MarshalIndent(s.orders, "", "  ")
	if err != nil {
		return err
	}
	return writeDurable(filepath.Join(s.dir, "orders.json"), b)
}

// writeDurable replaces path with data: tmp, fsync, rename, the
// directory's fsync.
func writeDurable(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
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
