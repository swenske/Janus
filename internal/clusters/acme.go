package clusters

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

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

// acmeBefore puts the version's Let's Encrypt configuration (r.ACMEConfig)
// on member m before its haproxy.cfg - nothing when m runs it already
// (its canonical form): the secrets pulled first from the member that
// orders - an empty secret keeps the one m holds -, and the certificates
// m has and the version doesn't kept, its haproxy.cfg perhaps still using
// them. final: the version's, to apply once haproxy.cfg is - nil when
// nothing was kept; applied: m's configuration changed.
func (e *Engine) acmeBefore(ctx context.Context, c *Cluster, m Member, r *Rendered) (final *janusv1alpha1.ACMEConfig, applied bool, err error) {
	want := r.ACMEConfig
	have, err := e.Nodes.ACMEConfig(ctx, m)
	if err != nil {
		return nil, false, fmt.Errorf("reading its Let's Encrypt configuration: %v", err)
	}
	if b, err := acme.Canonical(have); err == nil && bytes.Equal(b, r.ACME) {
		return nil, false, nil
	}
	if m.Name != c.SecretsFrom && c.Member(c.SecretsFrom) != nil {
		if _, err := e.Nodes.ClusterPull(ctx, m, c.SecretsFrom, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CONFIG}); err != nil {
			return nil, false, fmt.Errorf("pulling the Let's Encrypt secrets from %s: %v", c.SecretsFrom, err)
		}
		if have, err = e.Nodes.ACMEConfig(ctx, m); err != nil {
			return nil, false, fmt.Errorf("reading its Let's Encrypt configuration: %v", err)
		}
	}
	first := withKept(want, have)
	if err := e.Nodes.ACMEApply(ctx, m, first, "", false); err != nil {
		return nil, true, err // letsencrypt.json: ...
	}
	if len(first.GetCertificates()) == len(want.GetCertificates()) {
		return nil, true, nil
	}
	return want, true, nil
}

// ACMEOptions are ConfigureACME's choices.
type ACMEOptions struct {
	// Note: the new version's note.
	Note string
	// AccountKey: an existing account's private key (PEM), given to every
	// member - relayed, never kept -, so whichever orders uses that
	// account; empty: each keeps its own.
	AccountKey string
	// Base: the version the new one starts from - the one the members run
	// (Desired), or a newer one not applied yet, which then goes with it.
	// 0: Desired, unless a newer version waits - UnappliedError.
	Base int
}

// UnappliedError: a version newer than the one the members run waits,
// not applied, and the caller didn't say which one the cluster's Let's
// Encrypt configuration starts from.
type UnappliedError struct {
	Newest, Desired int
}

func (e *UnappliedError) Error() string {
	return fmt.Sprintf("version %d is newer than the one the members run (%d) and isn't applied: start from version %d - its changes applied with Let's Encrypt's -, or from version %d - it stays aside, not applied", e.Newest, e.Desired, e.Newest, e.Desired)
}

// ConfigureACME makes cfg cluster id's Let's Encrypt configuration: checked
// on the member that orders - the one the secrets come from -, given to
// it whole, its secrets with it (relayed, never kept); then the cluster's
// next version - from opts.Base -, cfg in it without its secrets, applied
// member by member, the cluster managing Let's Encrypt from then on - each
// member pulls the secrets from the orderer as it takes the version. When
// the cluster's version has cfg already, no new one: the others pull the
// secrets, a member that doesn't run it takes it again. An account key
// goes to every member. The version made, 0 if none.
func (e *Engine) ConfigureACME(ctx context.Context, id string, cfg *janusv1alpha1.ACMEConfig, opts ACMEOptions, actor string) (int, error) {
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
	newest := c.Desired
	if vs, err := e.Store.Versions(id); err == nil && len(vs) > 0 {
		newest = vs[0].Version
	}
	baseN := opts.Base
	switch {
	case baseN == 0 && newest > c.Desired:
		return 0, &UnappliedError{Newest: newest, Desired: c.Desired}
	case baseN == 0:
		baseN = c.Desired
	case baseN < c.Desired:
		return 0, fmt.Errorf("version %d is older than the one the members run (%d): start from %d, or a newer one", baseN, c.Desired, c.Desired)
	}
	base, err := e.Store.Version(id, baseN)
	if err != nil {
		return 0, fmt.Errorf("version %d: %w", baseN, err)
	}
	canonical, err := acme.Canonical(cfg)
	if err != nil {
		return 0, err
	}
	if err := e.lockable(ctx, c.Members, "acme"); err != nil {
		return 0, err
	}
	if err := e.Nodes.ACMEApply(ctx, *o, cfg, opts.AccountKey, true); err != nil {
		return 0, fmt.Errorf("checked on %s: %v", o.Name, err)
	}
	if err := e.Nodes.ACMEApply(ctx, *o, cfg, opts.AccountKey, false); err != nil {
		return 0, fmt.Errorf("%s: %v", o.Name, err)
	}
	secrets := acme.Secrets(cfg)
	if opts.AccountKey != "" {
		secrets = append(secrets, "the account key")
	}
	if len(secrets) > 0 {
		e.Journal(id, actor, "letsencrypt", o.Name, 0, "Let's Encrypt's configuration given to %s, which orders, with its secrets (%s) - the others pull them from it", o.Name, strings.Join(secrets, ", "))
	} else {
		e.Journal(id, actor, "letsencrypt", o.Name, 0, "Let's Encrypt's configuration given to %s, which orders", o.Name)
	}
	n, err := e.acmeVersion(ctx, c, base, canonical, opts.Note, actor)
	if err != nil || opts.AccountKey == "" {
		return n, err
	}
	// The account key, on the others too - not pulled: a member's own
	// key never leaves it. Then whoever orders answers with it, and the
	// others are told its thumbprint at once.
	want, err := acme.ParseConfig(canonical)
	if err != nil {
		return n, err
	}
	var failed []string
	for _, m := range c.Members {
		if m.Name == o.Name {
			continue
		}
		if err := e.Nodes.ACMEApply(ctx, m, want, opts.AccountKey, false); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", m.Name, err))
		}
	}
	if len(failed) > 0 {
		return n, fmt.Errorf("the account key not given to every member - %s", strings.Join(failed, "; "))
	}
	e.Journal(id, actor, "letsencrypt", "", n, "the account key given to every member: whichever orders uses that account")
	if cur, ok := e.Store.Get(id); ok {
		return n, e.SyncACME(ctx, cur, actor)
	}
	return n, nil
}

// acmeVersion makes canonical cluster c's Let's Encrypt configuration
// from version base: a new version when it differs, else - base being
// the one the members run - the secrets pulled again and a member that
// doesn't run it brought back.
func (e *Engine) acmeVersion(ctx context.Context, c *Cluster, base *Version, canonical []byte, note, actor string) (int, error) {
	id := c.ID
	if c.Manages("acme") && base.Version == c.Desired && bytes.Equal(base.Content[ACMEFile], canonical) {
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
	if base.Version != c.Desired {
		note += fmt.Sprintf(" (from version %d, not applied before)", base.Version)
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
	if err := e.Apply(ctx, id, v.Version, nil, actor); err != nil {
		return v.Version, err
	}
	// A member that ran the configuration already, its secrets aside,
	// was left alone by the apply: the secrets given to the orderer reach
	// it now.
	return v.Version, e.Pull(ctx, id, nil, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CONFIG}, actor)
}

// ACMEPathCheck is one member's answer to a way to the CA tried.
type ACMEPathCheck struct {
	Member string                               `json:"member"`
	Result *janusv1alpha1.ACMECheckPathResponse `json:"result,omitempty"`
	Error  string                               `json:"error,omitempty"`
}

// CheckACMEPath has every member of c fetch the CA's directory the way
// path says - nil: the way it's set - at once: a member's own secret
// (a proxy's password) where path leaves it empty.
func (e *Engine) CheckACMEPath(ctx context.Context, c *Cluster, path *janusv1alpha1.ACMEPath) []ACMEPathCheck {
	out := make([]ACMEPathCheck, len(c.Members))
	var wg sync.WaitGroup
	for i, m := range c.Members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.Nodes.ACMECheckPath(ctx, m, path)
			out[i] = ACMEPathCheck{Member: m.Name, Result: r}
			if err != nil {
				out[i].Error = err.Error()
			}
		}()
	}
	wg.Wait()
	return out
}

// RenewACME has the member that orders obtain names (every certificate
// if none) now; the others pull them at the next check.
func (e *Engine) RenewACME(ctx context.Context, c *Cluster, names []string, actor string) ([]string, error) {
	o := c.Member(c.SecretsFrom)
	if o == nil {
		return nil, errors.New("the cluster has no member the secrets come from: the one that orders")
	}
	got, err := e.Nodes.ACMERenew(ctx, *o, names)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", o.Name, err)
	}
	e.Journal(c.ID, actor, "letsencrypt", o.Name, 0, "a renewal asked of %s, which orders: %s - the others pull it at the next check", o.Name, ListOrNone(got))
	return got, nil
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
