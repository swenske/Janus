package clusters

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"time"
)

// A cluster seeded from a node (docs/guide/clusters.md "From a node"):
// the first of its new machines takes a node's state - its HAProxy
// configuration and history, its files, uploaded and Let's Encrypt
// certificates, its settings; not its identity nor its network - before
// the cluster is made from it, so the cluster starts where that node is,
// the node itself untouched but for one export it grants. The state goes
// from node to node over their own mutual TLS (StateImportFrom), never
// through the orchestrator, which only carries a signing request and a
// ten-minute certificate.

// SeedWait bounds the wait for the seeded node to come back into the
// state it took.
var SeedWait = 6 * time.Minute

// Seed has node to - unused, not a member yet - pull node from's state
// and restart into it, then waits until it serves from's HAProxy
// configuration. Journaled under cluster id.
func (e *Engine) Seed(ctx context.Context, id string, from, to Member, actor string) error {
	want, err := e.Nodes.HAProxyConfig(ctx, from)
	if err != nil {
		return fmt.Errorf("%s's configuration: %v", from.Name, err)
	}
	csr, err := e.Nodes.StateTransferRequest(ctx, to)
	if err != nil {
		return fmt.Errorf("%s, a key to pull %s's state with: %v", to.Name, from.Name, err)
	}
	cert, ca, err := e.Nodes.StateExportGrant(ctx, from, csr)
	if err != nil {
		return fmt.Errorf("%s granting %s one export of its state: %v", from.Name, to.Name, err)
	}
	addr := APIAddress(from)
	resp, err := e.Nodes.StateImportFrom(ctx, to, addr, ca, cert)
	if err != nil {
		return fmt.Errorf("%s pulling %s's state from %s: %v", to.Name, from.Name, addr, err)
	}
	e.Journal(id, actor, "seed", to.Name, 0, "%s took %s's state, node to node - %d file(s), without its identity nor its network - and restarts into it", to.Name, from.Name, len(resp.GetFiles()))
	for _, w := range resp.GetWarnings() {
		e.Journal(id, actor, "seed", to.Name, 0, "%s: %s", to.Name, w)
	}
	// Back, serving what from serves.
	sum := sha256.Sum256(want)
	deadline := time.Now().Add(SeedWait)
	for {
		got, err := e.Nodes.HAProxyConfig(ctx, to)
		if err == nil {
			if s := sha256.Sum256(got); bytes.Equal(s[:], sum[:]) {
				if err := e.Nodes.Healthy(ctx, to); err == nil {
					e.Journal(id, actor, "seed", to.Name, 0, "%s back, serving %s's configuration", to.Name, from.Name)
					return nil
				} else if time.Now().After(deadline) {
					return fmt.Errorf("%s runs %s's configuration, but HAProxy doesn't answer: %v - a bind on an address only %s has? (net.ipv4.ip_nonlocal_bind, or bind the cluster's virtual IP)", to.Name, from.Name, err, from.Name)
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s didn't come back with %s's configuration within %s", to.Name, from.Name, SeedWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
