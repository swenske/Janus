// Package cluster is a node's membership of a cluster (docs/cluster.md):
// what the orchestrator set - the cluster's id and name, this node's
// member name, the members and their addresses -, the cluster's CA and
// this node's member certificate, kept on STATE next to HAProxy's own
// files, and the "peers" section janusd generates from it for HAProxy
// (peersfile.go). A node that is no member is a cluster of one: its
// hostname is the local peer name, and the generated section names it
// alone - so a configuration that says "peers janus" is valid before
// and after joining, and a reload keeps the stick tables either way.
package cluster

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/internal/pki"
)

const (
	// DefaultPeersPort is the TCP port of every member's peers listener
	// unless the cluster says otherwise: the next after the Janus
	// exporter's 10056.
	DefaultPeersPort = 10057

	// The files under Dir. HAProxy reads MemberFile and CAFile (the
	// peers listener's certificate and the CA it verifies the others
	// with) and TicketKeysFile; janusd alone reads the rest.
	MembershipFile = "cluster.json"
	MemberFile     = "member.pem"
	CAFile         = "ca.pem"
	PendingKeyFile = "pending.key"
	TicketKeysFile = "tls-ticket-keys"

	// URIPrefix starts the SAN URI a member certificate carries:
	// janus://cluster/<id>/member/<name>.
	URIPrefix = "janus://cluster/"
)

// Managed domains a cluster may lock on a member (docs/cluster.md
// "Locks and drift"): what the orchestrator applies, refused to anyone
// else while the lock holds.
var Domains = []string{"haproxy", "files", "vrrp", "bgp", "firewall"}

var (
	idPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	namePattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	memberPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// Member is one node of the cluster as every member knows it.
type Member struct {
	// Name is a DNS label, unique in the cluster: HAProxy's peer name.
	Name string `json:"name"`
	// Address is the IP its peers listener binds, the others connect to.
	Address string `json:"address"`
	// API is its API for pulls, host:port; empty: Address, port 9505.
	API string `json:"api_address,omitempty"`
}

// Membership is what this node knows of its cluster: cluster.json.
type Membership struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Member    string   `json:"member"`
	Members   []Member `json:"members"`
	PeersPort int      `json:"peers_port"`
	Managed   []string `json:"managed,omitempty"`
	ManagedBy string   `json:"managed_by,omitempty"`
	// Advisory: an admin writes through the locks with the cluster's id
	// (an orchestrator without a Controller); otherwise only the
	// Controller does.
	Advisory bool      `json:"advisory,omitempty"`
	Updated  time.Time `json:"updated"`
}

// Self is this node's entry, nil when the membership doesn't name it.
func (m *Membership) Self() *Member {
	for i := range m.Members {
		if m.Members[i].Name == m.Member {
			return &m.Members[i]
		}
	}
	return nil
}

// Peers are the other members.
func (m *Membership) Peers() []Member {
	var out []Member
	for _, mb := range m.Members {
		if mb.Name != m.Member {
			out = append(out, mb)
		}
	}
	return out
}

// Find is the member named name, nil when none is.
func (m *Membership) Find(name string) *Member {
	for i := range m.Members {
		if m.Members[i].Name == name {
			return &m.Members[i]
		}
	}
	return nil
}

// Check reports what is wrong with the membership, worded for the
// operator: nil when it is sound.
func (m *Membership) Check() error {
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("the cluster id %q isn't a lowercase UUID", m.ID)
	}
	if !namePattern.MatchString(m.Name) {
		return fmt.Errorf("the cluster name %q isn't a label of 32 lowercase letters, digits and dashes at most", m.Name)
	}
	if !memberPattern.MatchString(m.Member) {
		return fmt.Errorf("the member name %q isn't a label of 63 lowercase letters, digits and dashes at most", m.Member)
	}
	if len(m.Members) == 0 {
		return errors.New("a cluster has at least one member")
	}
	seen := map[string]bool{}
	for _, mb := range m.Members {
		if !memberPattern.MatchString(mb.Name) {
			return fmt.Errorf("the member name %q isn't a label of 63 lowercase letters, digits and dashes at most", mb.Name)
		}
		if seen[mb.Name] {
			return fmt.Errorf("two members are named %q", mb.Name)
		}
		seen[mb.Name] = true
		if ip := net.ParseIP(mb.Address); ip == nil {
			return fmt.Errorf("member %s: %q isn't an IP address", mb.Name, mb.Address)
		}
		if mb.API != "" {
			if _, _, err := net.SplitHostPort(mb.API); err != nil {
				return fmt.Errorf("member %s: the API address %q isn't host:port", mb.Name, mb.API)
			}
		}
	}
	if !seen[m.Member] {
		return fmt.Errorf("this node, %s, isn't among the members", m.Member)
	}
	if m.PeersPort < 1 || m.PeersPort > 65535 {
		return fmt.Errorf("peers port %d: 1 to 65535", m.PeersPort)
	}
	for _, d := range m.Managed {
		if !slices.Contains(Domains, d) {
			return fmt.Errorf("unknown managed domain %q (%s)", d, strings.Join(Domains, ", "))
		}
	}
	return nil
}

// MemberURI is the SAN URI a member certificate carries.
func MemberURI(clusterID, member string) string {
	return URIPrefix + clusterID + "/member/" + member
}

// ParseMemberURI reads a member certificate's SAN URI: the cluster id
// and the member name, ok false when u isn't one.
func ParseMemberURI(u *url.URL) (clusterID, member string, ok bool) {
	if u == nil || u.Scheme != "janus" || u.Host != "cluster" {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 3 || parts[1] != "member" || !idPattern.MatchString(parts[0]) || !memberPattern.MatchString(parts[2]) {
		return "", "", false
	}
	return parts[0], parts[2], true
}

// MemberIdentity is what a member certificate says, once verified
// against the cluster's CA: the cluster it belongs to and its name.
func MemberIdentity(leaf *x509.Certificate) (clusterID, member string, err error) {
	if !slices.Contains(leaf.Subject.Organization, pki.RoleClusterMember) {
		return "", "", errors.New("not a cluster member certificate")
	}
	for _, u := range leaf.URIs {
		if id, name, ok := ParseMemberURI(u); ok {
			if clusterID != "" {
				return "", "", errors.New("a member certificate naming two clusters")
			}
			clusterID, member = id, name
		}
	}
	if clusterID == "" {
		return "", "", errors.New("a member certificate without its cluster URI")
	}
	return clusterID, member, nil
}

// Store keeps the membership on STATE: Dir holds cluster.json, the
// cluster's CA, the member certificate with its key, the pending key a
// signing request made, and the TLS ticket key ring. Nothing in Dir is
// ever served by the file RPCs (internal/api.SecretPaths).
type Store struct {
	Dir string
	// Hostname is the local peer name of a node that is no member.
	Hostname func() string

	mu     sync.RWMutex
	m      *Membership // nil: no member
	caPEM  []byte
	cas    []*x509.Certificate
	member *x509.Certificate // the member certificate's leaf
	// memberTLS is the member certificate with its key, served to a
	// member pulling from this node (ServerCertificate).
	memberTLS *tls.Certificate

	// The drain (drain.go).
	drainMu sync.Mutex
	drained bool
	drainCh chan struct{}
}

// Open reads the membership kept under dir, if any.
func Open(dir string, hostname func() string) (*Store, error) {
	s := &Store{Dir: dir, Hostname: hostname}
	data, err := os.ReadFile(filepath.Join(dir, MembershipFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var m Membership
	if err := json.Unmarshal(data, &m); err != nil {
		return s, fmt.Errorf("%s: %w", MembershipFile, err)
	}
	if err := m.Check(); err != nil {
		return s, fmt.Errorf("%s: %w", MembershipFile, err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, CAFile))
	if err != nil {
		return s, fmt.Errorf("the cluster's CA: %w", err)
	}
	cas, err := parseCAs(caPEM)
	if err != nil {
		return s, err
	}
	memberPEM, err := os.ReadFile(filepath.Join(dir, MemberFile))
	if err != nil {
		return s, fmt.Errorf("the member certificate: %w", err)
	}
	leaf, _, err := parseChain(memberPEM)
	if err != nil {
		return s, fmt.Errorf("the member certificate: %w", err)
	}
	pair, err := tls.X509KeyPair(memberPEM, memberPEM)
	if err != nil {
		return s, fmt.Errorf("the member certificate: %w", err)
	}
	s.m, s.caPEM, s.cas, s.member, s.memberTLS = &m, caPEM, cas, leaf, &pair
	return s, nil
}

// MemberTLSCertificate is the member certificate with its key, for
// this node to present when it pulls from another member; nil without
// one.
func (s *Store) MemberTLSCertificate() *tls.Certificate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.memberTLS
}

// ServerCertificate is the member certificate when serverName is this
// node's member name (pki.ClusterTrust), nil otherwise.
func (s *Store) ServerCertificate(serverName string) *tls.Certificate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.m == nil || serverName == "" || serverName != s.m.Member {
		return nil
	}
	return s.memberTLS
}

// Membership is a copy of what the node knows, nil when it is no
// member.
func (s *Store) Membership() *Membership {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.m == nil {
		return nil
	}
	c := *s.m
	c.Members = slices.Clone(s.m.Members)
	c.Managed = slices.Clone(s.m.Managed)
	return &c
}

// IsMember reports whether the node belongs to a cluster.
func (s *Store) IsMember() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m != nil
}

// LocalPeer is HAProxy's local peer name: the member name, or the
// hostname of a node that is no member.
func (s *Store) LocalPeer() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.m != nil {
		return s.m.Member
	}
	return s.Hostname()
}

// CAs are the cluster's CA certificates the node pins, none when it is
// no member.
func (s *Store) CAs() []*x509.Certificate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.cas)
}

// CAPEM is the cluster's CA file as the node keeps it.
func (s *Store) CAPEM() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.caPEM)
}

// MemberCertificate is this node's member certificate, nil without one.
func (s *Store) MemberCertificate() *x509.Certificate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.member
}

// Lock reports whether the node's cluster manages domain: the cluster's
// id and name when it does, and whether its locks are advisory.
func (s *Store) Lock(domain string) (id, name string, locked, advisory bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.m == nil || !slices.Contains(s.m.Managed, domain) {
		return "", "", false, false
	}
	return s.m.ID, s.m.Name, true, s.m.Advisory
}

// Release drops domains from what the cluster manages on this node -
// every one when none is named -, durably: the ones it released.
func (s *Store) Release(domains []string) (released []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		return nil, errors.New("this node is in no cluster")
	}
	for _, d := range domains {
		if !slices.Contains(Domains, d) {
			return nil, fmt.Errorf("unknown domain %q (%s)", d, strings.Join(Domains, ", "))
		}
	}
	m := *s.m
	m.Members = slices.Clone(s.m.Members)
	m.Managed = nil
	for _, d := range s.m.Managed {
		if len(domains) == 0 || slices.Contains(domains, d) {
			released = append(released, d)
		} else {
			m.Managed = append(m.Managed, d)
		}
	}
	if len(released) == 0 {
		return nil, nil
	}
	m.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeDurable(s.Dir, MembershipFile, append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	s.m = &m
	return released, nil
}

// ClusterID is the cluster's id, empty for no member.
func (s *Store) ClusterID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.m == nil {
		return ""
	}
	return s.m.ID
}

// AcceptMember reports whether leaf, verified against the cluster's CA
// through chain, is a member certificate of this node's own cluster - and
// which member it names.
func (s *Store) AcceptMember(chain []*x509.Certificate) (member string, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.m == nil {
		return "", errors.New("this node is in no cluster")
	}
	if len(chain) < 2 {
		return "", errors.New("no certificate chain")
	}
	anchor := chain[len(chain)-1]
	if !slices.ContainsFunc(s.cas, anchor.Equal) {
		return "", errors.New("not the cluster's CA")
	}
	id, name, err := MemberIdentity(chain[0])
	if err != nil {
		return "", err
	}
	if id != s.m.ID {
		return "", fmt.Errorf("a member of another cluster (%s)", id)
	}
	return name, nil
}

// SetRequest is what ClusterSet brings.
type SetRequest struct {
	Membership
	// CAPEM: the cluster's CA certificate(s); empty: unchanged.
	CAPEM []byte
	// MemberPEM: the member certificate and its chain, for the key the
	// last NewCSR made or the current member certificate's; empty:
	// unchanged.
	MemberPEM []byte
}

// Set installs or updates the membership, durably. It reports whether
// HAProxy's peers section changed (a reload) and whether the member
// certificate did (HAProxy takes it at runtime).
func (s *Store) Set(req SetRequest) (sectionChanged, certChanged bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := req.Membership
	if m.PeersPort == 0 {
		m.PeersPort = DefaultPeersPort
	}
	if m.Updated.IsZero() {
		m.Updated = time.Now().UTC()
	}
	if err := m.Check(); err != nil {
		return false, false, err
	}
	if s.m != nil && s.m.ID != m.ID {
		return false, false, fmt.Errorf("this node is a member of cluster %s (%s): it leaves it first", s.m.Name, s.m.ID)
	}
	cas, caPEM := s.cas, s.caPEM
	if len(req.CAPEM) > 0 {
		if cas, err = parseCAs(req.CAPEM); err != nil {
			return false, false, err
		}
		caPEM = req.CAPEM
	}
	if len(cas) == 0 {
		return false, false, errors.New("the cluster's CA is required the first time")
	}
	leaf, memberPEM := s.member, []byte(nil)
	if len(req.MemberPEM) > 0 {
		key, err := s.memberKeyLocked()
		if err != nil {
			return false, false, err
		}
		var chain []*x509.Certificate
		if leaf, chain, err = parseChain(req.MemberPEM); err != nil {
			return false, false, fmt.Errorf("the member certificate: %w", err)
		}
		if err := checkMember(leaf, chain, cas, key.Public(), m.ID, m.Member); err != nil {
			return false, false, fmt.Errorf("the member certificate: %w", err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return false, false, err
		}
		memberPEM = append(slices.Clone(req.MemberPEM), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})...)
		if _, err := tls.X509KeyPair(memberPEM, memberPEM); err != nil {
			return false, false, fmt.Errorf("the member certificate: %w", err)
		}
	} else if leaf != nil {
		// The same certificate must still fit the (possibly renamed) member.
		if err := checkMember(leaf, nil, cas, leaf.PublicKey, m.ID, m.Member); err != nil {
			return false, false, fmt.Errorf("the member certificate the node has: %w (bring a new one)", err)
		}
	}
	if leaf == nil {
		return false, false, errors.New("the member certificate is required the first time: ask the node for a signing request (CertificateRequest) and bring it signed")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return false, false, err
	}
	// The files HAProxy reads first, the membership last: a power cut in
	// between leaves the old membership whole, with newer certificates
	// it is still valid for.
	if len(req.CAPEM) > 0 {
		if err := writeDurable(s.Dir, CAFile, caPEM, 0o600); err != nil {
			return false, false, err
		}
	}
	if memberPEM != nil {
		if err := writeDurable(s.Dir, MemberFile, memberPEM, 0o600); err != nil {
			return false, false, err
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return false, false, err
	}
	if err := writeDurable(s.Dir, MembershipFile, append(data, '\n'), 0o600); err != nil {
		return false, false, err
	}
	if memberPEM != nil {
		_ = os.Remove(filepath.Join(s.Dir, PendingKeyFile))
		pair, err := tls.X509KeyPair(memberPEM, memberPEM)
		if err != nil {
			return false, false, err
		}
		s.memberTLS = &pair
	}
	sectionChanged = s.m == nil || !sameSection(s.m, &m)
	certChanged = memberPEM != nil
	s.m, s.cas, s.caPEM, s.member = &m, cas, caPEM, leaf
	return sectionChanged, certChanged, nil
}

// sameSection reports whether two memberships make the same peers
// section (peersfile.go): the names, addresses and port.
func sameSection(a, b *Membership) bool {
	if a.ID != b.ID || a.Member != b.Member || a.PeersPort != b.PeersPort || len(a.Members) != len(b.Members) {
		return false
	}
	for i := range a.Members {
		if a.Members[i].Name != b.Members[i].Name || a.Members[i].Address != b.Members[i].Address {
			return false
		}
	}
	return true
}

// memberKeyLocked is the key a member certificate must be for: the
// pending one a signing request made, else the current certificate's.
func (s *Store) memberKeyLocked() (*ecdsa.PrivateKey, error) {
	for _, name := range []string{PendingKeyFile, MemberFile} {
		data, err := os.ReadFile(filepath.Join(s.Dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		key, err := privateKey(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return key, nil
	}
	return nil, errors.New("no key to certify: ask the node for a signing request first (CertificateRequest)")
}

// Leave forgets the membership: the files go, the node is a cluster of
// one again.
func (s *Store) Leave() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leaveLocked()
}

func (s *Store) leaveLocked() error {
	for _, name := range []string{MembershipFile, MemberFile, CAFile, PendingKeyFile} {
		if err := os.Remove(filepath.Join(s.Dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := syncDir(s.Dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.m, s.cas, s.caPEM, s.member, s.memberTLS = nil, nil, nil, nil, nil
	return nil
}

// Snapshot is the membership's files as they are, for Restore: what a
// Set or Leave HAProxy then refuses to run with goes back to.
type Snapshot struct {
	membership, ca, member []byte // nil: absent
}

// Snapshot captures the membership's files.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	read := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join(s.Dir, name))
		if err != nil {
			return nil
		}
		return data
	}
	return Snapshot{membership: read(MembershipFile), ca: read(CAFile), member: read(MemberFile)}
}

// Restore puts a snapshot's files back, and the membership with them.
func (s *Store) Restore(snap Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap.membership == nil {
		return s.leaveLocked()
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{CAFile, snap.ca}, {MemberFile, snap.member}, {MembershipFile, snap.membership}} {
		if f.data == nil {
			continue
		}
		if err := writeDurable(s.Dir, f.name, f.data, 0o600); err != nil {
			return err
		}
	}
	re, err := Open(s.Dir, s.Hostname)
	if err != nil {
		return err
	}
	s.m, s.cas, s.caPEM, s.member, s.memberTLS = re.m, re.cas, re.caPEM, re.member, re.memberTLS
	return nil
}

// NewCSR makes a new key for the member certificate, kept pending under
// Dir until Set brings the certificate, and returns a signing request
// for it (PEM): the hostname as its common name, the signer decides the
// rest. A new request replaces the pending key.
func (s *Store) NewCSR() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.CertificateRequest{}
	if h := s.Hostname(); len(h) <= 64 {
		tmpl.Subject = pkix.Name{CommonName: h}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := writeDurable(s.Dir, PendingKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// checkMember verifies leaf as this node's member certificate: signed
// by one of cas (through chain), for key pub, naming cluster id and
// member.
func checkMember(leaf *x509.Certificate, chain []*x509.Certificate, cas []*x509.Certificate, pub crypto.PublicKey, id, member string) error {
	k, ok := pub.(interface{ Equal(x crypto.PublicKey) bool })
	if !ok || !k.Equal(leaf.PublicKey) {
		return errors.New("not for the key this node made")
	}
	cid, name, err := MemberIdentity(leaf)
	if err != nil {
		return err
	}
	if cid != id || name != member {
		return fmt.Errorf("names %s of cluster %s, not %s of %s", name, cid, member, id)
	}
	if !slices.Contains(leaf.DNSNames, member) {
		return fmt.Errorf("doesn't carry the DNS name %q the peers check (verifyhost)", member)
	}
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	for _, ca := range cas {
		roots.AddCert(ca)
	}
	for _, c := range chain {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return fmt.Errorf("not signed by the cluster's CA: %w", err)
	}
	return nil
}

// parseCAs reads one or two CA certificates (PEM).
func parseCAs(p []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := p
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("the cluster's CA file holds a %s", block.Type)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("the cluster's CA: %w", err)
		}
		if !c.IsCA {
			return nil, fmt.Errorf("the cluster's CA file holds a certificate that isn't a CA (%s)", c.Subject.CommonName)
		}
		out = append(out, c)
	}
	switch len(out) {
	case 0:
		return nil, errors.New("the cluster's CA file holds no certificate")
	case 1, 2:
		return out, nil
	}
	return nil, fmt.Errorf("the cluster's CA file holds %d certificates: one, or two while the CA is replaced", len(out))
}

// parseChain reads a certificate and its chain (PEM): the leaf first,
// then the rest. A private key block is skipped.
func parseChain(p []byte) (leaf *x509.Certificate, chain []*x509.Certificate, err error) {
	rest := p
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, err
		}
		if leaf == nil {
			leaf = c
		} else {
			chain = append(chain, c)
		}
	}
	if leaf == nil {
		return nil, nil, errors.New("no certificate")
	}
	return leaf, chain, nil
}

// privateKey reads the ECDSA key in p (PKCS#8, PEM, among other
// blocks).
func privateKey(p []byte) (*ecdsa.PrivateKey, error) {
	rest := p
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no private key")
		}
		if block.Type != "PRIVATE KEY" {
			continue
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		ec, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("the key isn't ECDSA")
		}
		return ec, nil
	}
}

// writeDurable replaces dir/name with data so that a power cut leaves
// either the old file or the new one: temporary file, fsync, rename,
// directory fsync.
func writeDurable(dir, name string, data []byte, perm os.FileMode) error {
	tmp := filepath.Join(dir, "."+name+".tmp")
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
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
