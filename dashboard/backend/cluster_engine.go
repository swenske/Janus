package main

import (
	"context"
	"log"
	"sync"

	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/internal/clusters"
)

// The Controller's side of the cluster engine (internal/clusters, shared
// with janusctl cluster - docs/cluster.md sections 2-4): one operation at
// a time per cluster, in the background, its phase on the record and what
// it did in the journal; the reconcile loop (cluster_reconcile.go); and
// the members reached as the Controller's nodes (cluster_nodes.go),
// called as nodeproxy.ClusterUser for the cluster (nodeproxy.ForCluster):
// what lets the writes through their cluster locks.

type clusterEngine struct {
	*clusters.Engine
	a    *app
	kick chan struct{}

	mu   sync.Mutex
	busy map[string]bool
	wg   sync.WaitGroup
}

func newClusterEngine(a *app, st *clusters.Store) *clusterEngine {
	e := &clusterEngine{a: a, kick: make(chan struct{}, 1), busy: map[string]bool{}}
	e.Engine = &clusters.Engine{
		Store:     st,
		Nodes:     clusterNodes(a),
		Ready:     a.clusterReady,
		ManagedBy: "janus-controller " + clusters.ShortID(a.controllerID),
	}
	return e
}

// poke has the reconcile loop look at every cluster now.
func (e *clusterEngine) poke() {
	if e == nil {
		return
	}
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// start runs fn for cluster id in the background: false when an
// operation already runs there. phase names it on the record; its
// failure stays there and in the journal.
func (e *clusterEngine) start(id, phase, actor string, fn func(ctx context.Context) error) bool {
	e.mu.Lock()
	if e.busy[id] {
		e.mu.Unlock()
		return false
	}
	e.busy[id] = true
	e.mu.Unlock()
	_, _ = e.Store.Update(id, func(c *clusters.Cluster) error {
		c.Phase, c.Error = phase, ""
		return nil
	})
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		err := fn(nodeproxy.ForCluster(context.Background(), id))
		msg := ""
		if err != nil {
			msg = err.Error()
			log.Printf("cluster %s: %s failed: %v", id, phase, err)
			_ = e.Store.Journal(id, clusters.Entry{Actor: actor, Action: phase, Message: phase + " failed", Error: msg})
		}
		_, _ = e.Store.Update(id, func(c *clusters.Cluster) error {
			c.Phase, c.Error = "", msg
			return nil
		})
		e.mu.Lock()
		delete(e.busy, id)
		e.mu.Unlock()
		e.poke()
	}()
	return true
}

func (e *clusterEngine) isBusy(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.busy[id]
}

// wait waits for the operations under way (tests).
func (e *clusterEngine) wait() { e.wg.Wait() }
