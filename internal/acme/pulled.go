package acme

import (
	"errors"
	"fmt"
	"strings"
)

// InstallPulled writes a certificate another cluster member obtained
// (ClusterService.ClusterPull, docs/cluster.md): the chain and key as the
// member keeps them, under name, and swaps it into HAProxy. The
// certificate needn't be in this node's own ACME configuration: the
// orderer's is, and this node serves what it pulls.
func (m *Manager) InstallPulled(name string, data []byte) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("certificate name %q: letters, digits, '.', '-' and '_'", name)
	}
	if !strings.Contains(string(data), "PRIVATE KEY-----") || !strings.Contains(string(data), "BEGIN CERTIFICATE-----") {
		return errors.New("a pulled certificate holds its chain and its key")
	}
	m.mu.Lock()
	if err := m.writeCert(name, data); err != nil {
		m.mu.Unlock()
		return err
	}
	created := m.certState(name).Created
	m.mu.Unlock()
	m.logger.Printf("certificate %s pulled from a cluster member", name)
	m.deploy(name, data, created)
	return nil
}
