package clusters

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme"
)

// Let's Encrypt in a cluster (docs/guide/clusters.md "Let's Encrypt"):
// one member orders - the one the others pull their secrets from -, the
// others order nothing (ClusterSetRequest.acme_orderer): they hold its
// configuration (pulled, CLUSTER_ITEM_ACME_CONFIG) for the day they become
// the orderer, serve its certificates (pulled, CLUSTER_ITEM_ACME_CERTS)
// and answer HTTP-01 challenges with its account's thumbprint - a
// challenge may reach any member. SyncACME keeps them in step: Reconcile
// calls it, so a renewal reaches every member within a check.
//
// A cluster that manages Let's Encrypt (domain acme, ConfigureACME) keeps
// its configuration in its versions - letsencrypt.json: the certificates,
// the account's settings, the DNS providers, how the members reach the CA
// -, the same on every member and locked there: whichever member orders,
// the others are armed alike, and a new orderer goes on the same way. Its
// secrets - DNS providers' settings, the EAB key, a proxy's password -
// are never in a version: given to the member that orders, relayed and
// never kept, the others pull them from it as each takes a version.

// ACMEState is what the cluster last read of its orderer.
type ACMEState struct {
	Orderer    string `json:"orderer,omitempty"`
	Thumbprint string `json:"thumbprint,omitempty"`
}

// SyncACME reads the orderer's letsencrypt extension; a new orderer or
// thumbprint goes to every member (ClusterSet), and a member whose
// certificates differ from the orderer's - a renewal, a certificate
// added or removed - pulls its configuration and certificates.
func (e *Engine) SyncACME(ctx context.Context, c *Cluster, actor string) error {
	src := c.Member(c.SecretsFrom)
	if src == nil || len(c.Members) < 2 {
		return nil
	}
	ost, err := e.Nodes.ACMEStatus(ctx, *src)
	if err != nil {
		return fmt.Errorf("%s's Let's Encrypt: %v", src.Name, err)
	}
	if ost.GetState() != janusv1alpha1.ModuleState_MODULE_STATE_RUNNING || !ost.GetConfigured() {
		return nil // nothing ordered there
	}
	if ost.GetOrderedBy() != "" {
		// The secrets' source changed, and still follows the one before:
		// told first, alone, that it orders now; its thumbprint, read
		// again then, goes to the others below, in this check. Told all
		// at once without it, they answered HTTP-01 with their own
		// account until the next check, and the new orderer's challenges
		// reaching them failed (v2026.10.25-3's lab).
		if _, err := e.Nodes.ClusterSet(ctx, *src, e.SetRequest(c, *src, nil, nil)); err != nil {
			return fmt.Errorf("telling %s it orders the Let's Encrypt certificates: %v", src.Name, err)
		}
		if err := e.waitLearnt(ctx, c, *src, actor); err != nil {
			return fmt.Errorf("telling %s it orders the Let's Encrypt certificates: %v", src.Name, err)
		}
		if ost, err = e.Nodes.ACMEStatus(ctx, *src); err != nil {
			return fmt.Errorf("%s's Let's Encrypt: %v", src.Name, err)
		}
		if ost.GetOrderedBy() != "" {
			return nil // it doesn't take it: the next check
		}
	}
	want := ACMEState{Orderer: src.Name, Thumbprint: ost.GetAccountThumbprint()}
	if e.Store.State(c.ID).ACME != want {
		if err := e.Store.UpdateState(c.ID, func(s *State) { s.ACME = want }); err != nil {
			return err
		}
		e.Journal(c.ID, actor, "letsencrypt", src.Name, 0, "the cluster's Let's Encrypt certificates are ordered by %s: the others order nothing, serve them and answer HTTP-01 with its account", src.Name)
		if err := e.tellOrderer(ctx, c, actor); err != nil {
			return err
		}
	}
	serials := map[string]string{}
	for _, cs := range ost.GetCertificates() {
		serials[cs.GetName()] = cs.GetSerial()
	}
	var failed []string
	for _, m := range c.Members {
		if m.Name == src.Name {
			continue
		}
		mst, err := e.Nodes.ACMEStatus(ctx, m)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", m.Name, err))
			continue
		}
		if mst.GetState() != janusv1alpha1.ModuleState_MODULE_STATE_RUNNING {
			continue // no letsencrypt extension there: it can't serve them
		}
		differ := len(mst.GetCertificates()) != len(serials)
		var renewed []string
		for _, cs := range mst.GetCertificates() {
			if s, ok := serials[cs.GetName()]; !ok {
				differ = true
			} else if s != "" && s != cs.GetSerial() {
				differ = true
				renewed = append(renewed, cs.GetName())
			}
		}
		if !differ {
			continue
		}
		// The configuration with them - unless the cluster's versions
		// carry it: what the member runs is then the version's.
		items := []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CONFIG, janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CERTS}
		if c.Manages("acme") {
			items = items[1:]
		}
		if err := e.Pull(ctx, c.ID, []string{m.Name}, items, actor); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", m.Name, err))
			continue
		}
		slices.Sort(renewed)
		if len(renewed) > 0 {
			e.Journal(c.ID, actor, "letsencrypt", m.Name, 0, "%s pulled %s's renewed certificate(s): %s", m.Name, src.Name, strings.Join(renewed, ", "))
		} else {
			e.Journal(c.ID, actor, "letsencrypt", m.Name, 0, "%s pulled %s's Let's Encrypt configuration and certificates", m.Name, src.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("the Let's Encrypt certificates not in step on %s", strings.Join(failed, "; "))
	}
	return nil
}

// tellOrderer sends every member the membership again: who orders the
// cluster's certificates, and the thumbprint known for it - one member at
// a time: one whose HTTP-01 answer changes reloads HAProxy, and learns
// its stick tables back from the others before the next one reloads
// (waitLearnt, as an apply does: members reloading together lose them).
func (e *Engine) tellOrderer(ctx context.Context, c *Cluster, actor string) error {
	var failed []string
	for _, m := range c.Members {
		if _, err := e.Nodes.ClusterSet(ctx, m, e.SetRequest(c, m, nil, nil)); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", m.Name, err))
			continue
		}
		if err := e.waitLearnt(ctx, c, m, actor); err != nil {
			return fmt.Errorf("telling the members who orders the Let's Encrypt certificates: %s: %v", m.Name, err)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("telling the members who orders the Let's Encrypt certificates: %s", strings.Join(failed, "; "))
	}
	return nil
}

// withKept is want with the certificates have configures and want
// doesn't: what a member takes before its new haproxy.cfg, which may
// still use them - they go once it's applied.
func withKept(want, have *janusv1alpha1.ACMEConfig) *janusv1alpha1.ACMEConfig {
	out := proto.Clone(want).(*janusv1alpha1.ACMEConfig)
	for _, h := range have.GetCertificates() {
		if !slices.ContainsFunc(out.Certificates, func(c *janusv1alpha1.ACMECertificate) bool { return c.GetName() == h.GetName() }) {
			out.Certificates = append(out.Certificates, proto.Clone(h).(*janusv1alpha1.ACMECertificate))
		}
	}
	return out
}

// acmeBefore puts the version's Let's Encrypt configuration want on
// member m before its haproxy.cfg: the secrets pulled first from the
// member that orders - an empty secret keeps the one m holds -, and the
// certificates m has and want doesn't kept, its haproxy.cfg perhaps still
// using them. final: want, to apply once haproxy.cfg is - nil when
// nothing was kept.
func (e *Engine) acmeBefore(ctx context.Context, c *Cluster, m Member, want *janusv1alpha1.ACMEConfig) (final *janusv1alpha1.ACMEConfig, err error) {
	if m.Name != c.SecretsFrom && c.Member(c.SecretsFrom) != nil {
		if _, err := e.Nodes.ClusterPull(ctx, m, c.SecretsFrom, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CONFIG}); err != nil {
			return nil, fmt.Errorf("pulling the Let's Encrypt secrets from %s: %v", c.SecretsFrom, err)
		}
	}
	have, err := e.Nodes.ACMEConfig(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("reading its Let's Encrypt configuration: %v", err)
	}
	first := withKept(want, have)
	if err := e.Nodes.ACMEApply(ctx, m, first, false); err != nil {
		return nil, err // letsencrypt.json: ...
	}
	if len(first.GetCertificates()) == len(want.GetCertificates()) {
		return nil, nil
	}
	return want, nil
}

// ConfigureACME makes cfg cluster id's Let's Encrypt configuration: checked
// on the member that orders - the one the secrets come from -, given to
// it whole, its secrets with it (relayed, never kept); then the cluster's
// next version, cfg in it without its secrets, applied member by member,
// the cluster managing Let's Encrypt from then on - each member pulls the
// secrets from the orderer as it takes the version. When the cluster's
// version has cfg already, no new one: the others pull the secrets, a
// member that doesn't run it takes it again. The version made, 0 if none.
func (e *Engine) ConfigureACME(ctx context.Context, id string, cfg *janusv1alpha1.ACMEConfig, note, actor string) (int, error) {
	c, ok := e.Store.Get(id)
	if !ok {
		return 0, ErrNotFound
	}
	o := c.Member(c.SecretsFrom)
	if o == nil {
		return 0, errors.New("the cluster has no member the secrets come from: the one that orders")
	}
	if c.Desired == 0 {
		return 0, errors.New("the cluster has no version yet")
	}
	canonical, err := acme.Canonical(cfg)
	if err != nil {
		return 0, err
	}
	if err := e.lockable(ctx, c.Members, "acme"); err != nil {
		return 0, err
	}
	if err := e.Nodes.ACMEApply(ctx, *o, cfg, true); err != nil {
		return 0, fmt.Errorf("checked on %s: %v", o.Name, err)
	}
	if err := e.Nodes.ACMEApply(ctx, *o, cfg, false); err != nil {
		return 0, fmt.Errorf("%s: %v", o.Name, err)
	}
	if s := acme.Secrets(cfg); len(s) > 0 {
		e.Journal(id, actor, "letsencrypt", o.Name, 0, "Let's Encrypt's configuration given to %s, which orders, with its secrets (%s) - the others pull them from it", o.Name, strings.Join(s, ", "))
	} else {
		e.Journal(id, actor, "letsencrypt", o.Name, 0, "Let's Encrypt's configuration given to %s, which orders", o.Name)
	}
	base, err := e.Store.Version(id, c.Desired)
	if err != nil {
		return 0, err
	}
	if c.Manages("acme") && bytes.Equal(base.Content[ACMEFile], canonical) {
		// The cluster's version has it already: only secrets changed, if
		// anything - the others pull them; a member that doesn't run the
		// version - drifted, released, behind - takes it again.
		st := e.Store.State(id)
		for _, m := range c.Members {
			if ms := st.Members[m.Name]; ms.Applied != c.Desired || slices.Contains(ms.Drift, "acme") || slices.Contains(ms.Released, "acme") {
				return 0, e.Apply(ctx, id, c.Desired, nil, actor)
			}
		}
		return 0, e.Pull(ctx, id, nil, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CONFIG}, actor)
	}
	files := maps.Clone(base.Content)
	files[ACMEFile] = canonical
	if note == "" {
		note = "Let's Encrypt's configuration"
	}
	v, err := e.Store.NewVersion(id, actor, note, files)
	if err != nil {
		return 0, err
	}
	e.Journal(id, actor, "version", "", v.Version, "version %d made: %s", v.Version, note)
	if !c.Manages("acme") {
		if _, err := e.Store.Update(id, func(c *Cluster) error {
			if !c.Manages("acme") {
				c.Managed = append(c.Managed, "acme")
			}
			return nil
		}); err != nil {
			return v.Version, err
		}
		e.Journal(id, actor, "manage", "", v.Version, "the cluster manages Let's Encrypt's configuration from version %d on: the same on every member, locked there", v.Version)
	}
	return v.Version, e.Apply(ctx, id, v.Version, nil, actor)
}

// lockable: each of members' releases can lock domain - an older one
// refuses a composition naming it, and the apply would stop half done.
func (e *Engine) lockable(ctx context.Context, members []Member, domain string) error {
	for _, m := range members {
		info, err := e.Nodes.ClusterGet(ctx, m)
		if err != nil {
			return fmt.Errorf("%s: %v", m.Name, err)
		}
		if !slices.Contains(info.GetLockable(), domain) {
			return fmt.Errorf("%s's release can't lock %s - update it first (the cluster's members updated in place), or leave %s out of what the cluster manages", m.Name, domainWords(domain), domain)
		}
	}
	return nil
}

// domainWords is what a domain is, in words.
func domainWords(domain string) string {
	if domain == "acme" {
		return "Let's Encrypt's configuration"
	}
	return domain
}
