package clusters

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// Reconciling (docs/cluster.md "Locks and drift"): each member is asked
// what it knows of its cluster, how its peers are, and whether what it
// runs is what the cluster applied. A difference is drift: reported,
// never overwritten - the next apply does that. Member certificates are
// renewed past two thirds of their life, the TLS ticket key ring rotated
// every TicketsEvery - renewed while a member that left still knows it.
// The Controller does it in a loop; janusctl cluster check, when asked.

// Reconcile looks at cluster c's members and records what it sees; again:
// one is in error.
func (e *Engine) Reconcile(ctx context.Context, c *Cluster) (again bool) {
	st := e.Store.State(c.ID)
	for _, m := range c.Members {
		prev := st.Members[m.Name]
		mctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ms := e.observe(mctx, c, m, prev)
		cancel()
		if ms.Error != "" {
			again = true
		}
		if d := newOnes(ms.Drift, prev.Drift); len(d) > 0 {
			e.Journal(c.ID, Actor, "drift", m.Name, ms.Applied, "%s no longer runs what version %d gave it: %s - the next apply puts it back", m.Name, ms.Applied, ListOrNone(d))
		}
		if r := newOnes(ms.Released, prev.Released); len(r) > 0 {
			e.Journal(c.ID, Actor, "released", m.Name, ms.Applied, "%s's users took back %s", m.Name, ListOrNone(r))
		}
		_ = e.Store.UpdateState(c.ID, func(s *State) { s.Members[m.Name] = ms })
	}
	if c.Balancer != nil {
		if cur, ok := e.Store.Get(c.ID); ok {
			if err := e.reconcileBalancer(ctx, cur); err != nil {
				again = true
			}
		}
	}
	every := e.TicketsEvery
	if every <= 0 {
		every = 24 * time.Hour
	}
	if len(e.Store.State(c.ID).TicketsRenew) > 0 {
		if err := e.RenewTickets(ctx, c, Actor); err != nil {
			log.Printf("cluster %s: TLS ticket keys: %v", c.Name, err)
			again = true
		}
	} else if len(c.Members) > 1 && !again && time.Since(st.TicketsRotated) > every {
		if err := e.RotateTickets(ctx, c); err != nil {
			log.Printf("cluster %s: TLS ticket keys: %v", c.Name, err)
			again = true
		} else {
			_ = e.Store.UpdateState(c.ID, func(s *State) { s.TicketsRotated = time.Now().UTC() })
		}
	}
	return again
}

// observe is member m as it is now; prev, as it was.
func (e *Engine) observe(ctx context.Context, c *Cluster, m Member, prev MemberState) MemberState {
	ms := prev
	ms.Checked, ms.Error, ms.Drift = time.Now().UTC(), "", nil
	info, err := e.Nodes.ClusterGet(ctx, m)
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
	if status, err := e.Nodes.ClusterStatus(ctx, m); err == nil {
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
		drift, err := e.Drift(ctx, c, m, prev)
		if err != nil {
			ms.Error = err.Error()
		}
		ms.Drift = drift
	}
	if info.GetMemberCertNotAfterUnix() == 0 {
		return ms
	}
	if left := time.Until(ms.CertNotAfter); left < MemberValidity/3 {
		if err := e.Renew(ctx, c, m); err != nil {
			ms.Error = "renewing its member certificate: " + err.Error()
		} else {
			e.Journal(c.ID, Actor, "renew", m.Name, 0, "%s's member certificate renewed (%s left)", m.Name, left.Round(time.Hour))
		}
	}
	return ms
}

// Drift is the managed domains whose content on member m isn't what was
// applied to it.
func (e *Engine) Drift(ctx context.Context, c *Cluster, m Member, ms MemberState) ([]string, error) {
	var out []string
	for _, d := range Domains {
		want, ok := ms.Hashes[d]
		if !ok || !c.Manages(d) {
			continue
		}
		var have string
		switch d {
		case "haproxy":
			cfg, err := e.Nodes.HAProxyConfig(ctx, m)
			if err != nil {
				return out, err
			}
			have = Sum(cfg)
		case "files":
			v, err := e.Store.Version(c.ID, ms.Applied)
			if err != nil {
				return out, err
			}
			list, err := e.Nodes.Files(ctx, m)
			if err != nil {
				return out, err
			}
			on := map[string]string{}
			for _, f := range list {
				on[f.GetName()] = f.GetSha256()
			}
			sums := map[string]string{}
			for p := range v.Content {
				if name, ok := strings.CutPrefix(p, "files/"); ok && name != "" {
					sums[name] = on[name]
				}
			}
			have = FilesHashOfSums(sums)
		default:
			cfg, err := e.Nodes.ModuleConfig(ctx, m, d)
			if err != nil {
				return out, err
			}
			have = Sum(cfg)
		}
		if have != want {
			out = append(out, d)
		}
	}
	return out, nil
}

// Renew gives member m a new member certificate, for a new key.
func (e *Engine) Renew(ctx context.Context, c *Cluster, m Member) error {
	ca, err := e.Store.CA(c.ID)
	if err != nil {
		return err
	}
	csr, err := e.Nodes.CertificateRequest(ctx, m)
	if err != nil {
		return err
	}
	cert, err := SignMember(ca, csr, c.ID, m.Name)
	if err != nil {
		return err
	}
	_, err = e.Nodes.ClusterSet(ctx, m, e.SetRequest(c, m, nil, cert))
	return err
}

// RotateTickets adds a key to the ring on the member the secrets come
// from, and has the others pull it.
func (e *Engine) RotateTickets(ctx context.Context, c *Cluster) error {
	m := c.Member(c.SecretsFrom)
	if m == nil {
		return fmt.Errorf("no member %s", c.SecretsFrom)
	}
	if err := e.Nodes.RotateTickets(ctx, *m, false); err != nil {
		return err
	}
	e.Journal(c.ID, Actor, "tickets", m.Name, 0, "TLS ticket key ring rotated on %s", m.Name)
	return e.Pull(ctx, c.ID, nil, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_TLS_TICKET_KEYS}, Actor)
}

// RenewTickets replaces every key of the ring on the member the secrets
// come from and has the others pull it: the members State.TicketsRenew
// names left knowing the ring - they must neither read the tickets the
// cluster issues from now on nor forge one it accepts. Each client makes
// one full handshake. Done, the names go.
func (e *Engine) RenewTickets(ctx context.Context, c *Cluster, actor string) error {
	left := e.Store.State(c.ID).TicketsRenew
	m := c.Member(c.SecretsFrom)
	if m == nil {
		return fmt.Errorf("no member %s", c.SecretsFrom)
	}
	if err := e.Nodes.RotateTickets(ctx, *m, true); err != nil {
		return fmt.Errorf("renew the TLS ticket key ring on %s: %w", m.Name, err)
	}
	if err := e.Pull(ctx, c.ID, nil, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_TLS_TICKET_KEYS}, actor); err != nil {
		return fmt.Errorf("the TLS ticket key ring renewed on %s, not pulled by every member: %w", m.Name, err)
	}
	if len(left) > 0 {
		e.Journal(c.ID, actor, "tickets", m.Name, 0, "TLS ticket key ring renewed on %s and pulled by the others: no key %s knew is used or accepted any more", m.Name, strings.Join(left, ", "))
	} else {
		e.Journal(c.ID, actor, "tickets", m.Name, 0, "TLS ticket key ring renewed on %s and pulled by the others: every key replaced", m.Name)
	}
	return e.Store.UpdateState(c.ID, func(s *State) {
		s.TicketsRenew = slices.DeleteFunc(s.TicketsRenew, func(n string) bool { return slices.Contains(left, n) })
		s.TicketsRotated = time.Now().UTC()
	})
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
