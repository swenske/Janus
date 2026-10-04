// Package pending holds nodes that have self-announced to the
// Controller's registration endpoint (see dashboard/backend's own
// startRegistrationListener) but haven't been approved by a human yet -
// a Tailscale-style admission gate, not fully automatic enrollment
// (the user's explicit choice once what fully-automatic would mean was
// laid out). Persisted to disk, same convention as internal/store: a
// pending entry needs to survive a Controller restart while awaiting
// approval, since the node itself only announces once per boot (see
// the node-side self-registration design) - losing it to an in-memory
// store would strand that node until its next reboot.
package pending

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Node is one self-announced, not-yet-approved node. ServiceCertPEM/
// ServiceKeyPEM are the credential the node generated for *itself* and
// sent during registration - unlike the human-driven add-node flow
// (dashboard/backend/main.go's parseAddNodeRequest), there's no
// separate bootstrap-credential-exchanged-for-a-service-credential
// step here: the node already did that part locally before announcing,
// so this is already the final shape store.Add needs on approval.
type Node struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Address        string    `json:"address"`
	CACertPEM      []byte    `json:"-"`
	ServiceCertPEM []byte    `json:"-"`
	ServiceKeyPEM  []byte    `json:"-"`
	AnnouncedAt    time.Time `json:"announced_at"`
	// PollSecretHash: a keyless announcement (protocol 2) - no service
	// credential, the SHA-256 of the secret the node polls with.
	// Approved/NodeID: approved, waiting for the node to fetch its
	// trust (not listed any more).
	PollSecretHash []byte `json:"-"`
	Approved       bool   `json:"-"`
	NodeID         string `json:"-"`
}

// Keyless reports whether n announced itself with no key (protocol 2).
func (n *Node) Keyless() bool { return len(n.PollSecretHash) > 0 }

// meta is Node's on-disk, non-secret half - same split store.go's own
// meta type uses, cert/key material in sibling files instead of one
// blob mixing secret and non-secret fields.
type meta struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Address        string    `json:"address"`
	AnnouncedAt    time.Time `json:"announced_at"`
	PollSecretHash []byte    `json:"poll_secret_sha256,omitempty"`
	Approved       bool      `json:"approved,omitempty"`
	NodeID         string    `json:"node_id,omitempty"`
}

type Store struct {
	dir string

	mu    sync.Mutex
	nodes map[string]*Node
}

// Open loads every already-pending node from dir (creating the pending/
// subdirectory if this is the first run).
func Open(dir string) (*Store, error) {
	pendingDir := filepath.Join(dir, "pending")
	if err := os.MkdirAll(pendingDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", pendingDir, err)
	}

	s := &Store{dir: dir, nodes: map[string]*Node{}}

	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pendingDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		node, err := loadNode(filepath.Join(pendingDir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("load pending node %s: %w", entry.Name(), err)
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
	n := &Node{
		ID: m.ID, Name: m.Name, Address: m.Address, AnnouncedAt: m.AnnouncedAt, CACertPEM: ca,
		PollSecretHash: m.PollSecretHash, Approved: m.Approved, NodeID: m.NodeID,
	}
	if n.Keyless() {
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

// List returns every pending node, oldest announcement first - the
// order a human triaging a queue would actually want.
func (s *Store) List() []*Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		if !n.Approved { // waiting for its node to fetch its trust: not pending any more
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AnnouncedAt.Before(out[j].AnnouncedAt) })
	return out
}

func (s *Store) Get(id string) (*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	return n, ok
}

// Add records a new self-announcement, generating its ID. A node
// re-announcing with the same address as an already-pending entry
// still gets a separate new entry - de-duplication is a human decision
// at approval time (reject the stale one), not something this store
// enforces on its own.
func (s *Store) Add(node *Node) error {
	id, err := randomID()
	if err != nil {
		return fmt.Errorf("generate pending id: %w", err)
	}
	node.ID = id
	node.AnnouncedAt = time.Now()

	dir := filepath.Join(s.dir, "pending", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	metaBytes, err := json.Marshal(meta{ID: id, Name: node.Name, Address: node.Address, AnnouncedAt: node.AnnouncedAt, PollSecretHash: node.PollSecretHash})
	if err != nil {
		return fmt.Errorf("marshal meta.json: %w", err)
	}
	type file struct {
		name string
		data []byte
	}
	writes := []file{{"meta.json", metaBytes}, {"ca.crt", node.CACertPEM}}
	if !node.Keyless() {
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

// MarkApproved records that a keyless announcement was approved as node
// nodeID: kept, unlisted, until the node fetches its trust.
func (s *Store) MarkApproved(id, nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[id]
	if !ok {
		return fmt.Errorf("no such pending node %q", id)
	}
	data, err := json.Marshal(meta{ID: n.ID, Name: n.Name, Address: n.Address, AnnouncedAt: n.AnnouncedAt, PollSecretHash: n.PollSecretHash, Approved: true, NodeID: nodeID})
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "pending", id, "meta.json")
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	n.Approved, n.NodeID = true, nodeID
	return nil
}

// RemoveApproved forgets the approved keyless announcements of node
// nodeID - once it took the fleet's trust.
func (s *Store) RemoveApproved(nodeID string) {
	s.mu.Lock()
	var ids []string
	for id, n := range s.nodes {
		if n.Approved && n.NodeID == nodeID {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		_ = s.Remove(id)
	}
}

// Remove discards a pending entry - used both by rejection and by
// approval, which reads the entry, hands its data to store.Add, and
// only then removes it from here (see dashboard/backend/main.go).
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	_, ok := s.nodes[id]
	delete(s.nodes, id)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such pending node %q", id)
	}
	return os.RemoveAll(filepath.Join(s.dir, "pending", id))
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
