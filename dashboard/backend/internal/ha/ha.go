// Package ha is a Controller's warm standby (docs/guide/controller-standby.md):
// two Controllers paired, one active - its loops, its nodes - and one
// standby that pulls a copy of the active's data directory every minute
// and does nothing else. Promoting the standby - by hand, never by itself
// - restores the last copy (or a final one the active hands over) and
// starts it as the active, one epoch above.
//
// The epoch fences the old active: whichever of the two sees its peer
// active at a higher epoch than its own steps down - at its start, before
// any loop, and every 30 s while it runs -, its peer saying so or the
// witness (witness.go), which both reach when they can't reach each
// other. Without one, a promotion forced while the old active still runs
// behind a partition leaves two actives until one sees the other.
//
// What it keeps, in <data>/ha - left out of backups and copies, each
// Controller's own:
//
//	state.json        role, epoch, the peer (address, its HA certificate's SHA-256)
//	secret.sealed     the secret both peers share: a request's bearer
//	ha.crt, ha.key.sealed  this Controller's HA certificate - what its peer pins
//	pairing.json      the active's pending pairing (a token's secret, hashed)
//	replica.sealed, replica.json  the standby's last copy and what it knows of it
//	previous/         a data directory set aside when its Controller became a standby
//
// Sealed files are sealed with the Controller's master key.
package ha

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Roles.
const (
	Active  = "active"
	Standby = "standby"
)

// ServerName is what a peer asks the registration port for (SNI): its
// HA certificate, the one the other pins - not the identity nodes see.
const ServerName = "ha.controller.janus"

// Dir is where a data directory keeps it.
const Dir = "ha"

// Sealer is the master key (internal/secrets).
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

const (
	purposeKey     = "ha/key"
	purposeSecret  = "ha/secret"
	purposeReplica = "ha/replica"
)

// Peer is the other Controller.
type Peer struct {
	Name string `json:"name"`
	// Address is its registration port, HOST:PORT: where its HA routes are.
	Address string `json:"address"`
	// CertSHA256 is its HA certificate's: what it's known by.
	CertSHA256 string    `json:"cert_sha256"`
	PairedAt   time.Time `json:"paired_at"`
}

// State is a Controller's place in the pair. The zero value - no file -
// is a Controller on its own: active, epoch 0, no peer.
type State struct {
	Role  string `json:"role"`
	Epoch uint64 `json:"epoch"`
	Peer  *Peer  `json:"peer,omitempty"`
}

// Store is one Controller's HA files.
type Store struct {
	dir string
	key Sealer

	mu     sync.Mutex
	state  State
	secret []byte
	cert   tls.Certificate
	certFP string
}

// Open reads dir (<data>/ha), making the HA certificate the first time.
func Open(dir string, key Sealer) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, key: key, state: State{Role: Active}}
	if err := readJSON(filepath.Join(dir, "state.json"), &s.state); err != nil {
		return nil, fmt.Errorf("ha: %w", err)
	}
	if s.state.Role == "" {
		s.state.Role = Active
	}
	if s.state.Role != Active && s.state.Role != Standby {
		return nil, fmt.Errorf("ha: state.json: role %q", s.state.Role)
	}
	if sealed, err := os.ReadFile(filepath.Join(dir, "secret.sealed")); err == nil {
		if s.secret, err = key.Open(sealed, purposeSecret); err != nil {
			return nil, fmt.Errorf("ha: the peers' secret isn't sealed with this master key - it changed since the pairing (a restore by an older dashboardd, a key replaced by hand): remove %s and pair the standby again: %w", dir, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := s.loadCert(); err != nil {
		if errors.Is(err, errSealedOther) {
			return nil, fmt.Errorf("ha: the HA certificate's key isn't sealed with this master key - it changed since it was made (a restore by an older dashboardd, a key replaced by hand): remove %s to make a new one; a standby must then be paired again", dir)
		}
		return nil, fmt.Errorf("ha: the HA certificate: %w", err)
	}
	return s, nil
}

func (s *Store) loadCert() error {
	certPath, keyPath := filepath.Join(s.dir, "ha.crt"), filepath.Join(s.dir, "ha.key.sealed")
	certPEM, err := os.ReadFile(certPath)
	if errors.Is(err, os.ErrNotExist) {
		return s.makeCert()
	}
	if err != nil {
		return err
	}
	sealed, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	keyPEM, err := s.key.Open(sealed, purposeKey)
	if err != nil {
		return fmt.Errorf("%w: %w", errSealedOther, err)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	s.cert, s.certFP = c, fingerprint(c.Certificate[0])
	return nil
}

func (s *Store) makeCert() error {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Janus Controller HA"},
		DNSNames:     []string{ServerName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	sealed, err := s.key.Seal(keyPEM, purposeKey)
	if err != nil {
		return err
	}
	if err := writeDurably(filepath.Join(s.dir, "ha.key.sealed"), sealed); err != nil {
		return err
	}
	if err := writeDurably(filepath.Join(s.dir, "ha.crt"), certPEM); err != nil {
		return err
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	s.cert, s.certFP = c, fingerprint(der)
	return nil
}

// errSealedOther: the HA key was sealed with another master key.
var errSealedOther = errors.New("sealed with another master key")

// Reseal seals what the store keeps sealed again with key: a promotion
// restores the active's master key over the standby's own.
func (s *Store) Reseal(key Sealer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keyPEM, err := pemKey(s.cert)
	if err != nil {
		return err
	}
	sealedKey, err := key.Seal(keyPEM, purposeKey)
	if err != nil {
		return err
	}
	if err := writeDurably(filepath.Join(s.dir, "ha.key.sealed"), sealedKey); err != nil {
		return err
	}
	if s.secret != nil {
		sealed, err := key.Seal(s.secret, purposeSecret)
		if err != nil {
			return err
		}
		if err := writeDurably(filepath.Join(s.dir, "secret.sealed"), sealed); err != nil {
			return err
		}
	}
	// The last copy is spent once restored: it goes rather than stay
	// sealed with a key the Controller no longer has.
	_ = os.Remove(filepath.Join(s.dir, "replica.sealed"))
	s.key = key
	return nil
}

func pemKey(c tls.Certificate) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// State is the Controller's place in the pair, now.
func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	if st.Peer != nil {
		p := *st.Peer
		st.Peer = &p
	}
	return st
}

// Certificate is the HA certificate: what the registration port serves
// for ServerName.
func (s *Store) Certificate() *tls.Certificate { return &s.cert }

// CertSHA256 is the HA certificate's SHA-256 (hex): what a peer pins.
func (s *Store) CertSHA256() string { return s.certFP }

// CertPEM is the HA certificate, PEM.
func (s *Store) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.cert.Certificate[0]})
}

// Authorized reports whether bearer is the peers' secret - false while
// unpaired.
func (s *Store) Authorized(bearer string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secret == nil || s.state.Peer == nil {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(bearer)
	return err == nil && subtle.ConstantTimeCompare(got, s.secret) == 1
}

// Bearer is the peers' secret as a request carries it.
func (s *Store) Bearer() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return base64.RawURLEncoding.EncodeToString(s.secret)
}

// SetPeer pairs the Controller with p, the secret they share.
func (s *Store) SetPeer(p Peer, secret []byte, role string, epoch uint64) error {
	sealed, err := s.key.Seal(secret, purposeSecret)
	if err != nil {
		return err
	}
	if err := writeDurably(filepath.Join(s.dir, "secret.sealed"), sealed); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := State{Role: role, Epoch: epoch, Peer: &p}
	if err := writeJSON(filepath.Join(s.dir, "state.json"), next); err != nil {
		return err
	}
	s.state, s.secret = next, append([]byte(nil), secret...)
	return nil
}

// Unpair forgets the peer; the Controller stays what it is.
func (s *Store) Unpair() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := State{Role: s.state.Role, Epoch: s.state.Epoch}
	if err := writeJSON(filepath.Join(s.dir, "state.json"), next); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.dir, "secret.sealed")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.state, s.secret = next, nil
	return nil
}

// SetRole makes the Controller active or standby at epoch, durably.
func (s *Store) SetRole(role string, epoch uint64) error {
	if role != Active && role != Standby {
		return fmt.Errorf("ha: role %q", role)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.Role == Active && role == Standby {
		// What an earlier time as a standby left - its pulls' notes; the
		// copy itself went with the promotion (Reseal) - is of an epoch
		// before this Controller was the active: not this standby's.
		for _, name := range []string{"replica.sealed", "replica.json"} {
			if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	next := s.state
	next.Role, next.Epoch = role, epoch
	if err := writeJSON(filepath.Join(s.dir, "state.json"), next); err != nil {
		return err
	}
	s.state = next
	return nil
}

// --- pairing ---

// tokenPrefix starts a pairing token: janus-standby_<the active's HA
// certificate SHA-256>_<secret>. The standby pins the active by the
// first; the second is good once, for TokenLifetime.
const tokenPrefix = "janus-standby_"

// TokenLifetime is how long a pairing token is good for.
const TokenLifetime = time.Hour

type pairing struct {
	SecretSHA256 string    `json:"secret_sha256"`
	Expires      time.Time `json:"expires"`
}

// NewPairing makes the active's pairing token - the one before it is
// no longer good.
func (s *Store) NewPairing(now time.Time) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	exp := now.Add(TokenLifetime)
	if err := writeJSON(filepath.Join(s.dir, "pairing.json"), pairing{SecretSHA256: hex.EncodeToString(sum[:]), Expires: exp}); err != nil {
		return "", time.Time{}, err
	}
	return tokenPrefix + s.certFP + "_" + secret, exp, nil
}

// ErrPairing is a pairing secret that isn't the token's, or a token
// used or expired.
var ErrPairing = errors.New("this pairing token isn't good: used, expired, or not this Controller's")

// UsePairing spends the pending pairing token whose secret is given.
func (s *Store) UsePairing(secret string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, "pairing.json")
	var p pairing
	if err := readJSON(path, &p); err != nil || p.SecretSHA256 == "" {
		return ErrPairing
	}
	sum := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(p.SecretSHA256)) != 1 || !now.Before(p.Expires) {
		return ErrPairing
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return nil
}

// ParseToken splits a pairing token: the active's HA certificate
// SHA-256, and the secret.
func ParseToken(token string) (certSHA256, secret string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(token), tokenPrefix)
	if !ok {
		return "", "", errors.New("not a standby pairing token (janus-standby_...)")
	}
	fp, secret, ok := strings.Cut(rest, "_")
	if !ok || len(fp) != 64 || secret == "" {
		return "", "", errors.New("a malformed standby pairing token")
	}
	if _, err := hex.DecodeString(fp); err != nil {
		return "", "", errors.New("a malformed standby pairing token")
	}
	return fp, secret, nil
}

// NewSecret is a secret two peers share.
func NewSecret() ([]byte, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return b, err
}

// --- decisions ---

// StepDown reports whether a Controller in own steps down, its peer
// answering role at epoch: the peer is active above its epoch - it was
// promoted while this one wasn't looking.
func StepDown(own State, peerRole string, peerEpoch uint64) bool {
	return own.Role == Active && peerRole == Active && peerEpoch > own.Epoch
}

// QuietFor is how long the active must have been silent before a standby
// promotes itself without -force.
const QuietFor = 2 * time.Minute

// MayPromote says whether a standby promotes without the active's
// handover: when the active hasn't answered for QuietFor, or forced.
func MayPromote(lastContact, now time.Time, force bool) error {
	if force || lastContact.IsZero() || now.Sub(lastContact) >= QuietFor {
		return nil
	}
	return fmt.Errorf("the active answered %s ago and didn't hand over: it may still run - stop it, wait %s, or force the promotion", now.Sub(lastContact).Round(time.Second), QuietFor)
}

// --- files ---

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Fingerprint is a certificate's SHA-256 (hex), from its DER.
func Fingerprint(der []byte) string { return fingerprint(der) }

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeDurably(path, append(data, '\n'))
}

// writeDurably writes path so that a power cut leaves the old file or the
// new one: tmp, fsync, rename, the directory's fsync.
func writeDurably(path string, data []byte) error {
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
