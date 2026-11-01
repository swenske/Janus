package ha

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Replica is a copy of the active's data directory the standby keeps.
type Replica struct {
	// At is when it was pulled; Created when the active made it.
	At      time.Time `json:"at"`
	Created time.Time `json:"created"`
	// Epoch and Version are the active's then.
	Epoch   uint64 `json:"epoch"`
	Version string `json:"version"`
	Size    int    `json:"size"`
	SHA256  string `json:"sha256"`
}

// Pulls is what the standby knows of its pulls.
type Pulls struct {
	Last *Replica `json:"last,omitempty"`
	// LastAttempt is the last pull; LastContact the last time the active
	// answered - its state or a copy.
	LastAttempt time.Time `json:"last_attempt"`
	LastContact time.Time `json:"last_contact"`
	// PeerEpoch is the highest epoch the active said it was at.
	PeerEpoch uint64 `json:"peer_epoch"`
	Error     string `json:"error,omitempty"`
}

// SaveReplica keeps archive - the active's copy, r says of what - sealed,
// as the last copy.
func (s *Store) SaveReplica(archive []byte, r Replica) error {
	sum := sha256.Sum256(archive)
	r.Size, r.SHA256 = len(archive), hex.EncodeToString(sum[:])
	s.mu.Lock()
	key := s.key
	s.mu.Unlock()
	sealed, err := key.Seal(archive, purposeReplica)
	if err != nil {
		return err
	}
	if err := writeDurably(filepath.Join(s.dir, "replica.sealed"), sealed); err != nil {
		return err
	}
	return s.updatePulls(func(p *Pulls) {
		p.Last, p.LastAttempt, p.LastContact, p.Error = &r, r.At, r.At, ""
		p.PeerEpoch = max(p.PeerEpoch, r.Epoch)
	})
}

// NotePull records a pull that brought no copy: the active answered
// (contact, its epoch) or not, err what went wrong.
func (s *Store) NotePull(at time.Time, contact bool, peerEpoch uint64, err error) error {
	return s.updatePulls(func(p *Pulls) {
		p.LastAttempt = at
		if contact {
			p.LastContact = at
			p.PeerEpoch = max(p.PeerEpoch, peerEpoch)
		}
		p.Error = ""
		if err != nil {
			p.Error = err.Error()
		}
	})
}

func (s *Store) updatePulls(f func(*Pulls)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, "replica.json")
	var p Pulls
	if err := readJSON(path, &p); err != nil {
		return err
	}
	f(&p)
	return writeJSON(path, p)
}

// Pulls is what the standby knows of its pulls.
func (s *Store) Pulls() Pulls {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p Pulls
	_ = readJSON(filepath.Join(s.dir, "replica.json"), &p)
	return p
}

// ReadPulls reads a data directory's pulls without opening its store:
// `dashboardd ha status` beside a running Controller.
func ReadPulls(dir string) (State, Pulls, error) {
	st := State{Role: Active}
	if err := readJSON(filepath.Join(dir, "state.json"), &st); err != nil {
		return st, Pulls{}, err
	}
	if st.Role == "" {
		st.Role = Active
	}
	var p Pulls
	err := readJSON(filepath.Join(dir, "replica.json"), &p)
	return st, p, err
}

// ErrNoReplica is a standby without a copy yet.
var ErrNoReplica = errors.New("no copy of the active yet")

// LastReplica is the last copy, unsealed.
func (s *Store) LastReplica() ([]byte, Replica, error) {
	p := s.Pulls()
	sealed, err := os.ReadFile(filepath.Join(s.dir, "replica.sealed"))
	if errors.Is(err, os.ErrNotExist) || p.Last == nil {
		return nil, Replica{}, ErrNoReplica
	}
	if err != nil {
		return nil, Replica{}, err
	}
	s.mu.Lock()
	key := s.key
	s.mu.Unlock()
	archive, err := key.Open(sealed, purposeReplica)
	if err != nil {
		return nil, Replica{}, err
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != p.Last.SHA256 {
		return nil, Replica{}, errors.New("the last copy isn't the one recorded")
	}
	return archive, *p.Last, nil
}

// Seen is what the active knows of its standby's pulls.
type Seen struct {
	At      time.Time `json:"at"`
	Version string    `json:"version"`
	From    string    `json:"from"`
}

// NoteSeen records a copy the standby took.
func (s *Store) NoteSeen(seen Seen) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(filepath.Join(s.dir, "seen.json"), seen)
}

// Seen is the standby's last copy, as the active saw it go.
func (s *Store) Seen() Seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	var seen Seen
	_ = readJSON(filepath.Join(s.dir, "seen.json"), &seen)
	return seen
}

// --- the peer, over the network ---

// Headers a copy carries.
const (
	HeaderEpoch   = "X-Janus-Epoch"
	HeaderVersion = "X-Janus-Version"
	HeaderCreated = "X-Janus-Created"
)

// PeerState is what a peer's GET /ha/state answers.
type PeerState struct {
	Role    string `json:"role"`
	Epoch   uint64 `json:"epoch"`
	Version string `json:"version"`
	Name    string `json:"name"`
}

// Client reaches a peer's HA routes: TLS 1.3 to its registration port
// asking for ServerName, its certificate pinned, the peers' secret as
// the bearer.
type Client struct {
	addr, bearer string
	hc           *http.Client
	// Version is this Controller's, told to the peer with each request.
	Version string
}

// NewClient is a client of the peer at addr, known by certSHA256.
func NewClient(addr, certSHA256, bearer string) *Client {
	tlsConfig := &tls.Config{
		ServerName: ServerName,
		MinVersion: tls.VersionTLS13,
		// The peer is known by its certificate's fingerprint alone,
		// checked below on every connection, resumed or not - no CA
		// signs it.
		InsecureSkipVerify: true, //nolint:gosec // G402: pinned by VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || fingerprint(cs.PeerCertificates[0].Raw) != certSHA256 {
				return fmt.Errorf("%s isn't the peer paired: its certificate isn't the one pinned", addr)
			}
			return nil
		},
	}
	return &Client{addr: addr, bearer: bearer, hc: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil, ForceAttemptHTTP2: true}}}
}

// Client is a client of the peer paired; nil without one.
func (s *Store) Client() *Client {
	st := s.State()
	if st.Peer == nil {
		return nil
	}
	return NewClient(st.Peer.Address, st.Peer.CertSHA256, s.Bearer())
}

// Do sends a request to the peer; a status other than 2xx is an error,
// its body the message.
func (c *Client) Do(ctx context.Context, method, path string, body []byte, limit int64) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, "https://"+c.addr+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.Version != "" {
		req.Header.Set(HeaderVersion, c.Version)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("%s %s: more than %d bytes", method, path, limit)
	}
	if resp.StatusCode/100 != 2 {
		return nil, nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(data))
	}
	return data, resp.Header, nil
}

// State asks the peer where it stands.
func (c *Client) State(ctx context.Context) (PeerState, error) {
	data, _, err := c.Do(ctx, http.MethodGet, "/ha/state", nil, 1<<16)
	if err != nil {
		return PeerState{}, err
	}
	var st PeerState
	err = json.Unmarshal(data, &st)
	return st, err
}

// MaxReplica bounds a copy.
const MaxReplica = 512 << 20

// ReplicaFrom reads a copy's headers.
func ReplicaFrom(h http.Header, at time.Time) Replica {
	epoch, _ := strconv.ParseUint(h.Get(HeaderEpoch), 10, 64)
	created, _ := time.Parse(time.RFC3339Nano, h.Get(HeaderCreated))
	return Replica{At: at, Created: created, Epoch: epoch, Version: h.Get(HeaderVersion)}
}
