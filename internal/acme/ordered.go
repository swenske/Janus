package acme

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/events"
)

// Ordered by the Controller (path CONTROLLER_ORDERED, docs/letsencrypt.md
// "Ordered by the Controller"): the node never reaches the CA. For each
// certificate, the Controller - or another orchestrator - asks for a
// certificate signing request (CertificateRequest: a new key, kept on
// STATE until its certificate comes), orders it from the configured CA
// with its own account, and hands back the chain (InstallCertificate) -
// or why it couldn't. The node checks it, writes it and swaps it into the
// running HAProxy as it does its own. The keys never leave the node.

// pendingDir holds the keys of the requests waiting for their
// certificate, under Dir.
const pendingDir = "acme/pending"

// ErrNoCertificate is a name no configured certificate has.
var ErrNoCertificate = errors.New("no certificate is configured with this name")

func pendingPath(name string) string { return filepath.Join(Dir, pendingDir, name+".key") }

// certificate is the configured certificate name. Called with mu held.
func (m *Manager) certificate(name string) *janusv1alpha1.ACMECertificate {
	for _, c := range m.cfg.GetCertificates() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// CertificateRequest makes a new key for the certificate name, keeps it
// until its certificate comes - replacing a request waiting - and returns
// its certificate signing request.
func (m *Manager) CertificateRequest(name string) (*janusv1alpha1.ACMECertificateRequestResponse, error) {
	if !m.Available() {
		return nil, errors.New("the letsencrypt extension isn't in this image")
	}
	m.mu.Lock()
	c := m.certificate(name)
	m.mu.Unlock()
	if c == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoCertificate, name)
	}
	key, err := newKey(c.KeyType)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.CertificateRequest{DNSNames: c.Domains}
	if len(c.Domains[0]) <= 64 { // a common name longer than that is refused
		tmpl.Subject = pkix.Name{CommonName: c.Domains[0]}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.certificate(name) == nil { // removed meanwhile
		return nil, fmt.Errorf("%w: %q", ErrNoCertificate, name)
	}
	if err := writeDurable(filepath.Join(Dir, pendingDir), name+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	m.logger.Printf("certificate %s: a request made for the Controller to order (%s, %s)", name, c.KeyType, joinDomains(c.Domains))
	return &janusv1alpha1.ACMECertificateRequestResponse{
		Name: name, CsrPem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})),
		Domains: c.Domains, KeyType: c.KeyType,
	}, nil
}

// InstallCertificate installs chain, the certificate the CA issued for
// name's waiting request, and swaps it into HAProxy; with failure instead,
// it keeps why it couldn't be obtained, and when it will be tried again.
func (m *Manager) InstallCertificate(name, chain, failure string, next time.Time) (*janusv1alpha1.ACMECertificateStatus, error) {
	if !m.Available() {
		return nil, errors.New("the letsencrypt extension isn't in this image")
	}
	m.mu.Lock()
	c := m.certificate(name)
	if c == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrNoCertificate, name)
	}
	dir := ResolveDirectory(m.cfg.Account.GetDirectory())
	cs := m.certState(name)
	now := time.Now()
	if failure != "" {
		cs.LastAttempt, cs.LastError, cs.NextAttempt = now, failure, next
		cs.Failures++
		if err := m.saveState(); err != nil {
			m.logger.Printf("save the state: %v", err)
		}
		st := m.certStatusLocked(c, dir, now)
		m.mu.Unlock()
		m.logger.Printf("certificate %s: the Controller couldn't obtain it: %s - next attempt %s", name, failure, nextAttempt(next))
		events.Publish("acme.failed", map[string]any{"name": name, "error": failure, "failures": cs.Failures})
		return st, nil
	}
	key, err := os.ReadFile(pendingPath(name))
	if errors.Is(err, os.ErrNotExist) {
		m.mu.Unlock()
		return nil, fmt.Errorf("no request is waiting for %s - ask for one first", name)
	}
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	data, leaf, err := checkIssued(chain, string(key), c.Domains)
	if err == nil && keyTypeOf(leaf) != c.KeyType {
		err = fmt.Errorf("the certificate's key is %s, not %s", keyTypeOf(leaf), c.KeyType)
	}
	if err == nil {
		err = m.writeCert(name, data)
	}
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	cs.Directory, cs.Thumbprint, cs.Profile = dir, m.answerThumbLocked(), c.Profile
	cs.LastAttempt, cs.LastSuccess, cs.LastError, cs.Failures, cs.NextAttempt = now, now, "", 0, time.Time{}
	cs.ARISerial, cs.ARIRenewAt, cs.ARICheck = "", time.Time{}, time.Time{}
	created := cs.Created
	delete(m.forced, name)
	if err := m.saveState(); err != nil {
		m.logger.Printf("save the state: %v", err)
	}
	if err := os.Remove(pendingPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.logger.Printf("certificate %s: remove its request's key: %v", name, err)
	}
	st := m.certStatusLocked(c, dir, now)
	m.mu.Unlock()
	m.logger.Printf("certificate %s installed, ordered by the Controller: serial %s, valid until %s", name, serialHex(leaf), leaf.NotAfter.Format(time.RFC3339))
	events.Publish("acme.issued", map[string]any{"name": name, "serial": serialHex(leaf), "not_after": leaf.NotAfter.Unix()})
	m.deploy(name, data, created)
	return st, nil
}
