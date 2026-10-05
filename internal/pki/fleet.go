package pki

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// A node trusts its own CA - its first-boot admin certificate, the
// break-glass way in - and, once it joined one, its fleet: the client
// certificates the fleet's issuing CAs sign (the Controller's, its
// users'), with the roles they carry.
//
// The fleet's root CA signs nothing a client presents day to day: it
// signs the issuing CAs and the bundle that lists the ones a node
// accepts, and its key stays offline (the Controller's recovery kit). A
// node pins the root once and only takes a bundle the root signed, newer
// than its own - an issuing CA leaves the fleet with the next bundle,
// without anyone touching the nodes. A certificate the root signs
// directly is accepted too: the recovery kit's way in, should every
// issuing CA be lost.

// RoleOperator runs what's already set up - HAProxy's configuration and
// runtime state, services, reboots - but doesn't change how the node is
// set up (network, firewall, upgrades, credentials). RoleController is
// the Controller's: no right of its own, it acts for the user it names
// (internal/api/authz.go).
const (
	RoleOperator   = "os:operator"
	RoleController = "janus:controller"
)

// FleetControllerName is the name a node asks the Controller's
// registration endpoint for (TLS SNI) to get its fleet certificate - and
// checks it against, chained to the fleet's root it was provisioned
// with.
const FleetControllerName = "controller.fleet.janus"

// The call metadata through which a RoleController certificate says
// whom it acts for: a user's name, and that user's roles, comma-separated.
const (
	AsUserKey  = "janus-as-user"
	AsRolesKey = "janus-as-roles"
)

// A node keeps its fleet under its PKI directory - FleetDir/
// FleetRootFile and FleetDir/FleetBundleFile -, where Fleet.Set writes
// it and where provisioning (Install, janusctl image seed-fleet, NoCloud)
// puts it on STATE for the first boot.
const (
	FleetDir        = "fleet"
	FleetRootFile   = "root.crt"
	FleetBundleFile = "bundle.json"
)

// Bundle is what the fleet's root says a node accepts.
type Bundle struct {
	// Version only ever grows: a node refuses one older than its own.
	Version uint64    `json:"version"`
	Issued  time.Time `json:"issued"`
	// IssuingCAs are PEM certificates, each signed by the root.
	IssuingCAs []string `json:"issuing_cas"`
}

// SignedBundle is a Bundle as the root signed it: the exact bytes, and
// their ECDSA (SHA-256, ASN.1) signature.
type SignedBundle struct {
	Payload   []byte `json:"payload"`
	Signature []byte `json:"signature"`
}

// SignBundle signs b with the root's key.
func SignBundle(root *CA, b Bundle) ([]byte, error) {
	payload, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	sig, err := root.Key.Sign(rand.Reader, sum[:], crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("sign the bundle: %w", err)
	}
	return json.Marshal(SignedBundle{Payload: payload, Signature: sig})
}

// CanonicalBundle is signed as SignBundle writes it - what a node keeps
// and compares: the same bundle, indented or not (a NoCloud user-data),
// is the same bundle.
func CanonicalBundle(signed []byte) ([]byte, error) {
	var sb SignedBundle
	if err := json.Unmarshal(signed, &sb); err != nil {
		return nil, fmt.Errorf("not a signed bundle: %w", err)
	}
	return json.Marshal(sb)
}

// VerifyBundle checks signed against root: its signature, and that each
// issuing CA it lists is a CA the root signed, still valid. It returns
// the bundle and its issuing CAs.
func VerifyBundle(root *x509.Certificate, signed []byte) (*Bundle, []*x509.Certificate, error) {
	var sb SignedBundle
	if err := json.Unmarshal(signed, &sb); err != nil {
		return nil, nil, fmt.Errorf("not a signed bundle: %w", err)
	}
	if err := root.CheckSignature(x509.ECDSAWithSHA256, sb.Payload, sb.Signature); err != nil {
		return nil, nil, fmt.Errorf("the bundle isn't signed by the fleet's root: %w", err)
	}
	var b Bundle
	if err := json.Unmarshal(sb.Payload, &b); err != nil {
		return nil, nil, fmt.Errorf("the bundle doesn't parse: %w", err)
	}
	if b.Version == 0 {
		return nil, nil, errors.New("the bundle has no version")
	}
	var cas []*x509.Certificate
	for i, p := range b.IssuingCAs {
		block, _ := pem.Decode([]byte(p))
		if block == nil {
			return nil, nil, fmt.Errorf("issuing CA %d: no PEM certificate", i)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("issuing CA %d: %w", i, err)
		}
		if !c.IsCA || c.CheckSignatureFrom(root) != nil {
			return nil, nil, fmt.Errorf("issuing CA %d (%s) isn't a CA the fleet's root signed", i, c.Subject.CommonName)
		}
		cas = append(cas, c)
	}
	return &b, cas, nil
}

// CheckFleet checks a fleet to provision a node with: rootPEM a
// self-signed CA, signed a bundle it signed.
func CheckFleet(rootPEM, signed []byte) (*Bundle, error) {
	root, err := parseFleetRoot(rootPEM)
	if err != nil {
		return nil, err
	}
	b, _, err := VerifyBundle(root, signed)
	return b, err
}

// ProvisionFleet writes a fleet under pkiDir for janusd to trust when it
// starts - rootfs/init's NoCloud seeding, before janusd runs. Checked
// first; refused when pkiDir already has one. The root is written last:
// its presence is what "provisioned" means, so a power cut in between
// leaves nothing half done.
func ProvisionFleet(pkiDir string, rootPEM, signed []byte) error {
	if _, err := CheckFleet(rootPEM, signed); err != nil {
		return err
	}
	signed, err := CanonicalBundle(signed)
	if err != nil {
		return err
	}
	dir := filepath.Join(pkiDir, FleetDir)
	if _, err := os.Stat(filepath.Join(dir, FleetRootFile)); err == nil {
		return errors.New("a fleet is already there")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeDurably(filepath.Join(dir, FleetBundleFile), signed, 0o644); err != nil {
		return err
	}
	return writeDurably(filepath.Join(dir, FleetRootFile), rootPEM, 0o644)
}

// Fleet is what a node trusts of its fleet, kept on STATE (dir/fleet).
type Fleet struct {
	dir string

	mu      sync.RWMutex
	root    *x509.Certificate
	bundle  *Bundle
	signed  []byte
	issuers []*x509.Certificate
}

// ErrFleetRootMismatch is a root other than the one pinned.
var ErrFleetRootMismatch = errors.New("the node trusts another fleet root: reset its fleet trust first, with a certificate of the node's own CA")

// OpenFleet loads the fleet trust kept under pkiDir. What can't be read
// back is reported, and the node trusts no fleet: its own CA still lets
// it in.
func OpenFleet(pkiDir string) (*Fleet, error) {
	f := &Fleet{dir: filepath.Join(pkiDir, FleetDir)}
	rootPEM, err := os.ReadFile(filepath.Join(f.dir, FleetRootFile))
	if os.IsNotExist(err) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	root, err := parseFleetRoot(rootPEM)
	if err != nil {
		return f, err
	}
	signed, err := os.ReadFile(filepath.Join(f.dir, FleetBundleFile))
	if os.IsNotExist(err) {
		f.root = root
		return f, nil
	}
	if err != nil {
		return f, err
	}
	b, issuers, err := VerifyBundle(root, signed)
	if err != nil {
		return f, fmt.Errorf("the stored bundle: %w", err)
	}
	f.root, f.bundle, f.signed, f.issuers = root, b, signed, issuers
	return f, nil
}

func parseFleetRoot(p []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(p)
	if block == nil {
		return nil, errors.New("the fleet root isn't a PEM certificate")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the fleet root: %w", err)
	}
	if !c.IsCA || c.CheckSignatureFrom(c) != nil {
		return nil, errors.New("the fleet root isn't a self-signed CA")
	}
	return c, nil
}

// Root is the pinned fleet root, nil when the node trusts no fleet.
func (f *Fleet) Root() *x509.Certificate {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.root
}

// Bundle is the bundle the node applies and its issuing CAs - nil when
// it has none.
func (f *Fleet) Bundle() (*Bundle, []*x509.Certificate) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.bundle, f.issuers
}

// Signed is the bundle as the root signed it, nil without one.
func (f *Fleet) Signed() []byte {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.signed
}

// Set pins rootPEM (only when no root is pinned; afterwards it must be
// the same, or empty) and applies signed, a bundle the root signed and
// newer than the node's - the same one again changes nothing.
func (f *Fleet) Set(rootPEM, signed []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	root := f.root
	if len(rootPEM) > 0 {
		r, err := parseFleetRoot(rootPEM)
		if err != nil {
			return err
		}
		if root != nil && !root.Equal(r) {
			return ErrFleetRootMismatch
		}
		root = r
	}
	if root == nil {
		return errors.New("the node trusts no fleet yet: give it the fleet's root with the bundle")
	}
	if len(signed) == 0 {
		return errors.New("no bundle")
	}
	b, issuers, err := VerifyBundle(root, signed)
	if err != nil {
		return err
	}
	if signed, err = CanonicalBundle(signed); err != nil {
		return err
	}
	if f.bundle != nil {
		switch {
		case b.Version < f.bundle.Version:
			return fmt.Errorf("bundle version %d is older than the node's (%d)", b.Version, f.bundle.Version)
		case b.Version == f.bundle.Version && bytes.Equal(signed, f.signed):
			return nil
		case b.Version == f.bundle.Version:
			return fmt.Errorf("the node already has another bundle version %d", b.Version)
		}
	}
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	if f.root == nil {
		if err := writeDurably(filepath.Join(f.dir, FleetRootFile), encodePEM(caCertPEMType, root.Raw), 0o644); err != nil {
			return err
		}
	}
	if err := writeDurably(filepath.Join(f.dir, FleetBundleFile), signed, 0o644); err != nil {
		return err
	}
	f.root, f.bundle, f.signed, f.issuers = root, b, signed, issuers
	return nil
}

// Reset forgets the fleet.
func (f *Fleet) Reset() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, name := range []string{FleetBundleFile, FleetRootFile} {
		if err := os.Remove(filepath.Join(f.dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := syncDir(f.dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	f.root, f.bundle, f.signed, f.issuers = nil, nil, nil, nil
	return nil
}

// AcceptChains is the TLS check behind ClientCAs (local CA and fleet
// root): a chain the node's own CA anchors, or one the fleet's root
// anchors whose leaf the root itself or an issuing CA of the bundle
// signed. A CA the bundle dropped no longer lets anyone in.
func (f *Fleet) AcceptChains(chains [][]*x509.Certificate, local *x509.Certificate) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, chain := range chains {
		if len(chain) < 2 {
			continue
		}
		anchor, issuer := chain[len(chain)-1], chain[1]
		if anchor.Equal(local) {
			return nil
		}
		if f.root == nil || !anchor.Equal(f.root) {
			continue
		}
		if issuer.Equal(f.root) {
			return nil
		}
		for _, ca := range f.issuers {
			if issuer.Equal(ca) {
				return nil
			}
		}
	}
	return errors.New("the client certificate's issuer isn't one this node trusts")
}

// writeDurably replaces path with data so that a power cut leaves either
// the old file or the new one: temporary file, fsync, rename, directory
// fsync.
func writeDurably(path string, data []byte, mode os.FileMode) error {
	if err := writeAtomic(path, data, mode); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
