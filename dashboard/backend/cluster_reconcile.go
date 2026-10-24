package main

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/clusters"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// The reconcile loop (docs/cluster.md "Locks and drift"): every 10
// minutes - every minute while a member is in error, at once when poked -
// each member is asked what it knows of its cluster, how its peers are,
// and whether what it runs is what the cluster applied. A difference is
// drift: reported, never overwritten - the next apply does that. Member
// certificates are renewed past two thirds of their life, the TLS ticket
// key ring rotated every day.

// Vars: the tests change them.
var (
	clusterEvery = 10 * time.Minute
	clusterRetry = time.Minute
	ticketsEvery = 24 * time.Hour
)

const clusterActor = "cluster"

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
	for _, c := range e.store.List() {
		if e.isBusy(c.ID) {
			continue
		}
		if e.reconcile(ctx, c) {
			again = true
		}
	}
	return again
}

// reconcile looks at cluster c's members; again: one is in error.
func (e *clusterEngine) reconcile(ctx context.Context, c *clusters.Cluster) (again bool) {
	cctx := nodeproxy.ForCluster(ctx, c.ID)
	st := e.store.State(c.ID)
	for _, m := range c.Members {
		prev := st.Members[m.Name]
		mctx, cancel := context.WithTimeout(cctx, 30*time.Second)
		ms := e.observe(mctx, c, m, prev)
		cancel()
		if ms.Error != "" {
			again = true
		}
		if d := newOnes(ms.Drift, prev.Drift); len(d) > 0 {
			e.journal(c.ID, clusterActor, "drift", m.Name, ms.Applied, "%s no longer runs what version %d gave it: %s - the next apply puts it back", m.Name, ms.Applied, listOrNone(d))
		}
		if r := newOnes(ms.Released, prev.Released); len(r) > 0 {
			e.journal(c.ID, clusterActor, "released", m.Name, ms.Applied, "%s's users took back %s", m.Name, listOrNone(r))
		}
		_ = e.store.UpdateState(c.ID, func(s *clusters.State) { s.Members[m.Name] = ms })
	}
	if len(c.Members) > 1 && !again && time.Since(st.TicketsRotated) > ticketsEvery {
		if err := e.rotateTickets(cctx, c); err != nil {
			log.Printf("cluster %s: TLS ticket keys: %v", c.Name, err)
			again = true
		} else {
			_ = e.store.UpdateState(c.ID, func(s *clusters.State) { s.TicketsRotated = time.Now().UTC() })
		}
	}
	return again
}

// observe is member m as it is now; prev, as it was.
func (e *clusterEngine) observe(ctx context.Context, c *clusters.Cluster, m clusters.Member, prev clusters.MemberState) clusters.MemberState {
	ms := prev
	ms.Checked, ms.Error, ms.Drift = time.Now().UTC(), "", nil
	n, err := e.node(m)
	if err != nil {
		ms.Error = err.Error()
		return ms
	}
	info, err := clusterNodes.ClusterGet(ctx, n)
	if err != nil {
		ms.Error = err.Error()
		return ms
	}
	if !info.GetMember() || info.GetId() != c.ID {
		ms.Error = "the node is no longer a member of the cluster"
		return ms
	}
	ms.Released = nil
	for _, d := range c.Managed {
		if !slices.Contains(info.GetManaged(), d) {
			ms.Released = append(ms.Released, d)
		}
	}
	ms.Drained = info.GetDrained()
	ms.CertNotAfter = time.Unix(info.GetMemberCertNotAfterUnix(), 0).UTC()
	if status, err := clusterNodes.ClusterStatus(ctx, n); err == nil {
		ms.PeersUp, ms.PeersTotal, ms.TablesSynced = 0, 0, true
		for _, p := range status.GetPeers() {
			if p.GetLocal() {
				continue
			}
			ms.PeersTotal++
			if p.GetUp() {
				ms.PeersUp++
			}
			for _, t := range p.GetTables() {
				if !t.GetSynced() {
					ms.TablesSynced = false
				}
			}
		}
	} else {
		ms.Error = "peers: " + err.Error()
	}
	if prev.Applied > 0 {
		drift, err := e.drift(ctx, c, n, prev)
		if err != nil {
			ms.Error = err.Error()
		}
		ms.Drift = drift
	}
	if !info.GetMember() || info.GetMemberCertNotAfterUnix() == 0 {
		return ms
	}
	if left := time.Until(ms.CertNotAfter); left < clusters.MemberValidity/3 {
		if err := e.renew(ctx, c, m); err != nil {
			ms.Error = "renewing its member certificate: " + err.Error()
		} else {
			e.journal(c.ID, clusterActor, "renew", m.Name, 0, "%s's member certificate renewed (%s left)", m.Name, left.Round(time.Hour))
		}
	}
	return ms
}

// drift is the managed domains whose content on member n isn't what was
// applied to it.
func (e *clusterEngine) drift(ctx context.Context, c *clusters.Cluster, n *store.Node, ms clusters.MemberState) ([]string, error) {
	var out []string
	for _, d := range clusters.Domains {
		want, ok := ms.Hashes[d]
		if !ok || !c.Manages(d) {
			continue
		}
		var have string
		switch d {
		case "haproxy":
			cfg, err := clusterNodes.HAProxyConfig(ctx, n)
			if err != nil {
				return out, err
			}
			have = clusters.Sum(cfg)
		case "files":
			v, err := e.store.Version(c.ID, ms.Applied)
			if err != nil {
				return out, err
			}
			list, err := clusterNodes.Files(ctx, n)
			if err != nil {
				return out, err
			}
			on := map[string]string{}
			for _, f := range list {
				on[f.GetName()] = f.GetSha256()
			}
			sums := map[string]string{}
			for p := range v.Content {
				if name, ok := cutFiles(p); ok {
					sums[name] = on[name]
				}
			}
			have = clusters.FilesHashOfSums(sums)
		default:
			cfg, err := clusterNodes.ModuleConfig(ctx, n, d)
			if err != nil {
				return out, err
			}
			have = clusters.Sum(cfg)
		}
		if have != want {
			out = append(out, d)
		}
	}
	return out, nil
}

func cutFiles(p string) (string, bool) {
	const prefix = "files/"
	if len(p) > len(prefix) && p[:len(prefix)] == prefix {
		return p[len(prefix):], true
	}
	return "", false
}

// renew gives member m a new member certificate, for a new key.
func (e *clusterEngine) renew(ctx context.Context, c *clusters.Cluster, m clusters.Member) error {
	n, err := e.node(m)
	if err != nil {
		return err
	}
	ca, err := e.store.CA(c.ID)
	if err != nil {
		return err
	}
	csr, err := clusterNodes.CertificateRequest(ctx, n)
	if err != nil {
		return err
	}
	cert, err := clusters.SignMember(ca, csr, c.ID, m.Name)
	if err != nil {
		return err
	}
	_, err = clusterNodes.ClusterSet(ctx, n, e.setRequest(c, m, nil, cert))
	return err
}

// rotateTickets adds a key to the ring on the member the secrets come
// from, and has the others pull it.
func (e *clusterEngine) rotateTickets(ctx context.Context, c *clusters.Cluster) error {
	m := c.Member(c.SecretsFrom)
	if m == nil {
		return fmt.Errorf("no member %s", c.SecretsFrom)
	}
	n, err := e.node(*m)
	if err != nil {
		return err
	}
	if err := clusterNodes.RotateTickets(ctx, n); err != nil {
		return err
	}
	e.journal(c.ID, clusterActor, "tickets", m.Name, 0, "TLS ticket key ring rotated on %s", m.Name)
	return e.pull(ctx, c.ID, nil, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_TLS_TICKET_KEYS}, clusterActor)
}

// newOnes are what's in now and wasn't in before.
func newOnes(now, before []string) []string {
	var out []string
	for _, x := range now {
		if !slices.Contains(before, x) {
			out = append(out, x)
		}
	}
	return out
}
