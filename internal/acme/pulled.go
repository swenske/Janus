package acme

import (
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
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

// ExportConfig is the configuration as a cluster member hands it to
// another (ClusterExport, CLUSTER_ITEM_ACME_CONFIG): every field, the
// DNS providers' credentials and the path's included - over the members'
// own mutual TLS only, never logged. nil without a configuration. Not
// the account key: each member orders with its own.
func (m *Manager) ExportConfig() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg == nil {
		return nil, nil
	}
	return Marshal(m.cfg)
}

// InstallPulledConfig makes the configuration another cluster member
// exported (ExportConfig) this node's, as Apply would: a member another
// orders for (Follow) holds it for the day it becomes the orderer.
func (m *Manager) InstallPulledConfig(data []byte) error {
	cfg := &janusv1alpha1.ACMEConfig{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("the pulled configuration: %w", err)
	}
	errs, err := m.Apply(cfg, "", false)
	if err != nil {
		if len(errs) > 0 {
			return fmt.Errorf("%v: %s", err, strings.Join(errs, "; "))
		}
		return err
	}
	m.logger.Printf("configuration pulled from a cluster member: %d certificate(s)", len(cfg.Certificates))
	return nil
}
