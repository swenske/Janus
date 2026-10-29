// Package consulctl is the Controller's side of the nodes' Consul agents
// (docs/guide/consul-controller.md): the Consul server an admin points
// it at, with a token it mints the nodes' own tokens with - one per
// node, its node identity and the policy janus-nodes - and the layer it
// pushes to every node with the consul extension (NetworkService/
// ConsulManagedSet): the datacenter, the servers to join and the gossip
// key, read from Consul itself, the servers' RPC CA, the node's token,
// its labels as node metadata. Settings and tokens are sealed with the
// master key.
package consulctl

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/labels"
	"github.com/swenske/Janus/internal/tlstrust"
)

// Sealer seals and opens secrets with the master key.
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

const purpose = "consul"

// Settings are what an admin sets.
type Settings struct {
	Enabled bool `json:"enabled"`
	// Address is the Consul server's HTTP API, https://consul.example
	// (:8501, or behind a reverse proxy on 443).
	Address string `json:"address"`
	// TLS is how the server's certificate is trusted: the system's
	// store, a CA, a fingerprint.
	TLS Trust `json:"tls"`
	// Token is the Controller's own (secret): acl = "write" (the nodes'
	// tokens and their policy), keyring = "read" (the gossip key),
	// operator = "read", node_prefix "" write (deregistering a node
	// removed), service_prefix and agent_prefix "" read.
	Token string `json:"token,omitempty"`
	// RPCCAPEM is the CA of the servers' RPC certificates, given to the
	// nodes: their agents verify the servers with it and get their own
	// certificate from them (auto_encrypt). Empty: RPC without TLS.
	RPCCAPEM string `json:"rpc_ca_pem,omitempty"`
	// Selector picks the nodes, by their labels: empty, every node with
	// the consul extension.
	Selector map[string]string `json:"selector,omitempty"`
	// Services are which of their services the nodes register.
	Services Toggles `json:"services"`
	// NodeMetaFromLabels: a node's labels are its node metadata in
	// Consul ('.' and '/' in their keys as '_').
	NodeMetaFromLabels bool `json:"node_meta_from_labels"`
}

// Toggles are the node's services it registers.
type Toggles struct {
	Janus     bool `json:"janus"`
	Exporters bool `json:"exporters"`
	Frontends bool `json:"frontends"`
	URLs      bool `json:"urls"`
}

// Trust is tlstrust.Trust without a client certificate.
type Trust struct {
	CAPEM       string `json:"ca_pem,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	ServerName  string `json:"server_name,omitempty"`
}

// TLS is t as tlstrust has it.
func (t Trust) TLS() tlstrust.Trust {
	return tlstrust.Trust{CAPEM: t.CAPEM, Fingerprint: t.Fingerprint, ServerName: t.ServerName}
}

// Defaults are the settings before an admin's: off, every service, the
// labels as metadata.
func Defaults() Settings {
	return Settings{Services: Toggles{Janus: true, Exporters: true, Frontends: true, URLs: true}, NodeMetaFromLabels: true}
}

// Redacted is s without its token.
func (s Settings) Redacted() Settings {
	out := s.clone()
	out.Token = ""
	return out
}

func (s Settings) clone() Settings {
	b, _ := json.Marshal(s)
	var out Settings
	_ = json.Unmarshal(b, &out)
	return out
}

// MergeSecrets fills the token s leaves empty from saved's, for the
// same address.
func (s *Settings) MergeSecrets(saved Settings) {
	if s.Token == "" && strings.TrimRight(s.Address, "/") == strings.TrimRight(saved.Address, "/") {
		s.Token = saved.Token
	}
}

var tokenPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Validate says what's wrong with s; enabled, it needs an address and a
// token.
func (s Settings) Validate() []string {
	var errs []string
	if s.Address != "" {
		u, err := url.Parse(s.Address)
		if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
			errs = append(errs, fmt.Sprintf("address %q: the server's https:// URL, nothing after the host and port", s.Address))
		}
	} else if s.Enabled {
		errs = append(errs, "address: the Consul server's https:// URL")
	}
	if err := s.TLS.TLS().Validate(); err != nil {
		errs = append(errs, "tls: "+err.Error())
	}
	if s.Token != "" && !tokenPattern.MatchString(s.Token) {
		errs = append(errs, "token: a token's secret ID (a UUID)")
	} else if s.Token == "" && s.Enabled {
		errs = append(errs, "token: the Controller's own token")
	}
	if s.RPCCAPEM != "" {
		if err := checkCerts(s.RPCCAPEM); err != nil {
			errs = append(errs, "rpc_ca_pem: "+err.Error())
		}
	}
	if err := labels.Check(s.Selector); err != nil {
		errs = append(errs, "selector: "+err.Error())
	}
	return errs
}

func checkCerts(s string) error {
	rest, n := []byte(s), 0
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return fmt.Errorf("a %s block: certificates only", b.Type)
		}
		if _, err := x509.ParseCertificate(b.Bytes); err != nil {
			return err
		}
		n++
	}
	if n == 0 || strings.TrimSpace(string(rest)) != "" {
		return errors.New("not PEM certificates")
	}
	return nil
}

// Picks reports whether s picks a node with labels l.
func (s Settings) Picks(l map[string]string) bool { return labels.Match(s.Selector, l) }

// NodeToken is the token the Controller made for a node.
type NodeToken struct {
	// NodeName and Datacenter are its node identity's.
	NodeName   string `json:"node_name"`
	Datacenter string `json:"datacenter"`
	AccessorID string `json:"accessor_id"`
	SecretID   string `json:"secret_id"` // secret
	// Server is the address it was made on: another server, another
	// token.
	Server  string    `json:"server"`
	Created time.Time `json:"created"`
	// Removed: the node is gone - the token is deleted and the node
	// deregistered at the next pass.
	Removed bool `json:"removed,omitempty"`
}

// Store keeps the settings and the nodes' tokens, sealed, in dir.
type Store struct {
	dir string
	key Sealer

	mu     sync.Mutex
	sealed sealedState
}

type sealedState struct {
	Settings Settings              `json:"settings"`
	PolicyID string                `json:"policy_id,omitempty"`
	Tokens   map[string]*NodeToken `json:"tokens,omitempty"` // by node ID
}

type stateFile struct {
	Sealed []byte `json:"sealed"`
}

// Open reads what dir holds - the defaults the first time.
func Open(dir string, key Sealer) (*Store, error) {
	s := &Store{dir: dir, key: key, sealed: sealedState{Settings: Defaults(), Tokens: map[string]*NodeToken{}}}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("consul state: %w", err)
	}
	plain, err := key.Open(st.Sealed, purpose)
	if err != nil {
		return nil, fmt.Errorf("consul state: %w", err)
	}
	if err := json.Unmarshal(plain, &s.sealed); err != nil {
		return nil, fmt.Errorf("consul state: %w", err)
	}
	if s.sealed.Tokens == nil {
		s.sealed.Tokens = map[string]*NodeToken{}
	}
	return s, nil
}

// Settings are the settings, the token included.
func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealed.Settings.clone()
}

// Set replaces the settings: a token sent empty keeps the saved one
// (same address).
func (s *Store) Set(next Settings) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next.Address = strings.TrimRight(strings.TrimSpace(next.Address), "/")
	next.Token = strings.TrimSpace(next.Token)
	next.MergeSecrets(s.sealed.Settings)
	if errs := next.Validate(); len(errs) > 0 {
		return Settings{}, errors.New(strings.Join(errs, "; "))
	}
	prev := s.sealed.Settings
	s.sealed.Settings = next
	if err := s.saveLocked(); err != nil {
		s.sealed.Settings = prev
		return Settings{}, err
	}
	return next.clone(), nil
}

// PolicyID is the janus-nodes policy's ID, as last seen.
func (s *Store) PolicyID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealed.PolicyID
}

// SetPolicyID records it.
func (s *Store) SetPolicyID(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed.PolicyID == id {
		return nil
	}
	s.sealed.PolicyID = id
	return s.saveLocked()
}

// Token is node's token, a copy; false: none.
func (s *Store) Token(node string) (NodeToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.sealed.Tokens[node]
	if !ok {
		return NodeToken{}, false
	}
	return *t, true
}

// SetToken records node's token.
func (s *Store) SetToken(node string, t NodeToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.sealed.Tokens[node]
	s.sealed.Tokens[node] = &t
	if err := s.saveLocked(); err != nil {
		if had {
			s.sealed.Tokens[node] = prev
		} else {
			delete(s.sealed.Tokens, node)
		}
		return err
	}
	return nil
}

// MarkRemoved has node's token deleted at the next pass: the node is
// gone.
func (s *Store) MarkRemoved(node string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.sealed.Tokens[node]
	if !ok || t.Removed {
		return nil
	}
	t.Removed = true
	return s.saveLocked()
}

// Forget drops node's token record: deleted in Consul.
func (s *Store) Forget(node string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sealed.Tokens[node]; !ok {
		return nil
	}
	delete(s.sealed.Tokens, node)
	return s.saveLocked()
}

// Nodes are the nodes with a token, sorted.
func (s *Store) Nodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sealed.Tokens))
	for id := range s.sealed.Tokens {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (s *Store) saveLocked() error {
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
