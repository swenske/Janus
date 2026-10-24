package main

import (
	"context"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/internal/clusters"
)

// The reconcile loop (docs/cluster.md "Locks and drift", the engine's
// Reconcile): every 10 minutes - every minute while a member is in error,
// at once when poked - each cluster's members are looked at; drift is
// reported, never overwritten, member certificates renewed, the TLS ticket
// key ring rotated every day.

// Vars: the tests change them.
var (
	clusterEvery = 10 * time.Minute
	clusterRetry = time.Minute
)

func (e *clusterEngine) loop(ctx context.Context) {
	for {
		wait := clusterEvery
		if e.reconcileAll(ctx) {
			wait = clusterRetry
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-e.kick:
			t.Stop()
		}
	}
}

func (e *clusterEngine) reconcileAll(ctx context.Context) (again bool) {
	for _, c := range e.Store.List() {
		if e.isBusy(c.ID) {
			continue
		}
		if e.reconcile(ctx, c) {
			again = true
		}
	}
	return again
}

// reconcile looks at cluster c's members, as the Controller acting for it;
// again: one is in error.
func (e *clusterEngine) reconcile(ctx context.Context, c *clusters.Cluster) (again bool) {
	return e.Reconcile(nodeproxy.ForCluster(ctx, c.ID), c)
}
