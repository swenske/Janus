package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/clusters"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// The cluster engine (docs/cluster.md sections 2-4): the Controller
// creates a cluster from its nodes, keeps its desired configuration in
// versions and applies them member by member, adds and removes members,
// and has the members pull their secrets from one another - never through
// the Controller. One operation at a time per cluster, in the background;
// its phase on the record, what it did in the journal. Members are
// called as nodeproxy.ClusterUser, for the cluster (nodeproxy.ForCluster):
// what lets the writes through their cluster locks.

type clusterEngine struct {
	a     *app
	store *clusters.Store
	kick  chan struct{}

	mu   sync.Mutex
	busy map[string]bool
	wg   sync.WaitGroup
}

func newClusterEngine(a *app, st *clusters.Store) *clusterEngine {
	return &clusterEngine{a: a, store: st, kick: make(chan struct{}, 1), busy: map[string]bool{}}
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
	_, _ = e.store.Update(id, func(c *clusters.Cluster) error {
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
			_ = e.store.Journal(id, clusters.Entry{Actor: actor, Action: phase, Message: phase + " failed", Error: msg})
		}
		_, _ = e.store.Update(id, func(c *clusters.Cluster) error {
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

func (e *clusterEngine) journal(id, actor, action, member string, version int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if err := e.store.Journal(id, clusters.Entry{Actor: actor, Action: action, Member: member, Version: version, Message: msg}); err != nil {
		log.Printf("cluster %s: journal: %v", id, err)
	}
}

func (e *clusterEngine) node(m clusters.Member) (*store.Node, error) {
	n, ok := e.a.store.Get(m.NodeID)
	if !ok {
		return nil, fmt.Errorf("member %s: node %s is no longer one of the Controller's", m.Name, m.NodeID)
	}
	return n, nil
}

// apiAddress is where the other members reach m's API.
func apiAddress(m clusters.Member) string {
	if m.API != "" {
		return m.API
	}
	return net.JoinHostPort(m.Address, "9505")
}

// setRequest is ClusterSet for member m of c: the whole composition, and
// the CA and member certificate when given.
func (e *clusterEngine) setRequest(c *clusters.Cluster, m clusters.Member, caPEM, certPEM []byte) *janusv1alpha1.ClusterSetRequest {
	req := &janusv1alpha1.ClusterSetRequest{
		Id: c.ID, Name: c.Name, MemberName: m.Name, PeersPort: uint32(c.PeersPort),
		CaCert: caPEM, MemberCert: certPEM, Managed: c.Managed,
		ManagedBy: fmt.Sprintf("janus-controller %s cluster %s", shortID(e.a.controllerID), c.Name),
	}
	for _, o := range c.Members {
		req.Members = append(req.Members, &janusv1alpha1.ClusterMember{Name: o.Name, Address: o.Address, ApiAddress: o.API})
	}
	return req
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// check is what a node must be to join c (docs/cluster.md "Checks"): one
// of the Controller's, trusting its fleet - the lock's way through -,
// answering, its HAProxy healthy, in no other cluster, on c's image, and
// reaching the other members' API as they reach its.
func (e *clusterEngine) check(ctx context.Context, c *clusters.Cluster, joining []clusters.Member) error {
	schematic := ""
	for _, m := range c.Members {
		if slices.ContainsFunc(joining, func(j clusters.Member) bool { return j.Name == m.Name }) {
			continue
		}
		n, err := e.node(m)
		if err != nil {
			return err
		}
		v, err := clusterNodes.Version(ctx, n)
		if err != nil {
			return fmt.Errorf("member %s: %v", m.Name, err)
		}
		schematic = v.GetSchematicId()
		break
	}
	for _, m := range joining {
		n, err := e.node(m)
		if err != nil {
			return err
		}
		if !n.TrustsFleet() {
			return fmt.Errorf("%s doesn't trust the Controller's fleet yet: a cluster's member is managed through it (Fleet, then wait for the node to take the bundle)", n.Name)
		}
		v, err := clusterNodes.Version(ctx, n)
		if err != nil {
			return fmt.Errorf("%s: %v", n.Name, err)
		}
		switch {
		case schematic == "":
			schematic = v.GetSchematicId()
		case v.GetSchematicId() != schematic:
			return fmt.Errorf("%s runs another image (schematic %s, the cluster's %s): a cluster's members share their extensions, HAProxy branch and kernel", n.Name, shortID(v.GetSchematicId()), shortID(schematic))
		}
		if err := clusterNodes.Healthy(ctx, n); err != nil {
			return fmt.Errorf("%s's HAProxy doesn't answer: %v", n.Name, err)
		}
		info, err := clusterNodes.ClusterGet(ctx, n)
		if err != nil {
			return fmt.Errorf("%s: %v", n.Name, err)
		}
		if info.GetMember() && info.GetId() != c.ID {
			return fmt.Errorf("%s is a member of cluster %s already: it leaves it first", n.Name, info.GetName())
		}
	}
	// Each joining member and every other reach each other's API: what a
	// pull goes over.
	for _, m := range joining {
		for _, o := range c.Members {
			if o.Name == m.Name {
				continue
			}
			for _, p := range [][2]clusters.Member{{m, o}, {o, m}} {
				n, err := e.node(p[0])
				if err != nil {
					return err
				}
				if err := clusterNodes.Probe(ctx, n, apiAddress(p[1])); err != nil {
					return fmt.Errorf("%s doesn't reach %s's API at %s: %v - members reach each other on port 9505, and on the peers port %d", p[0].Name, p[1].Name, apiAddress(p[1]), err, c.PeersPort)
				}
			}
		}
	}
	return nil
}

// join gives members of c their member certificate and the composition;
// the others get the composition.
func (e *clusterEngine) join(ctx context.Context, c *clusters.Cluster, joining []string, actor string) error {
	ca, err := e.store.CA(c.ID)
	if err != nil {
		return err
	}
	certs := map[string][]byte{}
	for _, name := range joining {
		m := c.Member(name)
		n, err := e.node(*m)
		if err != nil {
			return err
		}
		csr, err := clusterNodes.CertificateRequest(ctx, n)
		if err != nil {
			return fmt.Errorf("%s: a signing request: %v", name, err)
		}
		if certs[name], err = clusters.SignMember(ca, csr, c.ID, name); err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
	}
	for _, m := range c.Members {
		n, err := e.node(m)
		if err != nil {
			return err
		}
		var caPEM, cert []byte
		if certs[m.Name] != nil {
			caPEM, cert = ca.CertPEM, certs[m.Name]
		}
		if _, err := clusterNodes.ClusterSet(ctx, n, e.setRequest(c, m, caPEM, cert)); err != nil {
			return fmt.Errorf("%s: ClusterSet: %v", m.Name, err)
		}
		if cert != nil {
			e.journal(c.ID, actor, "join", m.Name, 0, "%s joined: member certificate signed, composition of %d members", m.Name, len(c.Members))
		}
	}
	return nil
}

// snapshot makes c's first version from member source's configuration:
// haproxy.cfg and the files that hold no key - those stay on source, and
// the others pull them.
func (e *clusterEngine) snapshot(ctx context.Context, c *clusters.Cluster, source, actor string) (int, error) {
	m := c.Member(source)
	n, err := e.node(*m)
	if err != nil {
		return 0, err
	}
	files := map[string][]byte{}
	cfg, err := clusterNodes.HAProxyConfig(ctx, n)
	if err != nil {
		return 0, fmt.Errorf("%s's haproxy.cfg: %v", source, err)
	}
	files["haproxy.cfg"] = cfg
	list, err := clusterNodes.Files(ctx, n)
	if err != nil {
		return 0, fmt.Errorf("%s's files: %v", source, err)
	}
	keys := 0
	for _, f := range list {
		if f.GetSecret() {
			keys++
			continue
		}
		data, err := clusterNodes.FileGet(ctx, n, f.GetName())
		if err != nil {
			return 0, fmt.Errorf("%s's file %s: %v", source, f.GetName(), err)
		}
		files["files/"+f.GetName()] = data
	}
	note := "taken from " + source
	if keys > 0 {
		note += fmt.Sprintf("; %d file(s) holding a key stay there, pulled by the members", keys)
	}
	v, err := e.store.NewVersion(c.ID, actor, note, files)
	if err != nil {
		return 0, err
	}
	e.journal(c.ID, actor, "version", source, v.Version, "version %d %s", v.Version, note)
	return v.Version, nil
}

// create brings cluster id's members together: checks, certificates,
// the composition, the first version from source, then the secrets and
// that version on the others.
func (e *clusterEngine) create(ctx context.Context, id, source, actor string) error {
	c, ok := e.store.Get(id)
	if !ok {
		return clusters.ErrNotFound
	}
	if err := e.check(ctx, c, c.Members); err != nil {
		return err
	}
	var names []string
	for _, m := range c.Members {
		names = append(names, m.Name)
	}
	if err := e.join(ctx, c, names, actor); err != nil {
		return err
	}
	n, err := e.snapshot(ctx, c, source, actor)
	if err != nil {
		return err
	}
	return e.secretsAndVersion(ctx, id, n, nil, actor)
}

// secretsAndVersion brings only's members - nil: every one - the secrets
// and version n, in the order they need them: the secrets first, as the
// version may name a file holding a key that only a pull brings; then
// the version; then the certificate store again, for a certificate
// bound into a crt-list only the version brings (refused by the first
// pull, journaled; one already bound stays as it is).
func (e *clusterEngine) secretsAndVersion(ctx context.Context, id string, n int, only []string, actor string) error {
	if err := e.pull(ctx, id, only, nil, actor); err != nil {
		return err
	}
	if n > 0 {
		if err := e.apply(ctx, id, n, only, actor); err != nil {
			return err
		}
	}
	return e.pull(ctx, id, only, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_RUNTIME_CERTS}, actor)
}

// apply puts version n on cluster id's members - only's, or every
// member's -, one at a time (docs/cluster.md "Applying"): rendered for
// each first, checked on the first member, then on each member its
// files, HAProxy, the obsolete files gone, the firewall, VRRP, BGP. The
// first failure stops it: the members before run version n, the others
// what they ran.
func (e *clusterEngine) apply(ctx context.Context, id string, n int, only []string, actor string) error {
	c, ok := e.store.Get(id)
	if !ok {
		return clusters.ErrNotFound
	}
	v, err := e.store.Version(id, n)
	if err != nil {
		return err
	}
	if c.Manages("haproxy") && v.Content["haproxy.cfg"] == nil {
		return fmt.Errorf("version %d has no haproxy.cfg, and the cluster manages HAProxy's configuration", n)
	}
	modules := []string{"firewall", "vrrp", "bgp"}
	rendered := map[string]*clusters.Rendered{}
	for _, m := range c.Members {
		r, err := clusters.Render(v, c, m.Name)
		if err != nil {
			return err
		}
		for _, d := range modules {
			if c.Manages(d) && r.Modules[d] == nil {
				return fmt.Errorf("version %d configures no %s, and the cluster manages it", n, d)
			}
		}
		rendered[m.Name] = r
	}
	targets := c.Members
	if only != nil {
		targets = slices.DeleteFunc(slices.Clone(c.Members), func(m clusters.Member) bool { return !slices.Contains(only, m.Name) })
	}
	if len(targets) == 0 {
		return nil
	}
	// Checked on the first member before any is touched.
	first := targets[0]
	fn, err := e.node(first)
	if err != nil {
		return err
	}
	r := rendered[first.Name]
	if c.Manages("haproxy") {
		if err := clusterNodes.HAProxyValidate(ctx, fn, r.HAProxy); err != nil {
			return fmt.Errorf("version %d, checked on %s: %v", n, first.Name, err)
		}
	}
	for _, d := range modules {
		if c.Manages(d) {
			if err := clusterNodes.ModuleApply(ctx, fn, d, r.Modules[d], true); err != nil {
				return fmt.Errorf("version %d's %s, checked on %s: %v", n, d, first.Name, err)
			}
		}
	}
	st := e.store.State(id)
	for i, m := range targets {
		if err := e.applyMember(ctx, c, v, m, rendered[m.Name], st.Members[m.Name], actor); err != nil {
			_ = e.store.UpdateState(id, func(s *clusters.State) {
				ms := s.Members[m.Name]
				ms.Error = err.Error()
				s.Members[m.Name] = ms
			})
			done := make([]string, 0, i)
			for _, p := range targets[:i] {
				done = append(done, p.Name)
			}
			return fmt.Errorf("partially applied - %s: %v (version %d on %s, the others as they were)", m.Name, err, n, listOrNone(done))
		}
		hashes := map[string]string{}
		for d, h := range rendered[m.Name].Hashes() {
			if c.Manages(d) {
				hashes[d] = h
			}
		}
		_ = e.store.UpdateState(id, func(s *clusters.State) {
			ms := s.Members[m.Name]
			ms.Applied, ms.Hashes, ms.Drift, ms.Released, ms.Error = n, hashes, nil, nil, ""
			s.Members[m.Name] = ms
		})
		e.journal(id, actor, "apply", m.Name, n, "version %d applied on %s", n, m.Name)
	}
	if only == nil {
		_, err = e.store.Update(id, func(c *clusters.Cluster) error {
			c.Desired = n
			return nil
		})
	}
	return err
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// applyMember puts version v, rendered as r, on member m.
func (e *clusterEngine) applyMember(ctx context.Context, c *clusters.Cluster, v *clusters.Version, m clusters.Member, r *clusters.Rendered, prev clusters.MemberState, actor string) error {
	n, err := e.node(m)
	if err != nil {
		return err
	}
	if len(prev.Drift) > 0 || len(prev.Released) > 0 {
		e.journal(c.ID, actor, "drift", m.Name, v.Version, "%s had drifted (%s) or been released (%s): version %d replaces it", m.Name, listOrNone(prev.Drift), listOrNone(prev.Released), v.Version)
	}
	// The locks back first: a domain released on the member is the
	// cluster's again.
	if _, err := clusterNodes.ClusterSet(ctx, n, e.setRequest(c, m, nil, nil)); err != nil {
		return fmt.Errorf("ClusterSet: %v", err)
	}
	var obsolete []string
	if c.Manages("files") {
		have := map[string]string{}
		list, err := clusterNodes.Files(ctx, n)
		if err != nil {
			return err
		}
		for _, f := range list {
			have[f.GetName()] = f.GetSha256()
		}
		for name, data := range r.Files {
			if have[name] == clusters.Sum(data) {
				continue
			}
			if err := clusterNodes.FilePut(ctx, n, name, data); err != nil {
				return fmt.Errorf("file %s: %v", name, err)
			}
		}
		// The files the version it ran had, and this one hasn't: gone once
		// HAProxy no longer uses them.
		if pv, err := e.store.Version(c.ID, prev.Applied); err == nil {
			for p := range pv.Content {
				name, ok := strings.CutPrefix(p, "files/")
				if _, still := r.Files[name]; ok && !still {
					if _, there := have[name]; there {
						obsolete = append(obsolete, name)
					}
				}
			}
		}
	}
	if c.Manages("haproxy") {
		if err := clusterNodes.HAProxyApply(ctx, n, r.HAProxy); err != nil {
			return err
		}
	}
	for _, name := range obsolete {
		if err := clusterNodes.FileDelete(ctx, n, name); err != nil {
			return fmt.Errorf("file %s, gone from the version: %v", name, err)
		}
	}
	for _, d := range []string{"firewall", "vrrp", "bgp"} {
		if c.Manages(d) {
			if err := clusterNodes.ModuleApply(ctx, n, d, r.Modules[d], false); err != nil {
				return fmt.Errorf("%s: %v", d, err)
			}
		}
	}
	return nil
}

// pull has cluster id's members - only's, or every one - pull items
// (none: every kind) from the member the secrets come from.
func (e *clusterEngine) pull(ctx context.Context, id string, only []string, items []janusv1alpha1.ClusterItem, actor string) error {
	c, ok := e.store.Get(id)
	if !ok {
		return clusters.ErrNotFound
	}
	var failed []string
	for _, m := range c.Members {
		if m.Name == c.SecretsFrom || (only != nil && !slices.Contains(only, m.Name)) {
			continue
		}
		n, err := e.node(m)
		if err == nil {
			var lines []string
			if lines, err = clusterNodes.ClusterPull(ctx, n, c.SecretsFrom, items); err == nil {
				e.journal(id, actor, "pull", m.Name, 0, "%s pulled from %s: %s", m.Name, c.SecretsFrom, listOrNone(lines))
				continue
			}
		}
		failed = append(failed, fmt.Sprintf("%s: %v", m.Name, err))
	}
	if len(failed) > 0 {
		return fmt.Errorf("pulling from %s - %s", c.SecretsFrom, strings.Join(failed, "; "))
	}
	return nil
}

// addMember brings m into cluster id: checks, certificate, composition
// everywhere, the secrets and the desired version.
func (e *clusterEngine) addMember(ctx context.Context, id string, m clusters.Member, actor string) error {
	c, ok := e.store.Get(id)
	if !ok {
		return clusters.ErrNotFound
	}
	probe := *c
	probe.Members = append(slices.Clone(c.Members), m)
	if err := e.check(ctx, &probe, []clusters.Member{m}); err != nil {
		return err
	}
	c, err := e.store.Update(id, func(c *clusters.Cluster) error {
		m.Joined = time.Now().UTC()
		c.Members = append(c.Members, m)
		return nil
	})
	if err != nil {
		return err
	}
	if err := e.join(ctx, c, []string{m.Name}, actor); err != nil {
		return err
	}
	return e.secretsAndVersion(ctx, id, c.Desired, []string{m.Name}, actor)
}

// removeMember takes member name out of cluster id: drained, out of the
// cluster, the others' composition without it. force: even when it
// doesn't answer - a node gone for good.
func (e *clusterEngine) removeMember(ctx context.Context, id, name string, force bool, actor string) error {
	c, ok := e.store.Get(id)
	if !ok {
		return clusters.ErrNotFound
	}
	m := c.Member(name)
	if m == nil {
		return fmt.Errorf("%s isn't a member", name)
	}
	if len(c.Members) == 1 {
		return errors.New("the cluster's last member: delete the cluster instead")
	}
	if n, err := e.node(*m); err == nil {
		dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		msg, derr := clusterNodes.ClusterDrain(dctx, n, time.Minute)
		cancel()
		switch {
		case derr == nil:
			e.journal(id, actor, "drain", name, 0, "%s drained: %s", name, msg)
		case !force:
			return fmt.Errorf("%s didn't drain: %v - remove it with force to go on anyway", name, derr)
		}
		if err := clusterNodes.ClusterLeave(ctx, n); err != nil && !force {
			return fmt.Errorf("%s didn't leave: %v - remove it with force to go on anyway", name, err)
		}
	} else if !force {
		return err
	}
	c, err := e.store.Update(id, func(c *clusters.Cluster) error {
		c.Members = slices.DeleteFunc(c.Members, func(x clusters.Member) bool { return x.Name == name })
		if c.SecretsFrom == name {
			c.SecretsFrom = c.Members[0].Name
		}
		return nil
	})
	if err != nil {
		return err
	}
	_ = e.store.UpdateState(id, func(s *clusters.State) { delete(s.Members, name) })
	e.journal(id, actor, "leave", name, 0, "%s left the cluster", name)
	var failed []string
	for _, o := range c.Members {
		n, err := e.node(o)
		if err == nil {
			_, err = clusterNodes.ClusterSet(ctx, n, e.setRequest(c, o, nil, nil))
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", o.Name, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("the members without %s: %s", name, strings.Join(failed, "; "))
	}
	return nil
}

// remove ends cluster id: every member leaves it. force: even those that
// don't answer.
func (e *clusterEngine) remove(ctx context.Context, id string, force bool) error {
	c, ok := e.store.Get(id)
	if !ok {
		return clusters.ErrNotFound
	}
	var failed []string
	for _, m := range c.Members {
		n, err := e.node(m)
		if err == nil {
			err = clusterNodes.ClusterLeave(ctx, n)
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", m.Name, err))
		}
	}
	if len(failed) > 0 && !force {
		return fmt.Errorf("members that didn't leave - %s; delete with force to forget the cluster anyway", strings.Join(failed, "; "))
	}
	return e.store.Remove(id)
}

// uploadCertificate puts a certificate on the member the secrets come
// from - relayed as a node's page relays one, never kept - and has the
// others pull it.
func (e *clusterEngine) uploadCertificate(ctx context.Context, id string, req *janusv1alpha1.CertificateUploadRequest, actor string) error {
	c, ok := e.store.Get(id)
	if !ok {
		return clusters.ErrNotFound
	}
	m := c.Member(c.SecretsFrom)
	if m == nil {
		return errors.New("the cluster has no member the secrets come from")
	}
	n, err := e.node(*m)
	if err != nil {
		return err
	}
	if err := clusterNodes.CertificateUpload(ctx, n, req); err != nil {
		return fmt.Errorf("%s: %v", m.Name, err)
	}
	e.journal(id, actor, "certificate", m.Name, 0, "certificate %s uploaded to %s", req.GetName(), m.Name)
	return e.pull(ctx, id, nil, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_RUNTIME_CERTS}, actor)
}
