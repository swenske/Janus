package pki

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Local is the node's own CA, replaceable while janusd runs
// (ServerCert.Rotate).
type Local struct{ ca atomic.Pointer[CA] }

func NewLocal(ca *CA) *Local {
	l := &Local{}
	l.ca.Store(ca)
	return l
}

// CA is the node's own CA, now.
func (l *Local) CA() *CA { return l.ca.Load() }

// Rotation is a node's new own CA and what it issued in place of the old
// one's certificates.
type Rotation struct {
	CA           *CA
	AdminCertPEM []byte
	// AdminKeyPEM is set when the node made the admin certificate's key
	// itself - to be shown once, like at first boot.
	AdminKeyPEM []byte
}

// rotationDir holds a rotation's files until they're all written: a
// rotation is ready - and finished at the next boot if a power cut
// interrupted it - only once rotationReady is there.
const (
	rotationDir   = "rotation"
	rotationReady = "ready"
)

// Rotate replaces the node's own CA (local, kept in s's directory): a new
// CA - cross-signed by the old one, served after the server certificate
// so whoever pinned the old CA still verifies the node and can learn the
// new one -, a server certificate for hostname and ips, and an admin
// certificate - for adminPub when given, its key never seen by the node,
// else for a key made here and returned. Every certificate the old CA
// issued stops working with the next connection. Serialized with
// Refresh, the other writer of the directory; written so that a power
// cut leaves the old CA or the new one, whole.
func (s *ServerCert) Rotate(local *Local, hostname string, ips []net.IP, adminPub crypto.PublicKey) (*Rotation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ca, err := NewCA("Janus node CA: " + hostname)
	if err != nil {
		return nil, err
	}
	crossPEM, err := s.ca.CrossSign(ca)
	if err != nil {
		return nil, err
	}
	caKeyPEM, err := ca.KeyPEM()
	if err != nil {
		return nil, err
	}
	dns, want := serverSANs(hostname, ips)
	serverPEM, serverKeyPEM, err := ca.Issue(IssueOptions{CommonName: hostname, DNSNames: dns, IPAddresses: want, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	if err != nil {
		return nil, fmt.Errorf("issue server certificate: %w", err)
	}
	server, err := tls.X509KeyPair(serverPEM, serverKeyPEM)
	if err != nil {
		return nil, err
	}
	admin := IssueOptions{CommonName: "admin", Roles: []string{RoleAdmin}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	r := &Rotation{CA: ca}
	if adminPub != nil {
		r.AdminCertPEM, err = ca.IssueFor(adminPub, admin)
	} else {
		r.AdminCertPEM, r.AdminKeyPEM, err = ca.Issue(admin)
	}
	if err != nil {
		return nil, fmt.Errorf("issue admin certificate: %w", err)
	}

	files := []file{
		{caCertFile, ca.CertPEM, 0o644},
		{caKeyFile, caKeyPEM, 0o600},
		{serverCertFile, serverPEM, 0o644},
		{serverKeyFile, serverKeyPEM, 0o600},
		{adminCertFile, r.AdminCertPEM, 0o600},
		{crossFile, crossPEM, 0o644},
	}
	var remove []string
	if r.AdminKeyPEM != nil {
		files = append(files, file{adminKeyFile, r.AdminKeyPEM, 0o600})
	} else {
		remove = append(remove, adminKeyFile) // not the node's to keep
	}
	if err := writeRotation(s.dir, files, remove); err != nil {
		return nil, err
	}
	local.ca.Store(ca)
	s.ca = ca
	block, _ := pem.Decode(crossPEM)
	s.cross = block.Bytes
	s.store(server)
	return r, nil
}

// writeRotation writes files to dir's rotation directory, durably, marks
// it ready - the commit point - and moves them in place (completeRotation).
func writeRotation(dir string, files []file, remove []string) error {
	rd := filepath.Join(dir, rotationDir)
	if err := os.RemoveAll(rd); err != nil {
		return err
	}
	if err := os.MkdirAll(rd, 0o700); err != nil {
		return err
	}
	for _, f := range files {
		if err := writeDurably(filepath.Join(rd, f.name), f.data, f.mode); err != nil {
			return err
		}
	}
	if err := writeDurably(filepath.Join(rd, rotationReady), []byte(strings.Join(remove, "\n")), 0o600); err != nil {
		return err
	}
	return completeRotation(dir)
}

// completeRotation finishes a rotation that was ready - moving each of
// its files in place, removing those it removes - and forgets one that
// wasn't: the node keeps its old CA, whole. Run at every boot before the
// CA is loaded, so a power cut in the middle of the moves is finished
// then.
func completeRotation(dir string) error {
	rd := filepath.Join(dir, rotationDir)
	ready, err := os.ReadFile(filepath.Join(rd, rotationReady))
	if errors.Is(err, os.ErrNotExist) {
		return os.RemoveAll(rd)
	}
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(rd)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == rotationReady {
			continue
		}
		if err := os.Rename(filepath.Join(rd, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	for _, name := range strings.Fields(string(ready)) {
		if err := os.Remove(filepath.Join(dir, filepath.Base(name))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	if err := os.RemoveAll(rd); err != nil {
		return err
	}
	return syncDir(dir)
}
