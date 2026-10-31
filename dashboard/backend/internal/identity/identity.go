// Package identity is the Controller's own TLS identity: what its
// registration port and its Let's Encrypt relay serve, what its page
// serves until it's given a certificate of its own, and what nodes are
// provisioned to check the Controller with.
//
// It is a CA kept for good - its key sealed with the master key - and a
// certificate it signs, valid a year and renewed by itself 35 days before
// it ends (or as soon as -advertise-address names something it doesn't
// cover). Nodes, the relay's users and janusctl are given the CA: a
// renewed certificate changes nothing for them.
//
// A Controller made before this had only the certificate, signed by a CA
// whose key was thrown away, and everything it provisioned pins that very
// certificate. It keeps serving it until 35 days before it ends - what's
// provisioned meanwhile gets the CA and it (TrustPEM) - then serves one
// the CA signs.
package identity

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/pki"
)

// The files, in the data directory. CertFile and KeyFile are the ones
// every release reads: the certificate served (followed by the CA once
// the CA signs it) and its key.
const (
	CAFile    = "dashboard-identity-ca.crt"
	CAKeyFile = "dashboard-identity-ca.key.sealed"
	CertFile  = "dashboard-identity.crt"
	KeyFile   = "dashboard-identity.key"

	caPurpose = "identity-ca-key"
	caName    = "Janus Controller identity CA"
	leafName  = "dashboard"
)

// Durations: vars, the tests change them.
var (
	caValidity   = 20 * 365 * 24 * time.Hour
	leafValidity = pki.LeafValidity
	// RenewBefore is how long before its end the certificate is renewed -
	// more than the 30 days under which the page warns of an expiry.
	RenewBefore = 35 * 24 * time.Hour
)

// Sealer seals the CA's key: the master key.
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

// Identity is the Controller's TLS identity.
type Identity struct {
	dir       string
	advertise []string
	now       func() time.Time

	ca  *pki.CA
	cur atomic.Pointer[served]

	mu sync.Mutex // renewals
}

type served struct {
	cert *tls.Certificate
	// legacy: a certificate the CA didn't sign - the first one of a
	// Controller made before the CA was kept.
	legacy bool
	// previous is the certificate served before this one, replaced at
	// replaced: an SSH sign-in begun against it is still good a moment.
	previous *tls.Certificate
	replaced time.Time
}

// Open loads the identity kept in dataDir, making what's missing: the CA
// (its key sealed with sealer), the certificate. advertise is
// -advertise-address: comma-separated addresses and names the
// certificate must cover besides this host's own.
func Open(dataDir string, sealer Sealer, advertise string) (*Identity, error) {
	id := &Identity{dir: dataDir, now: time.Now}
	for _, e := range strings.Split(advertise, ",") {
		if e = strings.TrimSpace(e); e != "" {
			id.advertise = append(id.advertise, e)
		}
	}
	ca, err := loadOrCreateCA(dataDir, sealer)
	if err != nil {
		return nil, err
	}
	id.ca = ca
	cert, err := tls.LoadX509KeyPair(id.path(CertFile), id.path(KeyFile))
	switch {
	case err == nil:
		if cert.Leaf == nil {
			cert.Leaf, _ = x509.ParseCertificate(cert.Certificate[0])
		}
		legacy := cert.Leaf.CheckSignatureFrom(ca.Cert) != nil
		if !legacy && len(cert.Certificate) == 1 {
			cert.Certificate = append(cert.Certificate, ca.Cert.Raw)
		}
		id.cur.Store(&served{cert: &cert, legacy: legacy})
		if _, err := id.Refresh(); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
		if _, err := id.issue(nil); err != nil {
			return nil, err
		}
	default:
		// A key that isn't the certificate's - a renewal cut between its
		// two files: the CA signs a new one, which what trusts the CA
		// takes.
		log.Printf("identity: %s and %s don't load together (%v): the identity CA signs a new certificate", CertFile, KeyFile, err)
		if _, err := id.issue(nil); err != nil {
			return nil, err
		}
	}
	return id, nil
}

func (id *Identity) path(name string) string { return filepath.Join(id.dir, name) }

func loadOrCreateCA(dir string, sealer Sealer) (*pki.CA, error) {
	certPath, keyPath := filepath.Join(dir, CAFile), filepath.Join(dir, CAKeyFile)
	certPEM, err := os.ReadFile(certPath)
	if err == nil {
		sealed, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("the identity CA's key: %w", err)
		}
		keyPEM, err := sealer.Open(sealed, caPurpose)
		if err != nil {
			return nil, fmt.Errorf("the identity CA's key (%s): %w - is this the master key it was sealed with?", keyPath, err)
		}
		ca, err := pki.LoadCA(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("the identity CA: %w", err)
		}
		return ca, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ca, err := pki.NewCAFor(caName, caValidity)
	if err != nil {
		return nil, err
	}
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		return nil, err
	}
	sealed, err := sealer.Seal(keyPEM, caPurpose)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The key first: the certificate there means the CA is whole.
	if err := writeAtomic(keyPath, sealed); err != nil {
		return nil, err
	}
	if err := writeAtomic(certPath, ca.CertPEM); err != nil {
		return nil, err
	}
	return ca, nil
}

// Certificate is the certificate served, followed by the CA when the CA
// signed it.
func (id *Identity) Certificate() *tls.Certificate { return id.cur.Load().cert }

// GetCertificate is a tls.Config's GetCertificate serving it.
func (id *Identity) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return id.Certificate(), nil
}

// CA is the identity's CA.
func (id *Identity) CA() *x509.Certificate { return id.ca.Cert }

// CAPEM is the CA's certificate, PEM.
func (id *Identity) CAPEM() []byte { return id.ca.CertPEM }

// Legacy reports whether the certificate served is still the first one
// of a Controller made before the CA was kept.
func (id *Identity) Legacy() bool { return id.cur.Load().legacy }

// SwitchAt is when a legacy certificate gives way to one the CA signs -
// zero when the CA already signs the one served.
func (id *Identity) SwitchAt() time.Time {
	s := id.cur.Load()
	if !s.legacy {
		return time.Time{}
	}
	return s.cert.Leaf.NotAfter.Add(-RenewBefore)
}

// TrustPEM is what to check the Controller with: the CA - and, while it's
// served, the legacy certificate, which whatever was given it before
// keeps checking.
func (id *Identity) TrustPEM() []byte {
	out := append([]byte{}, id.ca.CertPEM...)
	if s := id.cur.Load(); s.legacy {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.cert.Certificate[0]})...)
	}
	return out
}

// PreviousFingerprint is the SHA-256 (hex) of the certificate served
// before this one, if it was replaced less than within ago.
func (id *Identity) PreviousFingerprint(within time.Duration) string {
	s := id.cur.Load()
	if s.previous == nil || id.now().Sub(s.replaced) > within {
		return ""
	}
	return pki.Fingerprint(s.previous.Certificate[0])
}

// Status is what the Controller's page shows of it.
type Status struct {
	CAFingerprint string    `json:"ca_fingerprint"`
	CANotAfter    time.Time `json:"ca_not_after"`
	CAPEM         string    `json:"ca_pem"`
	NotAfter      time.Time `json:"not_after"`
	// Legacy: the first certificate of a Controller made before the CA
	// was kept is still served, until SwitchAt.
	Legacy   bool      `json:"legacy"`
	SwitchAt time.Time `json:"switch_at,omitzero"`
}

// Status is the identity now.
func (id *Identity) Status() Status {
	s := id.cur.Load()
	return Status{
		CAFingerprint: pki.Fingerprint(id.ca.Cert.Raw), CANotAfter: id.ca.Cert.NotAfter, CAPEM: string(id.ca.CertPEM),
		NotAfter: s.cert.Leaf.NotAfter, Legacy: s.legacy, SwitchAt: id.SwitchAt(),
	}
}

// Refresh renews the certificate when it ends within RenewBefore, or
// when -advertise-address names something it doesn't cover - true when
// it did. The new one names everything the one before did, and this
// host's addresses now.
func (id *Identity) Refresh() (bool, error) {
	id.mu.Lock()
	defer id.mu.Unlock()
	s := id.cur.Load()
	if s.cert.Leaf.NotAfter.Sub(id.now()) > RenewBefore && len(id.missing(s.cert.Leaf)) == 0 {
		return false, nil
	}
	if _, err := id.issueLocked(s.cert.Leaf); err != nil {
		return false, err
	}
	return true, nil
}

// RenewForTests renews the certificate now - another package's test.
// Panics outside a test.
func (id *Identity) RenewForTests() error {
	if !testing.Testing() {
		panic("identity: RenewForTests outside a test")
	}
	id.mu.Lock()
	defer id.mu.Unlock()
	_, err := id.issueLocked(id.cur.Load().cert.Leaf)
	return err
}

// missing are the -advertise-address entries leaf doesn't cover.
func (id *Identity) missing(leaf *x509.Certificate) []string {
	var out []string
	for _, e := range id.advertise {
		if ip := net.ParseIP(e); ip != nil {
			if !slices.ContainsFunc(leaf.IPAddresses, ip.Equal) {
				out = append(out, e)
			}
		} else if !slices.ContainsFunc(leaf.DNSNames, func(n string) bool { return strings.EqualFold(n, e) }) {
			out = append(out, e)
		}
	}
	return out
}

func (id *Identity) issue(prev *x509.Certificate) (*tls.Certificate, error) {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.issueLocked(prev)
}

// issueLocked signs a new certificate - its names prev's (if any), the
// loopback addresses, this host's and -advertise-address's -, writes it
// (key first, each file replaced whole) and serves it.
func (id *Identity) issueLocked(prev *x509.Certificate) (*tls.Certificate, error) {
	dnsNames := []string{"localhost"}
	ips := append([]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, pki.LocalIPs()...)
	if prev != nil {
		dnsNames = append(dnsNames, prev.DNSNames...)
		ips = append(ips, prev.IPAddresses...)
	}
	for _, e := range id.advertise {
		if ip := net.ParseIP(e); ip != nil {
			ips = append(ips, ip)
		} else {
			dnsNames = append(dnsNames, e)
		}
	}
	validity := leafValidity
	if left := time.Until(id.ca.Cert.NotAfter); left < validity {
		validity = left
	}
	if validity <= 0 {
		return nil, fmt.Errorf("the identity CA expired on %s", id.ca.Cert.NotAfter.UTC().Format(time.DateOnly))
	}
	certPEM, keyPEM, err := id.ca.Issue(pki.IssueOptions{
		CommonName:  leafName,
		DNSNames:    uniqueNames(dnsNames),
		IPAddresses: uniqueIPs(ips),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		Validity:    validity,
	})
	if err != nil {
		return nil, fmt.Errorf("issue the Controller's certificate: %w", err)
	}
	chainPEM := append(certPEM, id.ca.CertPEM...)
	cert, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(id.path(KeyFile), keyPEM); err != nil {
		return nil, err
	}
	if err := writeAtomic(id.path(CertFile), chainPEM); err != nil {
		return nil, err
	}
	next := &served{cert: &cert}
	if old := id.cur.Load(); old != nil {
		next.previous, next.replaced = old.cert, id.now()
	}
	id.cur.Store(next)
	what := "made"
	if prev != nil {
		what = "renewed"
	}
	log.Printf("identity: the Controller's certificate %s - valid until %s, signed by its identity CA (SHA-256 %s)", what, cert.Leaf.NotAfter.UTC().Format(time.DateOnly), pki.Fingerprint(id.ca.Cert.Raw))
	return &cert, nil
}

func uniqueNames(in []string) []string {
	var out []string
	for _, n := range in {
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, n) }) {
			out = append(out, n)
		}
	}
	return out
}

func uniqueIPs(in []net.IP) []net.IP {
	var out []net.IP
	for _, ip := range in {
		if ip != nil && !slices.ContainsFunc(out, ip.Equal) {
			out = append(out, ip)
		}
	}
	return out
}

// writeAtomic replaces path whole: a temporary file, fsync, rename,
// the directory synced.
func writeAtomic(path string, data []byte) error {
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
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// HoldsCA reports whether pemData holds the identity's CA - what a
// node's relay settings are checked for.
func (id *Identity) HoldsCA(pemData []byte) bool {
	for rest := pemData; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			return false
		}
		if b.Type == "CERTIFICATE" && bytes.Equal(b.Bytes, id.ca.Cert.Raw) {
			return true
		}
	}
}
