// Package clusters is the Controller's record of its clusters
// (docs/cluster.md): which nodes are members and by what name, what the
// cluster manages on them, the cluster's own CA - its key sealed with the
// master key -, the versions of its desired configuration, what each
// member has applied, and a journal of what was done.
//
// Kept under <data>/clusters/<id>/: meta.json, ca.crt, ca.key.sealed,
// versions/v<N>/, state.json, journal.jsonl - backed up with the rest of
// the data directory.
package clusters

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"
)

// Domains a cluster may manage on its members, in the order an apply
// goes through them (docs/cluster.md "Applying").
var Domains = []string{"haproxy", "files", "firewall", "vrrp", "bgp"}

// DefaultPeersPort is the members' peers listener's port.
const DefaultPeersPort = 10057

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	memberPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	varPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

// Member is a node of the cluster.
type Member struct {
	NodeID string `json:"node_id"`
	// Name is its member name: HAProxy's peer name, a DNS label.
	Name string `json:"name"`
	// Address is the IP its peers listener binds, the others reach.
	Address string `json:"address"`
	// API is its API as the other members reach it, for pulls; empty:
	// Address, port 9505.
	API string `json:"api_address,omitempty"`
	// Vars are what the configuration's templates may use for this
	// member (.Member.Vars): a VRRP priority, a BGP router ID.
	Vars   map[string]string `json:"vars,omitempty"`
	Joined time.Time         `json:"joined"`
}

// Cluster is a cluster as the Controller knows it.
type Cluster struct {
	ID        string   `json:"id"` // a UUID
	Name      string   `json:"name"`
	PeersPort int      `json:"peers_port"`
	Managed   []string `json:"managed"`
	Members   []Member `json:"members"`
	// SecretsFrom is the member the others pull certificates, keys and the
	// ticket key ring from.
	SecretsFrom string `json:"secrets_from"`
	// Desired is the version of the configuration the members should run;
	// 0 before the first.
	Desired int       `json:"desired"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Phase is the operation under way, "" when none; Error the last
	// one's failure.
	Phase string `json:"phase,omitempty"`
	Error string `json:"error,omitempty"`
}

// Member is the member named name, nil when none is.
func (c *Cluster) Member(name string) *Member {
	for i := range c.Members {
		if c.Members[i].Name == name {
			return &c.Members[i]
		}
	}
	return nil
}

// MemberOf is the member that node is, nil when it is none.
func (c *Cluster) MemberOf(nodeID string) *Member {
	for i := range c.Members {
		if c.Members[i].NodeID == nodeID {
			return &c.Members[i]
		}
	}
	return nil
}

// Manages reports whether the cluster manages domain.
func (c *Cluster) Manages(domain string) bool { return slices.Contains(c.Managed, domain) }

// Check reports what is wrong with the record.
func (c *Cluster) Check() error {
	if !namePattern.MatchString(c.Name) {
		return fmt.Errorf("the cluster name %q isn't a label of 32 lowercase letters, digits and dashes at most", c.Name)
	}
	if c.PeersPort < 1 || c.PeersPort > 65535 {
		return fmt.Errorf("peers port %d: 1 to 65535", c.PeersPort)
	}
	for _, d := range c.Managed {
		if !slices.Contains(Domains, d) {
			return fmt.Errorf("unknown domain %q (%v)", d, Domains)
		}
	}
	seen := map[string]bool{}
	nodes := map[string]bool{}
	for _, m := range c.Members {
		if !memberPattern.MatchString(m.Name) {
			return fmt.Errorf("the member name %q isn't a label of 63 lowercase letters, digits and dashes at most", m.Name)
		}
		if seen[m.Name] {
			return fmt.Errorf("two members are named %s", m.Name)
		}
		if nodes[m.NodeID] {
			return fmt.Errorf("node %s is two members", m.NodeID)
		}
		seen[m.Name], nodes[m.NodeID] = true, true
		for k := range m.Vars {
			if !varPattern.MatchString(k) {
				return fmt.Errorf("member %s: variable %q: letters, digits and _, starting with a letter", m.Name, k)
			}
		}
	}
	if c.SecretsFrom != "" && !seen[c.SecretsFrom] {
		return fmt.Errorf("the secrets come from %s, which isn't a member", c.SecretsFrom)
	}
	return nil
}

// MemberState is what a member has, as the Controller last saw it.
type MemberState struct {
	// Applied is the version it last took whole; Hashes, by domain, the
	// SHA-256 of what that version gave it - what drift is measured
	// against.
	Applied int               `json:"applied"`
	Hashes  map[string]string `json:"hashes,omitempty"`
	// Drift: the managed domains whose content isn't what was applied.
	Drift []string `json:"drift,omitempty"`
	// Released: the domains the member's own users took back
	// (ClusterRelease).
	Released []string `json:"released,omitempty"`
	// Peers: HAProxy's peers, up of total, as the member sees them.
	PeersUp    int `json:"peers_up"`
	PeersTotal int `json:"peers_total"`
	// TablesSynced: every stick table acknowledged by every peer.
	TablesSynced bool      `json:"tables_synced"`
	CertNotAfter time.Time `json:"cert_not_after,omitzero"`
	Drained      bool      `json:"drained"`
	Error        string    `json:"error,omitempty"`
	Checked      time.Time `json:"checked,omitzero"`
}

// State is what the members have.
type State struct {
	Members map[string]MemberState `json:"members"`
	// TicketsRotated is when the TLS ticket key ring last rotated.
	TicketsRotated time.Time `json:"tickets_rotated,omitzero"`
}

// Sealer seals and opens the cluster CA's key (the master key).
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

// Store keeps the clusters under dir.
type Store struct {
	dir string
	key Sealer

	mu       sync.Mutex
	clusters map[string]*Cluster
}

// Open loads the clusters kept under dataDir/clusters.
func Open(dataDir string, key Sealer) (*Store, error) {
	s := &Store{dir: filepath.Join(dataDir, "clusters"), key: key, clusters: map[string]*Cluster{}}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name(), "meta.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue // a cluster being created when the Controller stopped
		}
		if err != nil {
			return nil, err
		}
		var c Cluster
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("cluster %s: %w", e.Name(), err)
		}
		s.clusters[c.ID] = &c
	}
	return s, nil
}

func (s *Store) clusterDir(id string) string { return filepath.Join(s.dir, id) }

func clone(c *Cluster) *Cluster {
	out := *c
	out.Managed = slices.Clone(c.Managed)
	out.Members = make([]Member, len(c.Members))
	for i, m := range c.Members {
		m.Vars = cloneVars(m.Vars)
		out.Members[i] = m
	}
	return &out
}

func cloneVars(v map[string]string) map[string]string {
	if v == nil {
		return nil
	}
	out := make(map[string]string, len(v))
	for k, x := range v {
		out[k] = x
	}
	return out
}

// List is every cluster, by name.
func (s *Store) List() []*Cluster {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Cluster, 0, len(s.clusters))
	for _, c := range s.clusters {
		out = append(out, clone(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get is the cluster id.
func (s *Store) Get(id string) (*Cluster, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clusters[id]
	if !ok {
		return nil, false
	}
	return clone(c), true
}

// ClusterOf is the cluster node is a member of, "" when none.
func (s *Store) ClusterOf(nodeID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clusters {
		if c.MemberOf(nodeID) != nil {
			return c.ID
		}
	}
	return ""
}

// ErrNotFound is a cluster the store doesn't have.
var ErrNotFound = errors.New("no such cluster")

// Create records a new cluster - its id made here - with its CA, made
// here too, its key sealed. Its members join afterwards (Update).
func (s *Store) Create(name string, peersPort int, managed []string) (*Cluster, error) {
	if peersPort == 0 {
		peersPort = DefaultPeersPort
	}
	id, err := newUUID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	c := &Cluster{ID: id, Name: name, PeersPort: peersPort, Managed: slices.Clone(managed), Created: now, Updated: now}
	if err := c.Check(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.clusters {
		if o.Name == name {
			return nil, fmt.Errorf("a cluster is already named %s", name)
		}
	}
	dir := s.clusterDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := s.createCA(c); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := s.writeMeta(c); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s.clusters[id] = c
	return clone(c), nil
}

// Update changes cluster id with fn, which may refuse; the record is
// checked and written before the change counts.
func (s *Store) Update(id string, fn func(c *Cluster) error) (*Cluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.clusters[id]
	if !ok {
		return nil, ErrNotFound
	}
	next := clone(cur)
	if err := fn(next); err != nil {
		return nil, err
	}
	if err := next.Check(); err != nil {
		return nil, err
	}
	for _, m := range next.Members {
		for _, o := range s.clusters {
			if o.ID != id && o.MemberOf(m.NodeID) != nil {
				return nil, fmt.Errorf("node %s is a member of cluster %s already", m.NodeID, o.Name)
			}
		}
	}
	next.Updated = time.Now().UTC()
	if err := s.writeMeta(next); err != nil {
		return nil, err
	}
	s.clusters[id] = next
	return clone(next), nil
}

// Remove forgets cluster id, its CA, versions and journal.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clusters[id]; !ok {
		return ErrNotFound
	}
	if err := os.RemoveAll(s.clusterDir(id)); err != nil {
		return err
	}
	delete(s.clusters, id)
	return nil
}

func (s *Store) writeMeta(c *Cluster) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeDurable(filepath.Join(s.clusterDir(c.ID), "meta.json"), append(data, '\n'))
}

// State is what cluster id's members have.
func (s *Store) State(id string) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Members: map[string]MemberState{}}
	data, err := os.ReadFile(filepath.Join(s.clusterDir(id), "state.json"))
	if err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if st.Members == nil {
		st.Members = map[string]MemberState{}
	}
	return st
}

// UpdateState changes cluster id's state with fn.
func (s *Store) UpdateState(id string, fn func(st *State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clusters[id]; !ok {
		return ErrNotFound
	}
	st := State{Members: map[string]MemberState{}}
	p := filepath.Join(s.clusterDir(id), "state.json")
	if data, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if st.Members == nil {
		st.Members = map[string]MemberState{}
	}
	fn(&st)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeDurable(p, append(data, '\n'))
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// writeDurable replaces p with data: temporary file, fsync, rename,
// directory fsync.
func writeDurable(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
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
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(p))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
