// Package uitls is the certificate the Controller's main HTTPS port
// serves - its page, janusctl, the Terraform provider: one given as
// files (-tls-cert/-tls-key, read again when they change - a renewal by
// certbot or the like needs no restart), else one uploaded on the page
// (its key sealed with the master key), else the Controller's own
// self-signed identity. Nodes register against that identity on the
// registration port whatever this serves: a node provisioned with it
// keeps trusting it when the page gets a certificate of its own.
package uitls

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
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
	"time"
)

// Where the served certificate comes from.
const (
	SourceFiles      = "files"
	SourceUploaded   = "uploaded"
	SourceSelfSigned = "self-signed"
)

// Sealer seals the uploaded certificate's key: the master key.
type Sealer interface {
	Seal(plaintext []byte, purpose string) ([]byte, error)
	Open(sealed []byte, purpose string) ([]byte, error)
}

const (
	fileName   = "ui-tls.json"
	keyPurpose = "ui-tls-key"
	// reloadEvery bounds how often the files are looked at again.
	reloadEvery = 30 * time.Second
	// MaxBundle is the most a PEM bundle may weigh.
	MaxBundle = 64 << 10
)

// ErrFromFiles refuses changing a certificate the flags give.
var ErrFromFiles = errors.New("the certificate comes from -tls-cert/-tls-key: replace those files (read again within 30 s), or drop the flags to manage it here")

type served struct {
	cert   *tls.Certificate
	source string
}

// Store serves the main port's certificate.
type Store struct {
	path       string
	sealer     Sealer
	selfSigned tls.Certificate
	certFile   string
	keyFile    string
	now        func() time.Time

	cur atomic.Pointer[served]

	mu        sync.Mutex
	lastCheck time.Time
	stamp     [2]fileStamp
}

type fileStamp struct {
	mod  time.Time
	size int64
}

type stored struct {
	ChainPEM  string `json:"chain_pem"`
	KeySealed []byte `json:"key_sealed"`
}

// Open picks the certificate to serve: the files when certFile/keyFile
// are given (both or neither), else the one uploaded into dataDir, else
// selfSigned. An uploaded one that no longer loads is logged and left
// for the self-signed identity - the page must stay reachable to fix it.
func Open(dataDir string, sealer Sealer, selfSigned tls.Certificate, certFile, keyFile string) (*Store, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, errors.New("-tls-cert and -tls-key must both be set together")
	}
	s := &Store{path: filepath.Join(dataDir, fileName), sealer: sealer, selfSigned: selfSigned, certFile: certFile, keyFile: keyFile, now: time.Now}
	if certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("-tls-cert/-tls-key: %w", err)
		}
		s.stamp = s.stampFiles()
		s.lastCheck = s.now()
		s.cur.Store(&served{&cert, SourceFiles})
		return s, nil
	}
	s.cur.Store(&served{&s.selfSigned, SourceSelfSigned})
	cert, err := s.loadUploaded()
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		log.Printf("the uploaded HTTPS certificate doesn't load (%v): serving the self-signed one", err)
	default:
		s.cur.Store(&served{&cert, SourceUploaded})
	}
	return s, nil
}

func (s *Store) loadUploaded() (tls.Certificate, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return tls.Certificate{}, err
	}
	var st stored
	if err := json.Unmarshal(raw, &st); err != nil {
		return tls.Certificate{}, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if s.sealer == nil {
		return tls.Certificate{}, errors.New("no master key to open the key with")
	}
	keyPEM, err := s.sealer.Open(st.KeySealed, keyPurpose)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("open the key: %w", err)
	}
	return tls.X509KeyPair([]byte(st.ChainPEM), keyPEM)
}

// GetCertificate is the main port's tls.Config.GetCertificate.
func (s *Store) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if s.certFile != "" {
		s.maybeReload()
	}
	return s.cur.Load().cert, nil
}

// Current is the certificate served now and where it comes from.
func (s *Store) Current() (*tls.Certificate, string) {
	if s.certFile != "" {
		s.maybeReload()
	}
	c := s.cur.Load()
	return c.cert, c.source
}

// FromFiles reports whether the flags give the certificate.
func (s *Store) FromFiles() bool { return s.certFile != "" }

func (s *Store) stampFiles() [2]fileStamp {
	var out [2]fileStamp
	for i, p := range []string{s.certFile, s.keyFile} {
		if fi, err := os.Stat(p); err == nil {
			out[i] = fileStamp{fi.ModTime(), fi.Size()}
		}
	}
	return out
}

// maybeReload reads the files again when they changed - looked at once
// every reloadEvery at most. A pair that doesn't load (half written, a
// key not matching yet) leaves the current certificate served.
func (s *Store) maybeReload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Sub(s.lastCheck) < reloadEvery {
		return
	}
	s.lastCheck = now
	st := s.stampFiles()
	if st == s.stamp {
		return
	}
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		log.Printf("-tls-cert/-tls-key changed but don't load (%v): still serving the previous certificate", err)
		return
	}
	s.stamp = st
	s.cur.Store(&served{&cert, SourceFiles})
	log.Printf("-tls-cert/-tls-key changed: serving the new certificate")
}

// Upload makes bundle - the certificate, then its issuers, and its
// private key, PEM - the one served, at once and from now on.
func (s *Store) Upload(bundle []byte) (*tls.Certificate, error) {
	if s.certFile != "" {
		return nil, ErrFromFiles
	}
	cert, chainPEM, keyPEM, err := parse(bundle, s.now())
	if err != nil {
		return nil, err
	}
	if s.sealer == nil {
		return nil, errors.New("no master key to seal the key with")
	}
	sealed, err := s.sealer.Seal(keyPEM, keyPurpose)
	if err != nil {
		return nil, fmt.Errorf("seal the key: %w", err)
	}
	raw, err := json.Marshal(stored{ChainPEM: string(chainPEM), KeySealed: sealed})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeAtomic(s.path, raw); err != nil {
		return nil, err
	}
	s.cur.Store(&served{&cert, SourceUploaded})
	return &cert, nil
}

// Remove goes back to the self-signed identity.
func (s *Store) Remove() error {
	if s.certFile != "" {
		return ErrFromFiles
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.cur.Store(&served{&s.selfSigned, SourceSelfSigned})
	return nil
}

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

// Check parses bundle as Upload would, without serving it.
func Check(bundle []byte, now time.Time) (*tls.Certificate, error) {
	cert, _, _, err := parse(bundle, now)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

// parse reads a PEM bundle - certificates, the server's first then its
// issuers, and one private key, in any order - and checks it's
// something a browser takes: the key the certificate's, valid now, for
// servers, a key type Chromium accepts, the chain in order.
func parse(bundle []byte, now time.Time) (tls.Certificate, []byte, []byte, error) {
	fail := func(format string, a ...any) (tls.Certificate, []byte, []byte, error) {
		return tls.Certificate{}, nil, nil, fmt.Errorf(format, a...)
	}
	if len(bundle) > MaxBundle {
		return fail("more than %d KiB: a certificate, its chain and its key are a few", MaxBundle>>10)
	}
	var chainPEM, keyPEM []byte
	keys := 0
	for rest := bundle; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		switch {
		case b.Type == "CERTIFICATE":
			chainPEM = append(chainPEM, pem.EncodeToMemory(b)...)
		case strings.HasSuffix(b.Type, "PRIVATE KEY"):
			if b.Type == "ENCRYPTED PRIVATE KEY" || b.Headers["Proc-Type"] != "" {
				return fail("the private key is encrypted: give it decrypted (openssl pkey -in key.pem)")
			}
			keys++
			keyPEM = pem.EncodeToMemory(b)
		}
	}
	switch {
	case chainPEM == nil:
		return fail("no certificate (-----BEGIN CERTIFICATE-----) in what was given")
	case keys == 0:
		return fail("no private key (-----BEGIN PRIVATE KEY-----) in what was given: the certificate's key goes with it")
	case keys > 1:
		return fail("%d private keys: give the certificate's only", keys)
	}
	cert, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return fail("the key and the certificate: %v", err)
	}
	var chain []*x509.Certificate
	for _, der := range cert.Certificate {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return fail("a certificate: %v", err)
		}
		chain = append(chain, c)
	}
	leaf := chain[0]
	cert.Leaf = leaf
	if leaf.IsCA && leaf.BasicConstraintsValid {
		return fail("the first certificate is a CA's: the server's own goes first, then its issuers")
	}
	if now.Before(leaf.NotBefore) {
		return fail("the certificate is valid from %s only", leaf.NotBefore.UTC().Format(time.DateTime))
	}
	if !now.Before(leaf.NotAfter) {
		return fail("the certificate expired on %s", leaf.NotAfter.UTC().Format(time.DateTime))
	}
	if len(leaf.ExtKeyUsage) > 0 && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return fail("the certificate isn't for a server (no TLS Web Server Authentication in its extended key usage)")
	}
	switch k := leaf.PublicKey.(type) {
	case ed25519.PublicKey:
		return fail("an Ed25519 certificate: browsers (Chromium) refuse them - use ECDSA P-256 or RSA")
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return fail("an RSA key of %d bits: 2048 at least", k.N.BitLen())
		}
	case *ecdsa.PublicKey:
	default:
		return fail("a %T key: use ECDSA P-256 or RSA", k)
	}
	for i := 0; i+1 < len(chain); i++ {
		if err := chain[i].CheckSignatureFrom(chain[i+1]); err != nil {
			return fail("certificate %d isn't issued by the next one: the server's first, then each issuer in turn", i+1)
		}
	}
	return cert, chainPEM, keyPEM, nil
}

// Info is what a page shows of a certificate.
type Info struct {
	Source      string    `json:"source"`
	Subject     string    `json:"subject"`
	Issuer      string    `json:"issuer"`
	Names       []string  `json:"names"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
	Fingerprint string    `json:"fingerprint"`
	// Chain counts the certificates served, the server's included.
	Chain int `json:"chain"`
	// Warnings are what may go wrong with it: a name the page isn't
	// opened by, an expiry close.
	Warnings []string `json:"warnings"`
}

// Describe is cert's Info - host, the name the page is opened by, for
// the warnings.
func Describe(cert *tls.Certificate, source, host string, now time.Time) Info {
	leaf := cert.Leaf
	if leaf == nil {
		leaf, _ = x509.ParseCertificate(cert.Certificate[0])
	}
	sum := sha256.Sum256(cert.Certificate[0])
	info := Info{Source: source, Fingerprint: hex.EncodeToString(sum[:]), Chain: len(cert.Certificate), Names: []string{}, Warnings: []string{}}
	if leaf == nil {
		return info
	}
	info.Subject, info.Issuer = leaf.Subject.String(), leaf.Issuer.String()
	info.NotBefore, info.NotAfter = leaf.NotBefore, leaf.NotAfter
	info.Names = append(info.Names, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		info.Names = append(info.Names, ip.String())
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host != "" && leaf.VerifyHostname(host) != nil {
		info.Warnings = append(info.Warnings, fmt.Sprintf("it doesn't name %s, the address this page is opened by: browsers opening it so refuse it", host))
	}
	if left := leaf.NotAfter.Sub(now); left < 30*24*time.Hour {
		info.Warnings = append(info.Warnings, fmt.Sprintf("it expires in %d days", int(left.Hours()/24)))
	}
	return info
}
