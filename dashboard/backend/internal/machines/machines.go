// Package machines keeps the virtual machines the Controller creates on
// hypervisors (internal/hypervisor): what was asked for (Spec), how far
// creating it got (Phase, Events), how to find it again on the
// hypervisor (Ref), and the node it became once it registered (NodeID).
// Plain files under the data directory, like internal/store - one
// directory per machine holding meta.json, replaced atomically.
//
// A machine being created carries a one-time registration token (only
// its hash is kept): the node presents it when it announces itself, and
// ClaimToken lets the Controller admit it without the manual approval
// every other registration goes through - creating the machine was that
// approval.
package machines

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/hypervisor"
)

// Phase is where a machine stands.
type Phase string

const (
	PhasePending     Phase = "pending"
	PhaseImage       Phase = "preparing-image"
	PhaseCreating    Phase = "creating"
	PhaseRegistering Phase = "waiting-registration"
	PhaseReady       Phase = "ready"
	PhaseFailed      Phase = "failed"
	PhaseDestroying  Phase = "destroying"
	// PhaseUpdating: a ready machine being changed in place; back to
	// ready when done, with Error set if a step failed.
	PhaseUpdating Phase = "updating"
)

// Busy reports whether something is under way in this phase - work a
// restarted Controller didn't finish.
func (p Phase) Busy() bool {
	switch p {
	case PhasePending, PhaseImage, PhaseCreating, PhaseDestroying:
		return true
	}
	return false
}

// Spec is what the machine was created with.
type Spec struct {
	// Name is the node's hostname; the virtual machine is named after it
	// (with the hypervisor's prefix).
	Name         string `json:"name"`
	HypervisorID string `json:"hypervisor_id"`
	VCPUs        int    `json:"vcpus"`
	MemoryMiB    int    `json:"memory_mib"`
	// Version is the Janus release; empty means the newest one when the
	// machine is created (Machine.Version records which).
	Version    string   `json:"version,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	// Image, when set, is used instead of the release's (or image
	// factory's) image: a mirror, an air-gapped copy, a development
	// build.
	Image *ImageSource `json:"image,omitempty"`
	NICs  []NIC        `json:"nics"`
	// DNS servers; empty: those DHCP gives, if any.
	DNS []string `json:"dns,omitempty"`
	// NTP servers (at most two); empty: those DHCP gives, else
	// pool.ntp.org.
	NTP []string `json:"ntp,omitempty"`
	// ManagedBy names what manages the machine as code ("terraform"),
	// shown on its pages; Locked keeps the Controller's pages from
	// changing what that manages (its next run would undo it) - a
	// program's API token still can.
	ManagedBy string `json:"managed_by,omitempty"`
	Locked    bool   `json:"locked,omitempty"`
}

type ImageSource struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// NIC is one network interface: the hypervisor network it's attached to,
// and how the node configures it.
type NIC struct {
	Network string `json:"network"`
	// Name is the interface's name on the node ("mgmt", "eth0"...).
	Name string `json:"name"`
	// MAC is chosen by the Controller when not given.
	MAC string `json:"mac,omitempty"`
	// Mode is "dhcp", "static" or "none".
	Mode      string   `json:"mode"`
	Addresses []string `json:"addresses,omitempty"`
	Gateway   string   `json:"gateway,omitempty"`
}

// Event is one line of a machine's history, shown while it's created.
type Event struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

const maxEvents = 50

type Machine struct {
	ID    string `json:"id"`
	Spec  Spec   `json:"spec"`
	Phase Phase  `json:"phase"`
	// Error is why the machine is in PhaseFailed.
	Error string `json:"error,omitempty"`
	// Version and Schematic are what it was really created from.
	Version   string `json:"version,omitempty"`
	Schematic string `json:"schematic,omitempty"`
	// Ref is set once the virtual machine exists on the hypervisor.
	Ref *hypervisor.MachineRef `json:"ref,omitempty"`
	// NodeID is the node it registered as (internal/store).
	NodeID string  `json:"node_id,omitempty"`
	Events []Event `json:"events"`

	// The spec is kept as the node and the hypervisor really are
	// (machines_sync.go): when it was last read, why it couldn't be, and
	// the hostname the node reports (the name stays the machine's).
	SyncedAt     time.Time `json:"synced_at,omitempty"`
	SyncError    string    `json:"sync_error,omitempty"`
	NodeHostname string    `json:"node_hostname,omitempty"`

	TokenHash    string    `json:"token_hash,omitempty"`
	TokenExpires time.Time `json:"token_expires,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Log appends an event, keeping the newest maxEvents.
func (m *Machine) Log(format string, args ...any) {
	m.Events = append(m.Events, Event{Time: time.Now().UTC(), Message: fmt.Sprintf(format, args...)})
	if len(m.Events) > maxEvents {
		m.Events = m.Events[len(m.Events)-maxEvents:]
	}
}

func (m *Machine) clone() *Machine {
	raw, err := json.Marshal(m)
	if err != nil {
		panic(err) // plain data: can't fail
	}
	var c Machine
	if err := json.Unmarshal(raw, &c); err != nil {
		panic(err)
	}
	return &c
}

type Store struct {
	dir string

	mu       sync.Mutex
	machines map[string]*Machine
}

func Open(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "machines")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, machines: map[string]*Machine{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "meta.json"))
		if err != nil {
			return nil, fmt.Errorf("load machine %s: %w", e.Name(), err)
		}
		var m Machine
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("load machine %s: %w", e.Name(), err)
		}
		s.machines[m.ID] = &m
	}
	return s, nil
}

// List returns copies of every machine, oldest first.
func (s *Store) List() []*Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Machine, 0, len(s.machines))
	for _, m := range s.machines {
		out = append(out, m.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (s *Store) Get(id string) (*Machine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.machines[id]
	if !ok {
		return nil, false
	}
	return m.clone(), true
}

// Add saves a new machine, giving it an ID.
func (s *Store) Add(m *Machine) error {
	id, err := randomHex(8)
	if err != nil {
		return err
	}
	m.ID = id
	m.CreatedAt = time.Now().UTC()
	m.UpdatedAt = m.CreatedAt
	if m.Events == nil {
		m.Events = []Event{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.write(m); err != nil {
		return err
	}
	s.machines[id] = m.clone()
	return nil
}

// Update changes a machine under the store's lock and saves it: fn gets
// the current state, so concurrent updates never lose one another.
func (s *Store) Update(id string, fn func(m *Machine) error) (*Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.machines[id]
	if !ok {
		return nil, fmt.Errorf("no such machine %q", id)
	}
	next := cur.clone()
	if err := fn(next); err != nil {
		return nil, err
	}
	next.UpdatedAt = time.Now().UTC()
	if err := s.write(next); err != nil {
		return nil, err
	}
	s.machines[id] = next
	return next.clone(), nil
}

func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.machines[id]; !ok {
		return fmt.Errorf("no such machine %q", id)
	}
	delete(s.machines, id)
	return os.RemoveAll(filepath.Join(s.dir, id))
}

// NewToken gives a machine a fresh registration token valid for ttl,
// returning it - only its hash is stored.
func (s *Store) NewToken(id string, ttl time.Duration) (string, error) {
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	_, err = s.Update(id, func(m *Machine) error {
		m.TokenHash = hashToken(token)
		m.TokenExpires = time.Now().UTC().Add(ttl)
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// ClaimToken finds the machine a registration token belongs to and
// consumes the token, so it's only ever accepted once. An unknown or
// expired token claims nothing.
func (s *Store) ClaimToken(token string) (*Machine, bool) {
	if token == "" {
		return nil, false
	}
	h := []byte(hashToken(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.machines {
		if m.TokenHash == "" || subtle.ConstantTimeCompare(h, []byte(m.TokenHash)) != 1 {
			continue
		}
		next := m.clone()
		next.TokenHash, next.TokenExpires = "", time.Time{}
		if time.Now().After(m.TokenExpires) {
			next.Log("refused an expired registration token")
			_ = s.write(next)
			s.machines[m.ID] = next
			return nil, false
		}
		if err := s.write(next); err != nil {
			return nil, false
		}
		s.machines[m.ID] = next
		return next.clone(), true
	}
	return nil, false
}

func (s *Store) write(m *Machine) error {
	dir := filepath.Join(s.dir, m.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "meta.json")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
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

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
