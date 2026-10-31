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
//
// The issuing CA lasts two years. Renewing it takes the kit again
// (Renew): a new issuing CA and a bundle listing it and the current one.
// The nodes take that bundle from the trust loop; until a node has it,
// the Controller reaches it with a certificate of the CA before
// (ClientCertificateFor); janusctl's certificates come from the new CA
// once every node has it (Switch). The CA before leaves the bundles at
// the next renewal - expired, it lets nobody in anyway.
package fleet

import (
	"bytes"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/secrets"
	"github.com/swenske/Janus/internal/fleetkit"
	"github.com/swenske/Janus/internal/pki"
)

// Durations: vars, the tests change them.
var (
	rootValidity    = 20 * 365 * 24 * time.Hour
	issuingValidity = 2 * 365 * 24 * time.Hour
	// switchBefore: after a renewal, janusctl's certificates come from
	// the new issuing CA once every node took the renewal's bundle - or
	// this long before the previous CA ends, whatever the nodes.
	switchBefore = 30 * 24 * time.Hour
	// rescueValidity is how long the root-signed certificate a renewal
	// or a rescue makes - the kit open - reaches the nodes that refuse
	// every other.
	rescueValidity = time.Hour
)

// RenewAhead is how long before the issuing CA ends the Controller asks
// for its renewal.
const RenewAhead = 180 * 24 * time.Hour

const (
	identityValidity = 24 * time.Hour
	// identityRenewal: the Controller's certificate is renewed once it
	// has less than this left.
	identityRenewal = 16 * time.Hour

	// IdentityName is the Controller's client certificate's common name,
	// as the nodes log it ("alice (os:admin) via janus-controller").
	IdentityName = "janus-controller"
)

// The files in the fleet directory. issuing.{crt,key.sealed} is always
// the issuing CA janusctl's certificates come from - what an older
// release reads, rolled back to. A renewal's CA waits in
// issuing-<ms>.* until the switch, the CA before it then stays in
// previous-<ms>.*; a key is sealed for its file's name.
const (
	stateFile    = "state.json"
	rootCertFile = "root.crt"
	rootKeyFile  = "root.key.sealed"  // pending only
	kitFile      = "recovery-kit.age" // pending only
	issuingStem  = "issuing"
	bundleFile   = "bundle.json"
)

func crtFile(stem string) string    { return stem + ".crt" }
func keyFile(stem string) string    { return stem + ".key.sealed" }
func keyPurpose(stem string) string { return "fleet/" + keyFile(stem) }

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

	// The last renewal (Renew). Newest: the file stem of its CA while it
	// isn't in issuing.* yet - until the switch; Previous: the stem of
	// the CA before it. RenewedVersion is the first bundle listing the
	// new CA - a node with it lets the new CA's certificates in -, kept
	// in BundleFile too (bundle.json is copied from it).
	Newest         string    `json:"newest,omitempty"`
	Previous       string    `json:"previous,omitempty"`
	RenewedVersion uint64    `json:"renewed_version,omitempty"`
	BundleFile     string    `json:"bundle_file,omitempty"`
	Renewed        time.Time `json:"renewed,omitzero"`
	Switched       time.Time `json:"switched,omitzero"`
}

// pending reports whether a renewal waits for its switch.
func (st state) pending() bool { return st.RenewedVersion != 0 && st.Switched.IsZero() }

func (st state) newestStem() string {
	if st.Newest != "" {
		return st.Newest
	}
	return issuingStem
}

// Store is the Controller's fleet.
type Store struct {
	dir string
	key *secrets.Key
	// ControllerID names the kit (the Controller it belongs to).
	controllerID string

	mu   sync.Mutex
	st   state
	root *x509.Certificate
	// issuing is the newest issuing CA; previous the one before it, after
	// a renewal (nil before any).
	issuing  *pki.CA
	previous *pki.CA
	bundle   []byte
	// identities are the Controller's certificates, by their CA's
	// fingerprint; server its registration certificate; rescue the
	// root-signed one a renewal or a rescue left, an hour.
	identities map[string]*tls.Certificate
	server     *tls.Certificate
	rescue     *tls.Certificate
}

// Open loads the fleet kept in dir - finishing a renewal's writes a
// stop cut short.
func Open(dir string, key *secrets.Key, controllerID string) (*Store, error) {
	s := &Store{dir: dir, key: key, controllerID: controllerID, st: state{State: StateNone}, identities: map[string]*tls.Certificate{}}
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
	if err := s.rollForward(); err != nil {
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
	if s.issuing, err = s.loadCA(s.st.newestStem()); err != nil {
		return err
	}
	if s.st.Previous != "" {
		if s.previous, err = s.loadCA(s.st.Previous); err != nil {
			return err
		}
	}
	name := bundleFile
	if s.st.BundleFile != "" {
		name = s.st.BundleFile
	}
	if s.bundle, err = os.ReadFile(filepath.Join(s.dir, name)); err != nil {
		return err
	}
	return nil
}

func (s *Store) loadCA(stem string) (*pki.CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(s.dir, crtFile(stem)))
	if err != nil {
		return nil, err
	}
	sealed, err := os.ReadFile(filepath.Join(s.dir, keyFile(stem)))
	if err != nil {
		return nil, err
	}
	keyPEM, err := s.key.Open(sealed, keyPurpose(stem))
	if err != nil {
		return nil, fmt.Errorf("the issuing CA's key (%s): %w - is this the master key it was sealed with?", keyFile(stem), err)
	}
	ca, err := pki.LoadCA(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("the issuing CA (%s): %w", crtFile(stem), err)
	}
	return ca, nil
}

// saveCA writes ca under stem - its key sealed for that name -, the key
// first: the certificate there means the CA is whole.
func (s *Store) saveCA(stem string, ca *pki.CA) error {
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		return err
	}
	sealed, err := s.key.Seal(keyPEM, keyPurpose(stem))
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(s.dir, keyFile(stem)), sealed); err != nil {
		return err
	}
	return writeFile(filepath.Join(s.dir, crtFile(stem)), ca.CertPEM)
}

func (s *Store) removeCA(stem string) {
	for _, name := range []string{crtFile(stem), keyFile(stem)} {
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("fleet: remove %s: %v", name, err)
		}
	}
}

// rollForward finishes what state.json says was done: bundle.json is
// the state's bundle, and after the switch issuing.* the newest CA.
func (s *Store) rollForward() error {
	if s.st.BundleFile != "" {
		cur, err := os.ReadFile(filepath.Join(s.dir, bundleFile))
		if err != nil || !bytes.Equal(cur, s.bundle) {
			if err := writeFile(filepath.Join(s.dir, bundleFile), s.bundle); err != nil {
				return err
			}
		}
	}
	if !s.st.Switched.IsZero() && s.st.Newest != "" {
		return s.finishSwitch()
	}
	return nil
}

// Status is what the Controller shows of its fleet.
type Status struct {
	State           string    `json:"state"`
	RootFingerprint string    `json:"root_fingerprint,omitempty"`
	RootNotAfter    time.Time `json:"root_not_after,omitzero"`
	// IssuingNotAfter is when the newest issuing CA ends - RenewFrom
	// when the Controller starts asking for its renewal.
	IssuingName     string    `json:"issuing_name,omitempty"`
	IssuingNotAfter time.Time `json:"issuing_not_after,omitzero"`
	RenewFrom       time.Time `json:"renew_from,omitzero"`
	BundleVersion   uint64    `json:"bundle_version,omitempty"`
	Created         time.Time `json:"created,omitzero"`
	Confirmed       time.Time `json:"confirmed,omitzero"`
	MasterKeyBeside bool      `json:"master_key_beside_data"`
	// Renewal is the last renewal, if any.
	Renewal *Renewal `json:"renewal,omitempty"`
}

// Renewal is a renewal of the issuing CA: its bundle's version, when it
// was made, when janusctl's certificates switched to the new CA (zero:
// once every node has the bundle, or SwitchBy at the latest), the end of
// the CA before it, and until when its root-signed certificate reaches
// the nodes that refuse every other.
type Renewal struct {
	Version          uint64    `json:"version"`
	Renewed          time.Time `json:"renewed"`
	Switched         time.Time `json:"switched,omitzero"`
	SwitchBy         time.Time `json:"switch_by,omitzero"`
	PreviousName     string    `json:"previous_name,omitempty"`
	PreviousNotAfter time.Time `json:"previous_not_after,omitzero"`
	RescueUntil      time.Time `json:"rescue_until,omitzero"`
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
		st.IssuingName = s.issuing.Cert.Subject.CommonName
		st.IssuingNotAfter = s.issuing.Cert.NotAfter
		st.RenewFrom = s.issuing.Cert.NotAfter.Add(-RenewAhead)
	}
	if s.st.RenewedVersion != 0 {
		r := &Renewal{Version: s.st.RenewedVersion, Renewed: s.st.Renewed, Switched: s.st.Switched}
		if s.previous != nil {
			r.PreviousName, r.PreviousNotAfter = s.previous.Cert.Subject.CommonName, s.previous.Cert.NotAfter
			if s.st.pending() {
				r.SwitchBy = s.previous.Cert.NotAfter.Add(-switchBefore)
			}
		}
		st.Renewal = r
	}
	if s.rescue != nil && time.Now().Before(s.rescue.Leaf.NotAfter) {
		if st.Renewal == nil {
			st.Renewal = &Renewal{}
		}
		st.Renewal.RescueUntil = s.rescue.Leaf.NotAfter
	}
	return st
}

// RenewedVersion is the first bundle version listing the newest issuing
// CA - 0 before any renewal: a node with it, or a newer one, lets that
// CA's certificates in.
func (s *Store) RenewedVersion() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.RenewedVersion
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
	sealedIssuing, err := s.key.Seal(issuingKeyPEM, keyPurpose(issuingStem))
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
		{crtFile(issuingStem), issuing.CertPEM},
		{keyFile(issuingStem), sealedIssuing},
		{bundleFile, bundle},
	} {
		if err := writeFile(filepath.Join(s.dir, f.name), f.data); err != nil {
			return "", err
		}
	}
	if err := s.writeState(st); err != nil {
		return "", err
	}
	s.root, s.issuing, s.previous, s.bundle = root.Cert, issuing, nil, bundle
	s.identities, s.server, s.rescue = map[string]*tls.Certificate{}, nil, nil
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

// ErrExpired is an issuing CA past its end: the fleet needs renewing.
var ErrExpired = errors.New("the fleet's issuing CA has expired: renew it with the recovery kit (the Fleet card on the Controller's main page)")

func validNow(c *x509.Certificate) bool {
	now := time.Now()
	return !now.Before(c.NotBefore) && now.Before(c.NotAfter)
}

// ClientCertificate is the Controller's own client certificate for a
// node with the newest bundle - ClientCertificateFor.
func (s *Store) ClientCertificate() (*tls.Certificate, error) {
	return s.ClientCertificateFor(^uint64(0))
}

// ClientCertificateFor is the Controller's own client certificate for a
// node whose bundle is version nodeBundle (0: not known) - role
// janus:controller, followed by its issuing CA, valid a day and renewed
// when it has less than 16 hours left. For a node whose bundle lists the
// newest issuing CA (the renewal's version or newer), that CA's. For one
// on an older bundle, the CA before while it lasts, else the root-signed
// certificate a renewal or a rescue left while it lasts, else the
// newest's (a node whose version wasn't known may well have it). Only
// once the fleet is ready.
func (s *Store) ClientCertificateFor(nodeBundle uint64) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != StateReady {
		return nil, fmt.Errorf("%w: the fleet isn't set up", ErrState)
	}
	rescue := s.rescue
	if rescue != nil && time.Until(rescue.Leaf.NotAfter) <= 0 {
		rescue = nil
	}
	behind := s.st.RenewedVersion != 0 && nodeBundle < s.st.RenewedVersion
	switch {
	case behind && s.previous != nil && validNow(s.previous.Cert):
		return s.identityLocked(s.previous)
	case behind && rescue != nil:
		return rescue, nil
	case validNow(s.issuing.Cert):
		return s.identityLocked(s.issuing)
	case rescue != nil:
		return rescue, nil
	}
	return nil, ErrExpired
}

// RescueCertificate is the root-signed certificate a renewal or a
// rescue left, while it lasts - nil otherwise.
func (s *Store) RescueCertificate() *tls.Certificate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rescue == nil || time.Until(s.rescue.Leaf.NotAfter) <= 0 {
		return nil
	}
	return s.rescue
}

// identityLocked is the Controller's certificate from ca, renewed when
// it has less than 16 hours left - never past ca's end.
func (s *Store) identityLocked(ca *pki.CA) (*tls.Certificate, error) {
	fp := pki.Fingerprint(ca.Cert.Raw)
	if c := s.identities[fp]; c != nil && time.Until(c.Leaf.NotAfter) > min(identityRenewal, time.Until(ca.Cert.NotAfter)/2) {
		return c, nil
	}
	certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{
		CommonName:  IdentityName,
		Roles:       []string{pki.RoleController},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		Validity:    min(identityValidity, time.Until(ca.Cert.NotAfter)),
	})
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(append(certPEM, ca.CertPEM...), keyPEM)
	if err != nil {
		return nil, err
	}
	s.identities[fp] = &cert
	return &cert, nil
}

// ServerCertificate is the Controller's registration endpoint's
// certificate for nodes provisioned with the fleet's root: named
// pki.FleetControllerName, issued by the newest issuing CA (followed by
// it) - nodes check it against the root alone -, valid 30 days and
// renewed with less than 10 left, never past the CA's end. Only once the
// fleet is ready.
func (s *Store) ServerCertificate() (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != StateReady {
		return nil, fmt.Errorf("%w: the fleet isn't set up", ErrState)
	}
	if s.server != nil && time.Until(s.server.Leaf.NotAfter) > min(10*24*time.Hour, time.Until(s.issuing.Cert.NotAfter)/2) {
		return s.server, nil
	}
	if !validNow(s.issuing.Cert) {
		return nil, ErrExpired
	}
	certPEM, keyPEM, err := s.issuing.Issue(pki.IssueOptions{
		CommonName:  pki.FleetControllerName,
		DNSNames:    []string{pki.FleetControllerName},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		Validity:    min(30*24*time.Hour, time.Until(s.issuing.Cert.NotAfter)),
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

// usersCA is the issuing CA janusctl's certificates come from: until a
// renewal's switch, the one before it.
func (s *Store) usersCA() *pki.CA {
	if s.st.pending() && s.previous != nil {
		return s.previous
	}
	return s.issuing
}

// IssueUser signs an account's client certificate for pub - janusctl's,
// for its own key: name as the common name, role (os:admin, os:operator,
// os:reader) for the nodes - or, with scope, what it may do on each
// (pki.Scope) -, valid ttl at most (never past the issuing CA), followed
// by the issuing CA. Only once the fleet is ready.
func (s *Store) IssueUser(pub crypto.PublicKey, name, role string, scope *pki.Scope, ttl time.Duration) (chainPEM []byte, notAfter time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != StateReady {
		return nil, time.Time{}, fmt.Errorf("%w: the fleet isn't set up", ErrState)
	}
	ca := s.usersCA()
	left := time.Until(ca.Cert.NotAfter)
	if !validNow(ca.Cert) || left < time.Minute {
		return nil, time.Time{}, ErrExpired
	}
	ttl = min(ttl, left)
	certPEM, err := ca.IssueFor(pub, pki.IssueOptions{
		CommonName:  name,
		Roles:       []string{role},
		Scope:       scope,
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
	return append(certPEM, ca.CertPEM...), leaf.NotAfter, nil
}

// ErrRenewing refuses a renewal while the last one waits for its
// switch.
var ErrRenewing = errors.New("the last renewal of the issuing CA isn't over: every node takes its bundle first (or 30 days before the previous CA ends)")

// Renew makes a new issuing CA with the kit's root - kit must open with
// passphrase and hold this fleet's root - and a bundle listing it and
// the current one (while it lasts), newer than the Controller's and
// above (the highest version a node holds). The root's key is never
// written: what's left of it is a certificate it signs for the
// Controller, valid an hour, in memory, for the nodes that refuse every
// other (Rescue). Nodes get the bundle from the trust loop; janusctl's
// certificates switch to the new CA with Switch.
func (s *Store) Renew(kit []byte, passphrase string, above uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != StateReady {
		return fmt.Errorf("%w: the fleet isn't set up", ErrState)
	}
	if s.st.pending() {
		return ErrRenewing
	}
	root, err := s.kitRoot(kit, passphrase)
	if err != nil {
		return err
	}
	now := time.Now()
	stamp := now.UnixMilli()
	next, err := root.IssueCA("Janus fleet issuing CA "+now.UTC().Format("2006-01"), issuingValidity)
	if err != nil {
		return err
	}
	version := max(s.st.BundleVersion+1, uint64(stamp), above+1)
	b := pki.Bundle{Version: version, Issued: now.UTC()}
	if validNow(s.issuing.Cert) {
		b.IssuingCAs = append(b.IssuingCAs, string(s.issuing.CertPEM))
	}
	b.IssuingCAs = append(b.IssuingCAs, string(next.CertPEM))
	bundle, err := pki.SignBundle(root, b)
	if err != nil {
		return err
	}
	rescue, err := rescueCertificate(root)
	if err != nil {
		return err
	}

	nextStem := fmt.Sprintf("%s-%d", issuingStem, stamp)
	if err := s.saveCA(nextStem, next); err != nil {
		return err
	}
	name := fmt.Sprintf("bundle-%d.json", version)
	if err := writeFile(filepath.Join(s.dir, name), bundle); err != nil {
		return err
	}
	oldPrevious, oldBundle := s.st.Previous, s.st.BundleFile
	st := s.st
	st.Newest, st.Previous = nextStem, s.st.newestStem()
	st.RenewedVersion, st.BundleVersion, st.BundleFile = version, version, name
	st.Renewed, st.Switched = now.UTC(), time.Time{}
	if err := s.writeState(st); err != nil {
		return err
	}
	s.previous, s.issuing, s.bundle = s.issuing, next, bundle
	s.server, s.rescue = nil, rescue
	if err := writeFile(filepath.Join(s.dir, bundleFile), bundle); err != nil {
		return err
	}
	if oldPrevious != "" && oldPrevious != st.Previous {
		s.removeCA(oldPrevious)
	}
	if oldBundle != "" && oldBundle != name {
		_ = os.Remove(filepath.Join(s.dir, oldBundle))
	}
	return nil
}

// Rescue opens the kit - kit must open with passphrase and hold this
// fleet's root - for a certificate the root signs for the Controller,
// valid an hour, in memory: it reaches the nodes whose bundle lists no
// issuing CA the Controller can still sign with (ClientCertificateFor).
func (s *Store) Rescue(kit []byte, passphrase string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.State != StateReady {
		return fmt.Errorf("%w: the fleet isn't set up", ErrState)
	}
	root, err := s.kitRoot(kit, passphrase)
	if err != nil {
		return err
	}
	rescue, err := rescueCertificate(root)
	if err != nil {
		return err
	}
	s.rescue = rescue
	return nil
}

// kitRoot is the root kit holds, opened with passphrase - this fleet's.
func (s *Store) kitRoot(kit []byte, passphrase string) (*pki.CA, error) {
	content, err := fleetkit.Open(kit, passphrase)
	if err != nil {
		return nil, ErrKit
	}
	root, err := content.Root()
	if err != nil || !root.Cert.Equal(s.root) {
		return nil, ErrKit
	}
	return root, nil
}

func rescueCertificate(root *pki.CA) (*tls.Certificate, error) {
	certPEM, keyPEM, err := root.Issue(pki.IssueOptions{
		CommonName:  IdentityName,
		Roles:       []string{pki.RoleController},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		Validity:    rescueValidity,
	})
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

// Switch has janusctl's certificates come from the newest issuing CA -
// once every node has a renewal's bundle (allNodes), or 30 days before
// the CA before it ends whatever the nodes. true when it did. The newest
// CA goes into issuing.*, the one before into previous-<ms>.*.
func (s *Store) Switch(allNodes bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.st.pending() {
		return false, nil
	}
	if !allNodes && s.previous != nil && time.Until(s.previous.Cert.NotAfter) > switchBefore {
		return false, nil
	}
	prevStem := fmt.Sprintf("previous-%d", time.Now().UnixMilli())
	if s.previous != nil {
		if err := s.saveCA(prevStem, s.previous); err != nil {
			return false, err
		}
	}
	st := s.st
	st.Switched = time.Now().UTC()
	if s.previous != nil {
		st.Previous = prevStem
	} else {
		st.Previous = ""
	}
	if err := s.writeState(st); err != nil {
		return false, err
	}
	return true, s.finishSwitch()
}

// finishSwitch copies the newest CA into issuing.* - what an older
// release signs with - and forgets its other name. Again at Open when a
// stop cut it short.
func (s *Store) finishSwitch() error {
	newest := s.st.Newest
	if err := s.saveCA(issuingStem, s.issuing); err != nil {
		return err
	}
	st := s.st
	st.Newest = ""
	if err := s.writeState(st); err != nil {
		return err
	}
	s.removeCA(newest)
	return nil
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
