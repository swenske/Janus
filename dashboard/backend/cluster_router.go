package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/clusters"
)

// The router in front as a cluster's RouterOS hook reads it
// (internal/clusters RouterRoles): each member's role per service prefix
// - multipath, master, backup, absent -, on the cluster's page and on
// each member's (nodeproxy.ClusterRouter), next to what the node itself
// knows of its BGP announcements. A router may not be readable: then the
// pages show the node's side alone.

// routerViewTTL: the members' pages ask every 15 s - the router is read
// at most this often per cluster.
const routerViewTTL = 10 * time.Second

// routerViews caches each cluster's last reading.
type routerViews struct {
	mu sync.Mutex
	m  map[string]routerReading
}

type routerReading struct {
	at   time.Time
	key  string // the hook's settings it was read with
	view *clusters.RouterView
	err  error
}

// routerView is cluster c's router as its RouterOS hook reads it, at most
// routerViewTTL old; nil, nil without such a hook.
func (a *app) routerView(ctx context.Context, c *clusters.Cluster) (*clusters.RouterView, error) {
	b := c.Balancer
	if !b.IsRouterOS() {
		return nil, nil
	}
	key := b.URL + " " + b.RoutingTable + " " + strings.Join(b.Prefixes, ",")
	a.routers.mu.Lock()
	if a.routers.m == nil {
		a.routers.m = map[string]routerReading{}
	}
	r, ok := a.routers.m[c.ID]
	a.routers.mu.Unlock()
	if ok && r.key == key && time.Since(r.at) < routerViewTTL {
		return r.view, r.err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	view, err := a.clusters.RouterRoles(ctx, c)
	a.routers.mu.Lock()
	a.routers.m[c.ID] = routerReading{at: time.Now(), key: key, view: view, err: err}
	a.routers.mu.Unlock()
	return view, err
}

// handleClusterRouter is GET /api/clusters/{id}/router: the cluster's
// router as its RouterOS hook reads it - available false without one, an
// error when the router doesn't answer.
func (a *app) handleClusterRouter(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	out := map[string]any{"available": c.Balancer.IsRouterOS()}
	if c.Balancer.IsRouterOS() {
		out["read_only"] = c.Balancer.ReadOnly
		view, err := a.routerView(r.Context(), c)
		if err != nil {
			out["error"] = err.Error()
		} else {
			out["read_at"], out["members"], out["unknown"] = view.ReadAt, view.Members, view.Unknown
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// memberRouterView is node n's roles on its cluster's router, for its
// page: not available when it is no member, or its cluster has no
// RouterOS hook.
func (a *app) memberRouterView(ctx context.Context, n *store.Node) nodeproxy.RouterRoles {
	for _, c := range a.clusters.Store.List() {
		for _, m := range c.Members {
			if m.NodeID != n.ID {
				continue
			}
			if !c.Balancer.IsRouterOS() {
				return nodeproxy.RouterRoles{}
			}
			view, err := a.routerView(ctx, c)
			if err != nil {
				return nodeproxy.RouterRoles{Error: err.Error()}
			}
			at := view.ReadAt
			return nodeproxy.RouterRoles{Available: true, ReadAt: &at, Roles: view.Members[m.Name]}
		}
	}
	return nodeproxy.RouterRoles{}
}
