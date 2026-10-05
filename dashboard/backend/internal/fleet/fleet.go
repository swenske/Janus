// Package fleet is the Controller's half of its nodes' fleet trust
// (internal/pki/fleet.go): the fleet's root certificate, the issuing CA
// the Controller signs its own client certificates with, and the bundle
// the root signed that nodes apply.
//
// Setting it up is three steps, and only the last one changes anything
// for the nodes:
//
//  1. Setup makes the root, the issuing CA and the bundle, and a
//     recovery kit - the root's key, encrypted (age, scrypt) with a
//     passphrase made with it and shown once;
//  2. the operator downloads the kit and stores it with its passphrase
//     (a password manager);
//  3. Confirm takes the kit back with its passphrase - proof both were
//     kept - and deletes the root's key from the Controller for good.
//
// Until then the root's key waits, sealed by the master key, so the kit
// can be downloaded again. Afterwards the Controller only holds the
// issuing CA's key (sealed too): it signs its own client certificates,
// never a bundle - a new bundle needs the kit.
package fleet

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/internal/fleetkit"
	"github.com/swenske/Janus/internal/pki"
)

const (
	rootValidity     = 20 * 365 * 24 * time.Hour
	issuingValidity  = 2 * 365 * 24 * time.Hour
	identityValidity = 24 * time.Hour
	// identityRenewal: the Controller's certificate is renewed once it
	// has less than this left.
	identityRenewal = 16 * time.Hour

	// IdentityName is the Controller's client certificate's common name,
	// as the nodes log it ("alice (os:admin) via janus-controller").
	IdentityName = "janus-controller"
)

// The files in the fleet directory.
const (
	stateFile      = "state.json"
	rootCertFile   = "root.crt"
	rootKeyFile    = "root.key.sealed"  // pending only
	kitFile        = "recovery-kit.age" // pending only
	issuingCrtFile = "issuing.crt"
	issuingKeyFile = "issuing.key.sealed"
	bundleFile     = "bundle.json"
)

// States.
const (
	StateNone    = "none"    // not set up
	StatePending = "pending" // the kit waits to be confirmed
	StateReady   = "ready"
)

type state struct {
	State         string    `json:"state"`
	BundleVersion uint64    `json:"bundle_version"`
	Created       time.Time `json:"created"`
	Confirmed     time.Time `json:"confirmed,omitzero"`
}

// Store is the Controller's fleet.
type Store struct {
	dir string
	key *secrets.Key
	// ControllerID names the kit (the Controller it belongs to).
	controllerID string

	mu       sync.Mutex
	st       state
	root     *x509.Certificate
	issuing  *pki.CA
	bundle   []byte
	identity *tls.Certificate
	server   *tls.Certificate
}

// Open loads the fleet kept in dir.
func Open(dir string, key *secrets.Key, controllerID string) (*Store, error) {
	s := &Store{dir: dir, key: key, controllerID: controllerID, st: state{State: StateNone}}
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.st); err != nil {
		return nil, fmt.Errorf("%s: %w", stateFile, err)
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	rootPEM, err := os.ReadFile(filepath.Join(s.dir, rootCertFile))
	if err != nil {
		return err
	}
	if s.root, err = parseCert(rootPEM); err != nil {
		return fmt.Errorf("the fleet's root: %w", err)
	}
	certPEM, err := os.ReadFile(filepath.Join(s.dir, issuingCrtFile))
	if err != nil {
		return err
	}
	sealed, err := os.ReadFile(filepath.Join(s.dir, issuingKeyFile))
	if err != nil {
		return err
	}
	keyPEM, err := s.key.Open(sealed, "fleet/"+issuingKeyFile)
	if err != nil {
		return fmt.Errorf("the issuing CA's key: %w - is this the master key it was sealed with?", err)
	}
	if s.issuing, err = pki.LoadCA(certPEM, keyPEM); err != nil {
		return fmt.Errorf("the issuing CA: %w", err)
	}
	if s.bundle, err = os.ReadFile(filepath.Join(s.dir, bundleFile)); err != nil {
		return err
	}
	return nil
}

// Status is what the Controller shows of its fleet.
type Status struct {
	State           string    `json:"state"`
	RootFingerprint string    `json:"root_fingerprint,omitempty"`
	RootNotAfter    time.Time `json:"root_not_after,omitzero"`
	IssuingNotAfter time.Time `json:"issuing_not_after,omitzero"`
	BundleVersion   uint64    `json:"bundle_version,omitempty"`
	Created         time.Time `json:"created,omitzero"`
	Confirmed       time.Time `json:"confirmed,omitzero"`
	MasterKeyBeside bool      `json:"master_key_beside_data"`
}

func (s *Store) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{State: s.st.State, BundleVersion: s.st.BundleVersion, Created: s.st.Created, Confirmed: s.st.Confirmed, MasterKeyBeside: s.key.BesideData}
	if s.root != nil {
		st.RootFingerprint = pki.Fingerprint(s.root.Raw)
		st.RootNotAfter = s.root.NotAfter
	}
	if s.issuing != nil {
		st.IssuingNotAfter = s.issuing.Cert.NotAfter
	}
	return st
}

// ErrState is a step taken out of order.
var ErrState = errors.New("not at this step of the fleet's setup")

// Setup makes the fleet - root, issuing CA, bundle - and its recovery
// kit, and returns the kit's passphrase, shown once. Only before the
// fleet is ready; again while the kit waits, it starts over (a kit lost
// before it was confirmed).
func (s *Store) Setup() (passphrase string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State == StateReady {
		return "", fmt.Errorf("%w: the fleet is already set up", ErrState)
	}
	root, err := pki.NewCAFor("Janus fleet root", rootValidity)
	if err != nil {
		return "", err
	}
	issuing, err := root.IssueCA("Janus fleet issuing CA", issuingValidity)
	if err != nil {
		return "", err
	}
	bundle, err := pki.SignBundle(root, pki.Bundle{Version: 1, Issued: time.Now().UTC(), IssuingCAs: []string{string(issuing.CertPEM)}})
	if err != nil {
		return "", err
	}
	rootKeyPEM, err := root.KeyPEM()
	if err != nil {
		return "", err
	}
	issuingKeyPEM, err := issuing.KeyPEM()
	if err != nil {
		return "", err
	}
	passphrase, err = fleetkit.NewPassphrase()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	kit, err := fleetkit.Make(fleetkit.Kit{Controller: s.controllerID, Created: now, RootCert: string(root.CertPEM), RootKey: string(rootKeyPEM)}, passphrase)
	if err != nil {
		return "", err
	}
	sealedRoot, err := s.key.Seal(rootKeyPEM, "fleet/"+rootKeyFile)
	if err != nil {
		return "", err
	}
	sealedKit, err := s.key.Seal(kit, "fleet/"+kitFile)
	if err != nil {
		return "", err
	}
	sealedIssuing, err := s.key.Seal(issuingKeyPEM, "fleet/"+issuingKeyFile)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", err
	}
	st := state{State: StatePending, BundleVersion: 1, Created: now}
	for _, f := range []struct {
		name string
		data []byte
	}{
		{rootCertFile, root.CertPEM},
		{rootKeyFile, sealedRoot},
		{kitFile, sealedKit},
		{issuingCrtFile, issuing.CertPEM},
		{issuingKeyFile, sealedIssuing},
		{bundleFile, bundle},
	} {
		if err := writeFile(filepath.Join(s.dir, f.name), f.data); err != nil {
			return "", err
		}
	}
	if err := s.writeState(st); err != nil {
		return "", err
	}
	s.root, s.issuing, s.bundle, s.identity, s.server = root.Cert, issuing, bundle, nil, nil
	return passphrase, nil
}

// RecoveryKit is the kit to download, while it waits to be confirmed.
func (s *Store) RecoveryKit() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != StatePending {
		return nil, fmt.Errorf("%w: the kit can only be downloaded before it's confirmed", ErrState)
	}
	sealed, err := os.ReadFile(filepath.Join(s.dir, kitFile))
	if err != nil {
		return nil, err
	}
	return s.key.Open(sealed, "fleet/"+kitFile)
}

// KitFileName is the name the kit is downloaded under.
func (s *Store) KitFileName() string {
	id := s.controllerID
	if len(id) > 8 {
		id = id[:8]
	}
	return "janus-recovery-kit-" + id + ".age"
}

// ErrKit is a kit or passphrase that doesn't open, or a kit of another
// fleet.
var ErrKit = errors.New("this kit doesn't open with this passphrase, or isn't this fleet's")

// Confirm checks the operator kept the kit and its passphrase - kit must
// open with passphrase and hold this fleet's root - and deletes the
// root's key from the Controller.
func (s *Store) Confirm(kit []byte, passphrase string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != StatePending {
		return fmt.Errorf("%w: nothing waits to be confirmed", ErrState)
	}
	content, err := fleetkit.Open(kit, passphrase)
	if err != nil {
		return ErrKit
	}
	root, err := content.Root()
	if err != nil || !root.Cert.Equal(s.root) {
		return ErrKit
	}
	for _, name := range []string{rootKeyFile, kitFile} {
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	st := s.st
	st.State, st.Confirmed = StateReady, time.Now().UTC()
	return s.writeState(st)
}

// Trust is what a node applies: the root (PEM), the signed bundle and
// its version - once the fleet is ready.
func (s *Store) Trust() (rootPEM, bundle []byte, version uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != StateReady {
		return nil, nil, 0, false
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.root.Raw}), s.bundle, s.st.BundleVersion, true
}

// ClientCertificate is the Controller's own client certificate - role
// janus:controller, followed by the issuing CA, valid a day and renewed
// when it has less than 16 hours left. Only once the fleet is ready.
func (s *Store) ClientCertificate() (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != StateReady {
		return nil, fmt.Errorf("%w: the fleet isn't set up", ErrState)
	}
	if s.identity != nil && time.Until(s.identity.Leaf.NotAfter) > identityRenewal {
		return s.identity, nil
	}
	certPEM, keyPEM, err := s.issuing.Issue(pki.IssueOptions{
		CommonName:  IdentityName,
		Roles:       []string{pki.RoleController},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		Validity:    identityValidity,
	})
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(append(certPEM, s.issuing.CertPEM...), keyPEM)
	if err != nil {
		return nil, err
	}
	s.identity = &cert
	return s.identity, nil
}

// ServerCertificate is the Controller's registration endpoint's
// certificate for nodes provisioned with the fleet's root: named
// pki.FleetControllerName, issued by the issuing CA (followed by it),
// valid 30 days and renewed with less than 10 left. Only once the fleet
// is ready.
func (s *Store) ServerCertificate() (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != StateReady {
		return nil, fmt.Errorf("%w: the fleet isn't set up", ErrState)
	}
	if s.server != nil && time.Until(s.server.Leaf.NotAfter) > 10*24*time.Hour {
		return s.server, nil
	}
	certPEM, keyPEM, err := s.issuing.Issue(pki.IssueOptions{
		CommonName:  pki.FleetControllerName,
		DNSNames:    []string{pki.FleetControllerName},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		Validity:    30 * 24 * time.Hour,
	})
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(append(certPEM, s.issuing.CertPEM...), keyPEM)
	if err != nil {
		return nil, err
	}
	s.server = &cert
	return s.server, nil
}

// IssueUser signs an account's client certificate for pub - janusctl's,
// for its own key: name as the common name, role (os:admin, os:operator,
// os:reader) for the nodes, valid ttl at most (never past the issuing
// CA), followed by the issuing CA. Only once the fleet is ready.
func (s *Store) IssueUser(pub crypto.PublicKey, name, role string, ttl time.Duration) (chainPEM []byte, notAfter time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != StateReady {
		return nil, time.Time{}, fmt.Errorf("%w: the fleet isn't set up", ErrState)
	}
	if left := time.Until(s.issuing.Cert.NotAfter); left < ttl {
		ttl = left
	}
	certPEM, err := s.issuing.IssueFor(pub, pki.IssueOptions{
		CommonName:  name,
		Roles:       []string{role},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		Validity:    ttl,
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	leaf, err := parseCert(certPEM)
	if err != nil {
		return nil, time.Time{}, err
	}
	return append(certPEM, s.issuing.CertPEM...), leaf.NotAfter, nil
}

// RootPEM is the fleet's root certificate (PEM), once the fleet is ready
// - what a node is provisioned with.
func (s *Store) RootPEM() ([]byte, bool) {
	rootPEM, _, _, ok := s.Trust()
	return rootPEM, ok
}

func (s *Store) writeState(st state) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(s.dir, stateFile), data); err != nil {
		return err
	}
	s.st = st
	return nil
}

// writeFile replaces path durably: temporary file, fsync, rename.
func writeFile(path string, data []byte) error {
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

func parseCert(p []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(p)
	if block == nil {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
