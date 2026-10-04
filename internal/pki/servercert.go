package pki

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// renewBefore is how long before expiry the server certificate is
// reissued anyway.
const renewBefore = 30 * 24 * time.Hour

// ServerCert is the node's TLS server certificate, reissued on the fly
// when what it must cover changes - a new address (static configuration,
// a VLAN, a DHCP lease on another subnet) or hostname - or when it nears
// expiry. Without this, clients dialing a new address fail verification
// ("certificate is valid for X, not Y"): the certificate used to be
// issued once, at first boot, for the addresses of that moment only.
// New TLS connections get the new certificate; established ones keep
// theirs.
type ServerCert struct {
	ca  *CA
	dir string

	mu  sync.Mutex // serializes Refresh
	cur atomic.Pointer[tls.Certificate]
}

func NewServerCert(ca *CA, dir string, initial tls.Certificate) *ServerCert {
	s := &ServerCert{ca: ca, dir: dir}
	s.cur.Store(&initial)
	return s
}

// NotAfter is the current certificate's expiry (zero if it can't be read).
func (s *ServerCert) NotAfter() time.Time {
	c := s.cur.Load()
	if c.Leaf != nil {
		return c.Leaf.NotAfter
	}
	if len(c.Certificate) == 0 {
		return time.Time{}
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return time.Time{}
	}
	return leaf.NotAfter
}

// GetCertificate is the tls.Config hook.
func (s *ServerCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return s.cur.Load(), nil
}

// Refresh reissues the certificate if its SANs aren't exactly hostname,
// localhost, 127.0.0.1 and ips, or if it expires within 30 days - and
// persists it, so a restart serves it too. Returns whether it reissued.
func (s *ServerCert) Refresh(hostname string, ips []net.IP) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dns, want := serverSANs(hostname, ips)
	leaf, err := x509.ParseCertificate(s.cur.Load().Certificate[0])
	if err != nil {
		return false, fmt.Errorf("parse the current server certificate: %w", err)
	}
	if sameStrings(leaf.DNSNames, dns) && sameIPs(leaf.IPAddresses, want) && time.Until(leaf.NotAfter) > renewBefore {
		return false, nil
	}

	certPEM, keyPEM, err := s.ca.Issue(IssueOptions{
		CommonName:  hostname,
		DNSNames:    dns,
		IPAddresses: want,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return false, fmt.Errorf("issue server certificate: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false, err
	}
	// Each file is replaced atomically, but not the pair: a power cut
	// between the two renames leaves a key and a certificate that don't
	// match - LoadOrBootstrap then reissues the certificate from the CA
	// rather than fail.
	if err := writeAtomic(filepath.Join(s.dir, serverKeyFile), keyPEM, 0o600); err != nil {
		return false, err
	}
	if err := writeAtomic(filepath.Join(s.dir, serverCertFile), certPEM, 0o644); err != nil {
		return false, err
	}
	s.cur.Store(&cert)
	return true, nil
}

// serverSANs is what the server certificate covers: the hostname and
// localhost, 127.0.0.1 and every given address, sorted and unique.
func serverSANs(hostname string, ips []net.IP) ([]string, []net.IP) {
	dns := []string{hostname, "localhost"}
	if hostname == "localhost" || hostname == "" {
		dns = []string{"localhost"}
	}
	all := append([]net.IP{net.ParseIP("127.0.0.1")}, ips...)
	var out []net.IP
	for _, ip := range all {
		if ip.IsLinkLocalUnicast() {
			continue // unusable without an interface zone
		}
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		if !slices.ContainsFunc(out, ip.Equal) {
			out = append(out, ip)
		}
	}
	slices.SortFunc(out, func(a, b net.IP) int { return slices.Compare(a, b) })
	return dns, out
}

func sameStrings(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func sameIPs(a, b []net.IP) bool {
	if len(a) != len(b) {
		return false
	}
	for _, ip := range a {
		if !slices.ContainsFunc(b, ip.Equal) {
			return false
		}
	}
	return true
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
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
