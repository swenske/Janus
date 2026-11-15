package acme

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

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
	// Obtained by the orderer from the CA of the configuration pulled
	// with it (CLUSTER_ITEM_ACME_CONFIG comes first), for its account:
	// recorded so, a certificate still good reads valid here and isn't
	// ordered again for "the CA changed" the day this node orders - the
	// cluster's secrets come from it then (v2026.10.25-3's lab: a change
	// of the secrets' source re-issued every certificate).
	cs := m.certState(name)
	if c := m.certificate(name); c != nil {
		cs.Directory, cs.Thumbprint, cs.Profile = ResolveDirectory(m.cfg.GetAccount().GetDirectory()), m.followThumb, c.Profile
	}
	cs.LastError, cs.Failures, cs.NextAttempt = "", 0, time.Time{}
	cs.ARISerial, cs.ARIRenewAt, cs.ARICheck = "", time.Time{}, time.Time{}
	if err := m.saveState(); err != nil {
		m.mu.Unlock()
		return err
	}
	created := cs.Created
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
// orders for (Follow) holds it for the day it becomes the orderer. A
// certificate it no longer has that this node's haproxy.cfg still uses
// is kept - the rest, the secrets with it, installed all the same - until
// this node's configuration no longer uses it: the member that exported
// it may run a newer haproxy.cfg already (a cluster's version applied
// member by member, Let's Encrypt's with it).
func (m *Manager) InstallPulledConfig(data []byte) error {
	cfg := &janusv1alpha1.ACMEConfig{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("the pulled configuration: %w", err)
	}
	m.mu.Lock()
	var gone []*janusv1alpha1.ACMECertificate
	for _, c := range m.cfg.GetCertificates() {
		if !slices.ContainsFunc(cfg.Certificates, func(n *janusv1alpha1.ACMECertificate) bool { return n.Name == c.Name }) {
			gone = append(gone, c)
		}
	}
	var kept []string
	if len(gone) > 0 {
		names := make([]string, len(gone))
		for i, c := range gone {
			names[i] = c.Name
		}
		if len(m.checkRemovable(names)) > 0 {
			for _, c := range gone {
				cfg.Certificates = append(cfg.Certificates, proto.Clone(c).(*janusv1alpha1.ACMECertificate))
			}
			kept = names
		}
	}
	m.mu.Unlock()
	if len(kept) > 0 {
		m.logger.Printf("the pulled configuration has no %s: kept while this node's haproxy.cfg uses it", strings.Join(kept, ", "))
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
