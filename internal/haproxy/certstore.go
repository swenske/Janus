package haproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/swenske/Janus/internal/events"
)

// The certificate store: what CertificateUpload put into HAProxy at
// runtime only lives in that process - a reload or a restart starts a new
// one from the configuration, without it. With CertStoreDir set, every
// uploaded certificate (bundle and crt-list binding) is also written
// there, and put back into each new process right after it starts.
// CertificateDelete removes it.
//
// On a node, CertStoreDir is under /etc/haproxy - STATE's haproxy/
// directory - so the certificates survive reboots and upgrades like the
// configuration they go with.

const certIndexFile = "index.json"

// storedCert is one entry of the store's index.
type storedCert struct {
	Name    string   `json:"name"`
	File    string   `json:"file"` // in CertStoreDir
	CrtList string   `json:"crt_list,omitempty"`
	SNI     []string `json:"sni,omitempty"`
}

// readCertIndex returns the stored certificates. Called with certMu held.
func (m *Manager) readCertIndex() ([]storedCert, error) {
	data, err := os.ReadFile(filepath.Join(m.CertStoreDir, certIndexFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var certs []storedCert
	if err := json.Unmarshal(data, &certs); err != nil {
		return nil, fmt.Errorf("certificate store %s: %w", m.CertStoreDir, err)
	}
	return certs, nil
}

// writeCertIndex replaces the index, durably. Called with certMu held.
func (m *Manager) writeCertIndex(certs []storedCert) error {
	data, err := json.MarshalIndent(certs, "", "  ")
	if err != nil {
		return err
	}
	return writeDurable(m.CertStoreDir, certIndexFile, append(data, '\n'), 0o600)
}

// storeCert records an uploaded certificate. The bundle holds a private
// key: the store is 0700, its files 0600.
func (m *Manager) storeCert(name string, pemBundle []byte, crtList string, sni []string) error {
	if m.CertStoreDir == "" {
		return nil
	}
	m.certMu.Lock()
	defer m.certMu.Unlock()
	if err := os.MkdirAll(m.CertStoreDir, 0o700); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(name))
	file := hex.EncodeToString(sum[:8]) + ".pem"
	if err := writeDurable(m.CertStoreDir, file, pemBundle, 0o600); err != nil {
		return err
	}
	certs, err := m.readCertIndex()
	if err != nil {
		return err
	}
	entry := storedCert{Name: name, File: file, CrtList: crtList, SNI: sni}
	replaced := false
	for i := range certs {
		if certs[i].Name == name {
			certs[i], replaced = entry, true
		}
	}
	if !replaced {
		certs = append(certs, entry)
	}
	return m.writeCertIndex(certs)
}

// forgetCert removes a certificate from the store; found reports
// whether it was there.
func (m *Manager) forgetCert(name string) (found bool, err error) {
	if m.CertStoreDir == "" {
		return false, nil
	}
	m.certMu.Lock()
	defer m.certMu.Unlock()
	certs, err := m.readCertIndex()
	if err != nil {
		return false, err
	}
	kept := certs[:0]
	var gone *storedCert
	for i := range certs {
		if certs[i].Name == name {
			gone = &storedCert{File: certs[i].File}
			continue
		}
		kept = append(kept, certs[i])
	}
	if gone == nil {
		return false, nil
	}
	if err := m.writeCertIndex(kept); err != nil {
		return true, err
	}
	if err := os.Remove(filepath.Join(m.CertStoreDir, gone.File)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	return true, nil
}

// StoredCertificates lists the names of the certificates in the store.
func (m *Manager) StoredCertificates() ([]string, error) {
	if m.CertStoreDir == "" {
		return nil, nil
	}
	m.certMu.Lock()
	defer m.certMu.Unlock()
	certs, err := m.readCertIndex()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(certs))
	for _, c := range certs {
		names = append(names, c.Name)
	}
	return names, nil
}

// restoreCerts puts the stored certificates into the process that now
// answers on the stats socket. A certificate the new configuration can't
// take (its crt-list is gone, say) is logged and stays stored. Called
// with mu held, right after a start or reload.
func (m *Manager) restoreCerts(p *process) {
	if m.CertStoreDir == "" {
		return
	}
	m.certMu.Lock()
	certs, err := m.readCertIndex()
	m.certMu.Unlock()
	if err != nil {
		log.Printf("haproxy: certificate store: %v", err)
		return
	}
	if len(certs) == 0 {
		return
	}
	m.waitAnswering(p)
	restored, failed := 0, 0
	for _, c := range certs {
		bundle, err := os.ReadFile(filepath.Join(m.CertStoreDir, c.File))
		if err == nil {
			err = m.loadCert(c.Name, bundle)
		}
		if err == nil && c.CrtList != "" {
			err = m.bindCert(c.Name, c.CrtList, c.SNI)
		}
		if err != nil {
			failed++
			log.Printf("haproxy: restore certificate %s: %v", c.Name, err)
			continue
		}
		restored++
	}
	events.Publish("haproxy.certificates_restored", map[string]any{"restored": restored, "failed": failed})
}

// bindCert adds name to crtList unless it's already there - a crt-list
// file on disk may list it, and HAProxy loaded it from there.
func (m *Manager) bindCert(name, crtList string, sni []string) error {
	listArg, err := cliToken("crt-list", crtList)
	if err != nil {
		return err
	}
	if out, err := m.statsCommand("show ssl crt-list " + listArg); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) > 0 && f[0] == name {
				return nil
			}
		}
	}
	return m.addToCrtList(name, crtList, sni)
}

// writeDurable writes dir/name through a temporary file, fsynced and
// renamed, then fsyncs dir: it's on disk when this returns.
func writeDurable(dir, name string, data []byte, perm os.FileMode) error {
	tmp := filepath.Join(dir, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
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
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
