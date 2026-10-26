package clusters

import (
	"context"
	"fmt"
	"slices"
	"strings"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// Let's Encrypt in a cluster (docs/guide/clusters.md "Let's Encrypt"):
// one member orders - the one the others pull their secrets from -, the
// others order nothing (ClusterSetRequest.acme_orderer): they hold its
// configuration (pulled, CLUSTER_ITEM_ACME_CONFIG) for the day they become
// the orderer, serve its certificates (pulled, CLUSTER_ITEM_ACME_CERTS)
// and answer HTTP-01 challenges with its account's thumbprint - a
// challenge may reach any member. SyncACME keeps them in step: Reconcile
// calls it, so a renewal reaches every member within a check.

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
		if err := e.Pull(ctx, c.ID, []string{m.Name}, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CONFIG, janusv1alpha1.ClusterItem_CLUSTER_ITEM_ACME_CERTS}, actor); err != nil {
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
