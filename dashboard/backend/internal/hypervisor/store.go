package hypervisor

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Hypervisor is one configured hypervisor. SSHKey is the key the
// Controller generated to log in to it - never shown, only its public
// half (AuthorizedKey).
type Hypervisor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// ControllerAddress is where machines on this hypervisor reach the
	// Controller's registration endpoint ("host:port"). Empty: the
	// Controller's own guess (-advertise-address and -register-addr).
	ControllerAddress string         `json:"controller_address,omitempty"`
	Libvirt           *LibvirtConfig `json:"libvirt,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`

	SSHKey ed25519.PrivateKey `json:"-"`
}

// LibvirtConfig reaches a libvirt daemon over SSH, as a dedicated user
// whose access to libvirt is ideally restricted to the pool, networks
// and name prefix below (docs/hypervisors.md, polkit).
type LibvirtConfig struct {
	// Host is the SSH server: "host" or "host:port".
	Host string `json:"host"`
	User string `json:"user"`
	// Socket is libvirt's socket on the host. Empty: DefaultSocket.
	Socket string `json:"socket,omitempty"`
	// HostKey is the host's SSH public key, in authorized_keys form,
	// pinned once the operator confirmed its fingerprint. Empty: not
	// trusted yet - nothing but a probe connects.
	HostKey string `json:"host_key,omitempty"`
	// Pool is the storage pool (type dir) images and disks go to.
	Pool string `json:"pool"`
	// Networks lists the libvirt networks machines may be attached to.
	Networks []string `json:"networks"`
	// NamePrefix starts every virtual machine's name. Empty:
	// DefaultNamePrefix.
	NamePrefix string `json:"name_prefix,omitempty"`
}

const (
	DefaultSocket     = "/var/run/libvirt/libvirt-sock"
	DefaultNamePrefix = "janus-"
)

// SocketPath is the libvirt socket to use.
func (c *LibvirtConfig) SocketPath() string {
	if c.Socket != "" {
		return c.Socket
	}
	return DefaultSocket
}

// Prefix is the virtual machine name prefix to use.
func (c *LibvirtConfig) Prefix() string {
	if c.NamePrefix != "" {
		return c.NamePrefix
	}
	return DefaultNamePrefix
}

// SSHAddress is Host with SSH's default port when it has none.
func (c *LibvirtConfig) SSHAddress() string {
	if _, _, err := net.SplitHostPort(c.Host); err == nil {
		return c.Host
	}
	return net.JoinHostPort(c.Host, "22")
}

// AllowsNetwork reports whether machines may be attached to network.
func (c *LibvirtConfig) AllowsNetwork(network string) bool {
	for _, n := range c.Networks {
		if n == network {
			return true
		}
	}
	return false
}

var (
	nameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	prefixRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)
	userRe   = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
)

// Validate checks a configuration before it's saved.
func (h *Hypervisor) Validate() error {
	if strings.TrimSpace(h.Name) == "" {
		return errors.New("a name is required")
	}
	if h.ControllerAddress != "" {
		if host, port, err := net.SplitHostPort(h.ControllerAddress); err != nil || host == "" || port == "" {
			return fmt.Errorf("controller address %q isn't host:port", h.ControllerAddress)
		}
	}
	switch h.Kind {
	case KindLibvirt:
		c := h.Libvirt
		if c == nil {
			return errors.New("libvirt settings are required")
		}
		if c.Host == "" {
			return errors.New("host is required")
		}
		if host, port, err := net.SplitHostPort(c.SSHAddress()); err != nil || host == "" || port == "" {
			return fmt.Errorf("host %q isn't a host or host:port", c.Host)
		}
		if !userRe.MatchString(c.User) {
			return fmt.Errorf("user %q isn't a valid user name", c.User)
		}
		if c.Socket != "" && !filepath.IsAbs(c.Socket) {
			return errors.New("socket must be an absolute path")
		}
		if !nameRe.MatchString(c.Pool) {
			return fmt.Errorf("pool %q isn't a valid storage pool name", c.Pool)
		}
		if len(c.Networks) == 0 {
			return errors.New("at least one network is required")
		}
		for _, n := range c.Networks {
			if !nameRe.MatchString(n) {
				return fmt.Errorf("network %q isn't a valid network name", n)
			}
		}
		if c.NamePrefix != "" && !prefixRe.MatchString(c.NamePrefix) {
			return fmt.Errorf("name prefix %q: letters, digits, '_', '.' and '-' only", c.NamePrefix)
		}
		if c.HostKey != "" {
			if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.HostKey)); err != nil {
				return fmt.Errorf("host key: %w", err)
			}
		}
	default:
		return fmt.Errorf("unknown kind %q (want %q)", h.Kind, KindLibvirt)
	}
	return nil
}

// AuthorizedKey is the line to add to the hypervisor user's
// ~/.ssh/authorized_keys.
func (h *Hypervisor) AuthorizedKey() string {
	signer, err := ssh.NewSignerFromKey(h.SSHKey)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	return line + " janus-controller-" + h.ID
}

// Signer is the SSH identity to log in with.
func (h *Hypervisor) Signer() (ssh.Signer, error) {
	return ssh.NewSignerFromKey(h.SSHKey)
}

func (h *Hypervisor) clone() *Hypervisor {
	c := *h
	if h.Libvirt != nil {
		l := *h.Libvirt
		l.Networks = append([]string(nil), h.Libvirt.Networks...)
		c.Libvirt = &l
	}
	return &c
}

// Store keeps the configured hypervisors under <data-dir>/hypervisors,
// one directory each: meta.json and ssh.key (0600).
type Store struct {
	dir string

	mu  sync.Mutex
	hvs map[string]*Hypervisor
}

func Open(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "hypervisors")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, hvs: map[string]*Hypervisor{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		h, err := load(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("load hypervisor %s: %w", e.Name(), err)
		}
		s.hvs[h.ID] = h
	}
	return s, nil
}

func load(dir string) (*Hypervisor, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var h Hypervisor
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("parse meta.json: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ssh.key"))
	if err != nil {
		return nil, err
	}
	key, err := ssh.ParseRawPrivateKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse ssh.key: %w", err)
	}
	edKey, ok := key.(*ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("ssh.key is a %T, want ed25519", key)
	}
	h.SSHKey = *edKey
	return &h, nil
}

// List returns copies of every hypervisor, by name.
func (s *Store) List() []*Hypervisor {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Hypervisor, 0, len(s.hvs))
	for _, h := range s.hvs {
		out = append(out, h.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns a copy of one hypervisor.
func (s *Store) Get(id string) (*Hypervisor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hvs[id]
	if !ok {
		return nil, false
	}
	return h.clone(), true
}

// Add validates h, gives it an ID and a fresh SSH key, and saves it.
func (s *Store) Add(h *Hypervisor) error {
	if err := h.Validate(); err != nil {
		return err
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	h.ID, h.SSHKey, h.CreatedAt = id, key, now()

	dir := filepath.Join(s.dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(key, "janus-controller-"+id)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "ssh.key"), pem.EncodeToMemory(block)); err != nil {
		return err
	}
	if err := s.writeMeta(h); err != nil {
		return err
	}
	s.mu.Lock()
	s.hvs[id] = h.clone()
	s.mu.Unlock()
	return nil
}

// Update saves a changed configuration (its ID, kind and key are kept).
func (s *Store) Update(h *Hypervisor) error {
	if err := h.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.hvs[h.ID]
	if !ok {
		return fmt.Errorf("no such hypervisor %q", h.ID)
	}
	next := h.clone()
	next.Kind, next.SSHKey, next.CreatedAt = cur.Kind, cur.SSHKey, cur.CreatedAt
	if err := s.writeMeta(next); err != nil {
		return err
	}
	s.hvs[h.ID] = next
	return nil
}

func (s *Store) Remove(id string) error {
	s.mu.Lock()
	_, ok := s.hvs[id]
	delete(s.hvs, id)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such hypervisor %q", id)
	}
	return os.RemoveAll(filepath.Join(s.dir, id))
}

func (s *Store) writeMeta(h *Hypervisor) error {
	raw, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, h.ID, "meta.json"), append(raw, '\n'))
}

// writeFileAtomic replaces path with data, 0600: a crash leaves the old
// file or the new one, never a torn one.
func writeFileAtomic(path string, data []byte) error {
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
	return os.Rename(tmp, path)
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
