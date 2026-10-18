package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/nodemetrics"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
)

// The node list's curves (GET /api/nodes/metrics): each node the caller
// sees, its last minutes of CPU, HAProxy requests and traffic, from one
// sampler (internal/nodemetrics) that reads a node while a page asks for
// it - each request renews its lease - and keeps what it read for a
// while after.
//
// The sampler reads as the Controller (Automation, like /api/nodes/
// status), once for every page: what it reads - SystemStat,
// NetworkDeviceStats, ShowInfo - is a reader's, in the observe domain
// every permission on a node includes, so whoever sees a node may read
// these figures on its page anyway. A node that doesn't trust the fleet
// yet is reached as admin: like its page, only an admin gets its curves.

type nodesMetrics struct {
	IntervalMs int64                         `json:"interval_ms"`
	NowMs      int64                         `json:"now_ms"`
	Nodes      map[string]nodemetrics.Series `json:"nodes"`
}

func newNodeMetrics(a *app) *nodemetrics.Sampler {
	return nodemetrics.New(nodemetrics.Options{Read: func(ctx context.Context, id string) (*nodemetrics.Counters, error) {
		n, ok := a.store.Get(id)
		if !ok {
			return nil, errors.New("removed")
		}
		return nodeproxy.ReadCounters(ctx, n)
	}})
}

func (a *app) handleNodesMetrics(w http.ResponseWriter, r *http.Request) {
	p := requestPrincipal(r)
	var ids []string
	for _, n := range a.visibleNodes(p) {
		if n.TrustsFleet() || p.Role == auth.Admin {
			ids = append(ids, n.ID)
		}
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	a.nodeMetrics.Watch(ids)
	out := nodesMetrics{NowMs: time.Now().UnixMilli(), Nodes: a.nodeMetrics.Snapshot(ids, since)}
	if a.nodeMetrics != nil {
		out.IntervalMs = a.nodeMetrics.Interval().Milliseconds()
	}
	writeJSON(w, http.StatusOK, out)
}
