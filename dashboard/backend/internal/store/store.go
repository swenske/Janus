// Package store is the dashboard's own node registry: plain files under a
// data directory, one subdirectory per registered node - the same style
// internal/pki already uses for a node's own PKI material, not a
// database. What's stored per node is deliberately never the user's own
// personal admin credential (that's used once, at registration, to mint
// a dedicated "service" credential via SystemService.
// GenerateClientConfiguration, then discarded - see AddNode in
// dashboard/backend/main.go) - only that freshly-issued service
// credential lives here.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Node is one registered target. ServiceCertPEM/ServiceKeyPEM are the
// dashboard's own dedicated credential for this node - never the user's
// personal one.
type Node struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Address is the node's own real gRPC ip:port. Set it only before
	// the node is added; afterwards read it with Addr and change it with
	// Store.SetAddress - a network reconfiguration can move a node.
	Address string `json:"address"`
	Port    int    `json:"port"` // this dashboard's per-node listener port
	// MachineID is set for a node the Controller created itself on a
	// hypervisor (internal/machines) - the virtual machine it runs in.
	MachineID string `json:"machine_id,omitempty"`
	// CACertPEM is the node's own CA, the Controller's pin for its server
	// certificate; ServiceCertPEM/ServiceKeyPEM, the credential the
	// Controller got when it added the node - gone once the node trusts
	// the Controller's fleet (Fleet), whose certificate it then uses. Set
	// them only before the node is added; afterwards read them with CA
	// and ServiceCredential.
	CACertPEM      []byte `json:"-"`
	ServiceCertPEM []byte `json:"-"`
	ServiceKeyPEM  []byte `json:"-"`
	Fleet          bool   `json:"-"`

	mu sync.Mutex // guards Address, the credentials and Fleet once the node is in a Store
}

// CA is the node's own CA (PEM), the pin for its server certificate.
func (n *Node) CA() []byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.CACertPEM
}

// TrustsFleet reports whether the node trusts the Controller's fleet:
// the Controller then reaches it with its fleet certificate.
func (n *Node) TrustsFleet() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Fleet
}

// ServiceCredential is the credential the Controller got when it added
// the node - nil once the node trusts the fleet.
func (n *Node) ServiceCredential() (certPEM, keyPEM []byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ServiceCertPEM, n.ServiceKeyPEM
}

// Addr is the node's current gRPC address.
func (n *Node) Addr() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Address
}

// meta is Node's on-disk, non-secret half - the cert/key material lives
// in sibling files instead, matching internal/pki's own convention of
// one file per PEM block rather than one blob that mixes secret and
// non-secret fields.
type meta struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Port      int    `json:"port"`
	MachineID string `json:"machine_id,omitempty"`
	// Fleet: the node trusts the Controller's fleet - no service
	// credential is kept for it anymore.
	Fleet bool `json:"fleet,omitempty"`
}

type Store struct {
	dir string

	mu    sync.Mutex
	nodes map[string]*Node
}

// Open loads every already-registered node from dir (creating it if this
// is the first run) - dir is expected to be a persistent volume mount in
// the Docker image, see dashboard/backend/main.go's -data-dir flag.
func Open(dir string) (*Store, error) {
	nodesDir := filepath.Join(dir, "nodes")
	if err := os.MkdirAll(nodesDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", nodesDir, err)
	}

	s := &Store{dir: dir, nodes: map[string]*Node{}}

	entries, err := os.ReadDir(nodesDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", nodesDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		node, err := loadNode(filepath.Join(nodesDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("load node %s: %w", entry.Name(), err)
		}
		s.nodes[node.ID] = node
	}
	return s, nil
}

func loadNode(dir string) (*Node, error) {
	metaBytes, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("read meta.json: %w", err)
	}
	var m meta
	if err := json.Unmarshal(metaBytes, &m); err != nil {
		return nil, fmt.Errorf("parse meta.json: %w", err)
	}

	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read ca.crt: %w", err)
	}
	n := &Node{ID: m.ID, Name: m.Name, Address: m.Address, Port: m.Port, MachineID: m.MachineID, CACertPEM: ca, Fleet: m.Fleet}
	if m.Fleet {
		return n, nil
	}
	if n.ServiceCertPEM, err = os.ReadFile(filepath.Join(dir, "service.crt")); err != nil {
		return nil, fmt.Errorf("read service.crt: %w", err)
	}
	if n.ServiceKeyPEM, err = os.ReadFile(filepath.Join(dir, "service.key")); err != nil {
		return nil, fmt.Errorf("read service.key: %w", err)
	}
	return n, nil
}

// List returns every registered node, sorted by name for stable output -
// map iteration order isn't, and this feeds a REST response directly.
func (s *Store) List() []*Node {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) Get(id string) (*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	return n, ok
}

// UsedPorts returns every port already allocated to a registered node -
// used by main.go's port-pool allocation so a restart doesn't hand out a
// port a still-registered node already owns.
func (s *Store) UsedPorts() map[int]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	used := make(map[int]bool, len(s.nodes))
	for _, n := range s.nodes {
		used[n.Port] = true
	}
	return used
}

// Add generates a random ID, writes node's files to disk (0600 - this is
// a full admin credential for the target node, even if a dashboard-
// specific one rather than the user's own personal cert), and registers
// it in memory. node.ID is set by this call, overwriting anything the
// caller passed.
func (s *Store) Add(node *Node) error {
	id, err := randomID()
	if err != nil {
		return fmt.Errorf("generate node id: %w", err)
	}
	node.ID = id

	dir := filepath.Join(s.dir, "nodes", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	metaBytes, err := json.Marshal(meta{ID: id, Name: node.Name, Address: node.Address, Port: node.Port, MachineID: node.MachineID, Fleet: node.Fleet})
	if err != nil {
		return fmt.Errorf("marshal meta.json: %w", err)
	}
	type file struct {
		name string
		data []byte
	}
	writes := []file{{"meta.json", metaBytes}, {"ca.crt", node.CACertPEM}}
	if !node.Fleet { // trusting the fleet from the start, a node has no service credential
		writes = append(writes, file{"service.crt", node.ServiceCertPEM}, file{"service.key", node.ServiceKeyPEM})
	}
	for _, w := range writes {
		if err := os.WriteFile(filepath.Join(dir, w.name), w.data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", w.name, err)
		}
	}

	s.mu.Lock()
	s.nodes[id] = node
	s.mu.Unlock()
	return nil
}

// SetAddress records that a node is now reached at addr - after a
// network reconfiguration moved it.
func (s *Store) SetAddress(id, addr string) error {
	s.mu.Lock()
	n, ok := s.nodes[id]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such node %q", id)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	m := meta{ID: n.ID, Name: n.Name, Address: addr, Port: n.Port, MachineID: n.MachineID, Fleet: n.Fleet}
	if err := s.write(id, "meta.json", mustJSON(m)); err != nil {
		return err
	}
	n.Address = addr
	return nil
}

// SetFleet records that a node trusts the Controller's fleet, and
// deletes the service credential kept for it: the fleet's certificate
// reaches it from now on.
func (s *Store) SetFleet(id string) error {
	n, ok := s.Get(id)
	if !ok {
		return fmt.Errorf("no such node %q", id)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	m := meta{ID: n.ID, Name: n.Name, Address: n.Address, Port: n.Port, MachineID: n.MachineID, Fleet: true}
	if err := s.write(id, "meta.json", mustJSON(m)); err != nil {
		return err
	}
	for _, name := range []string{"service.key", "service.crt"} {
		if err := os.Remove(filepath.Join(s.dir, "nodes", id, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	n.Fleet, n.ServiceCertPEM, n.ServiceKeyPEM = true, nil, nil
	return nil
}

// SetCA records the node's own CA anew (PEM) - after it replaced it.
func (s *Store) SetCA(id string, caPEM []byte) error {
	n, ok := s.Get(id)
	if !ok {
		return fmt.Errorf("no such node %q", id)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := s.write(id, "ca.crt", caPEM); err != nil {
		return err
	}
	n.CACertPEM = caPEM
	return nil
}

// write replaces one of a node's files: temporary file, rename.
func (s *Store) write(id, name string, data []byte) error {
	path := filepath.Join(s.dir, "nodes", id, name)
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

// Remove deletes a node's files and forgets it - the caller is
// responsible for stopping its per-node listener first (this package
// doesn't know about listeners at all, see dashboard/backend/main.go).
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	_, ok := s.nodes[id]
	delete(s.nodes, id)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such node %q", id)
	}
	return os.RemoveAll(filepath.Join(s.dir, "nodes", id))
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
