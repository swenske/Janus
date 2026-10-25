package clusters

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"slices"
	"strings"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// The cluster engine (docs/cluster.md sections 2-4), shared by the two
// orchestrators - the Controller and janusctl cluster: it creates a
// cluster from nodes, keeps its desired configuration in versions and
// applies them member by member, adds and removes members, and has the
// members pull their secrets from one another - never through the
// orchestrator. The caller runs one operation at a time per cluster, with
// a context that lets its calls through the members' locks.

// Actor is the journal's actor for what the engine does by itself
// (renewals, ticket rotation, drift seen).
const Actor = "cluster"

// Engine runs clusters kept in Store on the nodes behind Nodes.
type Engine struct {
	Store *Store
	Nodes Nodes
	// Ready: m's node may be one of a cluster's - the orchestrator
	// manages it (the Controller: one of its nodes, trusting its fleet).
	// Nil: any.
	Ready func(ctx context.Context, m Member) error
	// ManagedBy names the orchestrator on the members
	// (ClusterInfo.managed_by), the cluster's name after it.
	ManagedBy string
	// Advisory: the members' locks let an admin through (an orchestrator
	// without a Controller, janusctl cluster).
	Advisory bool
	// TicketsEvery is how often Reconcile rotates the TLS ticket key ring;
	// 0: every 24 hours.
	TicketsEvery time.Duration
	// LearnTimeout bounds the wait for a reloaded member to learn its
	// stick tables from its peers (waitLearnt); 0: 30 s.
	LearnTimeout time.Duration
}

// waitLearnt waits until member m's HAProxy, just reloaded, has learnt
// its stick tables from its peers - before the next member reloads: a
// process reloaded among remote peers learns from them only once its
// resync timeout passes (5 s, measured on 3.4.7), its tables empty until
// then, and members reloaded inside that window would leave no one to
// teach the others - the tables gone from every member. A member with no
// peer up has no one to learn from: its tables start empty, and the
// journal says so.
func (e *Engine) waitLearnt(ctx context.Context, c *Cluster, m Member, actor string) error {
	if len(c.Members) < 2 {
		return nil
	}
	timeout := e.LearnTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var st *janusv1alpha1.ClusterStatusResponse
	for {
		var err error
		if st, err = e.Nodes.ClusterStatus(ctx, m); err == nil && st.GetTablesLearnt() {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	for _, p := range st.GetPeers() {
		if !p.GetLocal() && p.GetUp() {
			return fmt.Errorf("its stick tables not learnt from its peers after %s - the next members keep theirs: look at its peers (janusctl cluster status), then apply again", timeout)
		}
	}
	e.Journal(c.ID, actor, "tables", m.Name, 0, "%s has no peer up to learn its stick tables from: they start empty", m.Name)
	return nil
}

// Journal records what was done on cluster id.
func (e *Engine) Journal(id, actor, action, member string, version int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if err := e.Store.Journal(id, Entry{Actor: actor, Action: action, Member: member, Version: version, Message: msg}); err != nil {
		log.Printf("cluster %s: journal: %v", id, err)
	}
}

// APIAddress is where the other members reach m's API.
func APIAddress(m Member) string {
	if m.API != "" {
		return m.API
	}
	return net.JoinHostPort(m.Address, "9505")
}

// SetRequest is ClusterSet for member m of c: the whole composition, and
// the CA and member certificate when given.
func (e *Engine) SetRequest(c *Cluster, m Member, caPEM, certPEM []byte) *janusv1alpha1.ClusterSetRequest {
	req := &janusv1alpha1.ClusterSetRequest{
		Id: c.ID, Name: c.Name, MemberName: m.Name, PeersPort: uint32(c.PeersPort),
		CaCert: caPEM, MemberCert: certPEM, Managed: c.Managed,
		ManagedBy: strings.TrimSpace(e.ManagedBy + " cluster " + c.Name), Advisory: e.Advisory,
	}
	for _, o := range c.Members {
		req.Members = append(req.Members, &janusv1alpha1.ClusterMember{Name: o.Name, Address: o.Address, ApiAddress: o.API})
	}
	// The secrets' source orders the Let's Encrypt certificates; the
	// thumbprint once Reconcile read it from it (acme.go).
	req.AcmeOrderer = c.SecretsFrom
	if st := e.Store.State(c.ID).ACME; st.Orderer == c.SecretsFrom {
		req.AcmeThumbprint = st.Thumbprint
	}
	return req
}

// ShortID is a cluster or Controller id as people read it.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ListOrNone is names, "none" when there's none.
func ListOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// Check is what a node must be to join c (docs/cluster.md "Checks"): one
// the orchestrator manages (Ready), answering, its HAProxy healthy, in no
// other cluster, on c's image, and reaching the other members' API as
// they reach its.
func (e *Engine) Check(ctx context.Context, c *Cluster, joining []Member) error {
	return e.check(ctx, c, joining, false)
}

// check is Check; newImage lets the joining members run another image
// (a replacement, the cluster changing image member by member).
func (e *Engine) check(ctx context.Context, c *Cluster, joining []Member, newImage bool) error {
	schematic := ""
	for _, m := range c.Members {
		if slices.ContainsFunc(joining, func(j Member) bool { return j.Name == m.Name }) {
			continue
		}
		v, err := e.Nodes.Version(ctx, m)
		if err != nil {
			return fmt.Errorf("member %s: %v", m.Name, err)
		}
		schematic = v.GetSchematicId()
		break
	}
	for _, m := range joining {
		if e.Ready != nil {
			if err := e.Ready(ctx, m); err != nil {
				return err
			}
		}
		v, err := e.Nodes.Version(ctx, m)
		if err != nil {
			return fmt.Errorf("%s: %v", m.Name, err)
		}
		switch {
		case newImage:
		case schematic == "":
			schematic = v.GetSchematicId()
		case v.GetSchematicId() != schematic:
			return fmt.Errorf("%s runs another image (schematic %s, the cluster's %s): a cluster's members share their extensions, HAProxy branch and kernel", m.Name, ShortID(v.GetSchematicId()), ShortID(schematic))
		}
		if err := e.Nodes.Healthy(ctx, m); err != nil {
			return fmt.Errorf("%s's HAProxy doesn't answer: %v", m.Name, err)
		}
		info, err := e.Nodes.ClusterGet(ctx, m)
		if err != nil {
			return fmt.Errorf("%s: %v", m.Name, err)
		}
		if info.GetMember() && info.GetId() != c.ID {
			return fmt.Errorf("%s is a member of cluster %s already: it leaves it first", m.Name, info.GetName())
		}
	}
	// Each joining member and every other reach each other's API: what a
	// pull goes over.
	for _, m := range joining {
		for _, o := range c.Members {
			if o.Name == m.Name {
				continue
			}
			for _, p := range [][2]Member{{m, o}, {o, m}} {
				if err := e.Nodes.Probe(ctx, p[0], APIAddress(p[1])); err != nil {
					return fmt.Errorf("%s doesn't reach %s's API at %s: %v - members reach each other on port 9505, and on the peers port %d", p[0].Name, p[1].Name, APIAddress(p[1]), err, c.PeersPort)
				}
			}
		}
	}
	return nil
}

// Join gives members of c their member certificate and the composition;
// the others get the composition.
func (e *Engine) Join(ctx context.Context, c *Cluster, joining []string, actor string) error {
	ca, err := e.Store.CA(c.ID)
	if err != nil {
		return err
	}
	certs := map[string][]byte{}
	for _, name := range joining {
		csr, err := e.Nodes.CertificateRequest(ctx, *c.Member(name))
		if err != nil {
			return fmt.Errorf("%s: a signing request: %v", name, err)
		}
		if certs[name], err = SignMember(ca, csr, c.ID, name); err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
	}
	for _, m := range c.Members {
		var caPEM, cert []byte
		if certs[m.Name] != nil {
			caPEM, cert = ca.CertPEM, certs[m.Name]
		}
		if _, err := e.Nodes.ClusterSet(ctx, m, e.SetRequest(c, m, caPEM, cert)); err != nil {
			return fmt.Errorf("%s: ClusterSet: %v", m.Name, err)
		}
		if cert != nil {
			e.Journal(c.ID, actor, "join", m.Name, 0, "%s joined: member certificate signed, composition of %d members", m.Name, len(c.Members))
		} else if err := e.waitLearnt(ctx, c, m, actor); err != nil {
			// A member already in reloaded with the new composition: the
			// next one waits until it has the tables back.
			return fmt.Errorf("%s: %v", m.Name, err)
		}
	}
	return nil
}

// Snapshot makes c's next version from member source's configuration:
// haproxy.cfg and the files that hold no key - those stay on source, and
// the others pull them.
func (e *Engine) Snapshot(ctx context.Context, c *Cluster, source, actor string) (int, error) {
	m := c.Member(source)
	if m == nil {
		return 0, fmt.Errorf("%s isn't a member", source)
	}
	files := map[string][]byte{}
	cfg, err := e.Nodes.HAProxyConfig(ctx, *m)
	if err != nil {
		return 0, fmt.Errorf("%s's haproxy.cfg: %v", source, err)
	}
	files["haproxy.cfg"] = cfg
	list, err := e.Nodes.Files(ctx, *m)
	if err != nil {
		return 0, fmt.Errorf("%s's files: %v", source, err)
	}
	keys := 0
	for _, f := range list {
		if f.GetSecret() {
			keys++
			continue
		}
		data, err := e.Nodes.FileGet(ctx, *m, f.GetName())
		if err != nil {
			return 0, fmt.Errorf("%s's file %s: %v", source, f.GetName(), err)
		}
		files["files/"+f.GetName()] = data
	}
	note := "taken from " + source
	if keys > 0 {
		note += fmt.Sprintf("; %d file(s) holding a key stay there, pulled by the members", keys)
	}
	v, err := e.Store.NewVersion(c.ID, actor, note, files)
	if err != nil {
		return 0, err
	}
	e.Journal(c.ID, actor, "version", source, v.Version, "version %d %s", v.Version, note)
	return v.Version, nil
}

// Create brings cluster id's members together: checks, certificates,
// the composition, the first version from source, then the secrets and
// that version on the others.
func (e *Engine) Create(ctx context.Context, id, source, actor string) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	if err := e.Check(ctx, c, c.Members); err != nil {
		return err
	}
	var names []string
	for _, m := range c.Members {
		names = append(names, m.Name)
	}
	if err := e.Join(ctx, c, names, actor); err != nil {
		return err
	}
	n, err := e.Snapshot(ctx, c, source, actor)
	if err != nil {
		return err
	}
	if err := e.SecretsAndVersion(ctx, id, n, nil, actor); err != nil {
		return err
	}
	for _, m := range c.Members {
		if err := e.inService(ctx, id, m, actor); err != nil {
			return err
		}
	}
	// Who orders the Let's Encrypt certificates, and its thumbprint, at
	// once rather than at the first check; journaled, not fatal.
	if cur, ok := e.Store.Get(id); ok {
		if err := e.SyncACME(ctx, cur, actor); err != nil {
			e.Journal(id, actor, "letsencrypt", "", 0, "%v - the next check tries again", err)
		}
	}
	return nil
}

// SecretsAndVersion brings only's members - nil: every one - the secrets
// and version n, in the order they need them: the secrets first, as the
// version may name a file holding a key that only a pull brings; then
// the version; then the certificate store again, for a certificate
// bound into a crt-list only the version brings (refused by the first
// pull, journaled; one already bound stays as it is).
func (e *Engine) SecretsAndVersion(ctx context.Context, id string, n int, only []string, actor string) error {
	if err := e.Pull(ctx, id, only, nil, actor); err != nil {
		return err
	}
	if n > 0 {
		if err := e.Apply(ctx, id, n, only, actor); err != nil {
			return err
		}
	}
	return e.Pull(ctx, id, only, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_RUNTIME_CERTS}, actor)
}

// Apply puts version n on cluster id's members - only's, or every
// member's -, one at a time (docs/cluster.md "Applying"): rendered for
// each first, checked on the first member, then on each member its
// files, HAProxy, the obsolete files gone, the firewall, VRRP, BGP. The
// first failure stops it: the members before run version n, the others
// what they ran.
func (e *Engine) Apply(ctx context.Context, id string, n int, only []string, actor string) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	v, err := e.Store.Version(id, n)
	if err != nil {
		return err
	}
	if c.Manages("haproxy") && v.Content["haproxy.cfg"] == nil {
		return fmt.Errorf("version %d has no haproxy.cfg, and the cluster manages HAProxy's configuration", n)
	}
	modules := []string{"firewall", "vrrp", "bgp"}
	rendered := map[string]*Rendered{}
	for _, m := range c.Members {
		r, err := Render(v, c, m.Name)
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
		targets = slices.DeleteFunc(slices.Clone(c.Members), func(m Member) bool { return !slices.Contains(only, m.Name) })
	}
	if len(targets) == 0 {
		return nil
	}
	// Checked on the first member before any is touched.
	first := targets[0]
	r := rendered[first.Name]
	if c.Manages("haproxy") {
		if err := e.Nodes.HAProxyValidate(ctx, first, r.HAProxy); err != nil {
			return fmt.Errorf("version %d, checked on %s: %v", n, first.Name, err)
		}
	}
	for _, d := range modules {
		if c.Manages(d) {
			if err := e.Nodes.ModuleApply(ctx, first, d, r.Modules[d], true); err != nil {
				return fmt.Errorf("version %d's %s, checked on %s: %v", n, d, first.Name, err)
			}
		}
	}
	st := e.Store.State(id)
	for i, m := range targets {
		err := e.applyMember(ctx, c, v, m, rendered[m.Name], st.Members[m.Name], actor)
		if err == nil && c.Manages("haproxy") {
			err = e.waitLearnt(ctx, c, m, actor)
		}
		if err != nil {
			_ = e.Store.UpdateState(id, func(s *State) {
				ms := s.Members[m.Name]
				ms.Error = err.Error()
				s.Members[m.Name] = ms
			})
			done := make([]string, 0, i)
			for _, p := range targets[:i] {
				done = append(done, p.Name)
			}
			return fmt.Errorf("partially applied - %s: %v (version %d on %s, the others as they were)", m.Name, err, n, ListOrNone(done))
		}
		hashes := map[string]string{}
		for d, h := range rendered[m.Name].Hashes() {
			if c.Manages(d) {
				hashes[d] = h
			}
		}
		_ = e.Store.UpdateState(id, func(s *State) {
			ms := s.Members[m.Name]
			ms.Applied, ms.Hashes, ms.Drift, ms.Released, ms.Error = n, hashes, nil, nil, ""
			s.Members[m.Name] = ms
		})
		e.Journal(id, actor, "apply", m.Name, n, "version %d applied on %s", n, m.Name)
	}
	if only == nil {
		_, err = e.Store.Update(id, func(c *Cluster) error {
			c.Desired = n
			return nil
		})
	}
	return err
}

// applyMember puts version v, rendered as r, on member m.
func (e *Engine) applyMember(ctx context.Context, c *Cluster, v *Version, m Member, r *Rendered, prev MemberState, actor string) error {
	if len(prev.Drift) > 0 || len(prev.Released) > 0 {
		e.Journal(c.ID, actor, "drift", m.Name, v.Version, "%s had drifted (%s) or been released (%s): version %d replaces it", m.Name, ListOrNone(prev.Drift), ListOrNone(prev.Released), v.Version)
	}
	// The locks back first: a domain released on the member is the
	// cluster's again.
	if _, err := e.Nodes.ClusterSet(ctx, m, e.SetRequest(c, m, nil, nil)); err != nil {
		return fmt.Errorf("ClusterSet: %v", err)
	}
	var obsolete []string
	if c.Manages("files") {
		have := map[string]string{}
		list, err := e.Nodes.Files(ctx, m)
		if err != nil {
			return err
		}
		for _, f := range list {
			have[f.GetName()] = f.GetSha256()
		}
		for name, data := range r.Files {
			if have[name] == Sum(data) {
				continue
			}
			if err := e.Nodes.FilePut(ctx, m, name, data); err != nil {
				return fmt.Errorf("file %s: %v", name, err)
			}
		}
		// The files the version it ran had, and this one hasn't: gone once
		// HAProxy no longer uses them.
		if pv, err := e.Store.Version(c.ID, prev.Applied); err == nil {
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
		if err := e.Nodes.HAProxyApply(ctx, m, r.HAProxy); err != nil {
			return err
		}
	}
	for _, name := range obsolete {
		if err := e.Nodes.FileDelete(ctx, m, name); err != nil {
			return fmt.Errorf("file %s, gone from the version: %v", name, err)
		}
	}
	for _, d := range []string{"firewall", "vrrp", "bgp"} {
		if c.Manages(d) {
			if err := e.Nodes.ModuleApply(ctx, m, d, r.Modules[d], false); err != nil {
				return fmt.Errorf("%s: %v", d, err)
			}
		}
	}
	return nil
}

// Pull has cluster id's members - only's, or every one - pull items
// (none: every kind) from the member the secrets come from.
func (e *Engine) Pull(ctx context.Context, id string, only []string, items []janusv1alpha1.ClusterItem, actor string) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	var failed []string
	for _, m := range c.Members {
		if m.Name == c.SecretsFrom || (only != nil && !slices.Contains(only, m.Name)) {
			continue
		}
		lines, err := e.Nodes.ClusterPull(ctx, m, c.SecretsFrom, items)
		if err == nil {
			e.Journal(id, actor, "pull", m.Name, 0, "%s pulled from %s: %s", m.Name, c.SecretsFrom, ListOrNone(lines))
			continue
		}
		failed = append(failed, fmt.Sprintf("%s: %v", m.Name, err))
	}
	if len(failed) > 0 {
		return fmt.Errorf("pulling from %s - %s", c.SecretsFrom, strings.Join(failed, "; "))
	}
	return nil
}

// AddMember brings m into cluster id: checks, certificate, composition
// everywhere, the secrets and the desired version.
func (e *Engine) AddMember(ctx context.Context, id string, m Member, actor string) error {
	return e.addMember(ctx, id, m, actor, false, false)
}

// addMember is AddMember; drained: m is drained as soon as it's a member
// (a replacement, out of service until the member it replaces leaves);
// newImage: m may run another image.
func (e *Engine) addMember(ctx context.Context, id string, m Member, actor string, drained, newImage bool) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	probe := *c
	probe.Members = append(slices.Clone(c.Members), m)
	// The cluster's version must render for it - its templates' variables
	// - before anything is touched.
	if c.Desired > 0 {
		v, err := e.Store.Version(id, c.Desired)
		if err != nil {
			return err
		}
		if _, err := Render(v, &probe, m.Name); err != nil {
			return fmt.Errorf("%s can't run version %d: %v - give it the variables the version's templates use", m.Name, c.Desired, err)
		}
	}
	if err := e.check(ctx, &probe, []Member{m}, newImage); err != nil {
		return err
	}
	c, err := e.Store.Update(id, func(c *Cluster) error {
		m.Joined = time.Now().UTC()
		c.Members = append(c.Members, m)
		return nil
	})
	if err != nil {
		return err
	}
	if err := e.Join(ctx, c, []string{m.Name}, actor); err != nil {
		return err
	}
	if drained {
		// Before its VRRP and BGP are configured: it starts BACKUP, its
		// routes disabled.
		dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		msg, err := e.Nodes.ClusterDrain(dctx, m, time.Minute)
		cancel()
		if err != nil {
			return fmt.Errorf("draining it before its configuration: %v", err)
		}
		e.setDrained(id, m.Name, true)
		e.Journal(id, actor, "drain", m.Name, 0, "%s joins drained: %s", m.Name, msg)
	}
	if err := e.SecretsAndVersion(ctx, id, c.Desired, []string{m.Name}, actor); err != nil {
		return err
	}
	if drained {
		return nil
	}
	if err := e.inService(ctx, id, m, actor); err != nil {
		return err
	}
	e.setDrained(id, m.Name, false)
	_ = e.announce(ctx, id, "in", m.Name, actor) // journaled; Reconcile calls again
	return nil
}

// inService undrains a member that joined drained - left so by an
// earlier removal, or by hand: joining a cluster puts a node in service
// (Replace's newcomer joins drained on purpose, undrained as it takes
// over).
func (e *Engine) inService(ctx context.Context, id string, m Member, actor string) error {
	info, err := e.Nodes.ClusterGet(ctx, m)
	if err != nil || !info.GetDrained() {
		return err
	}
	if err := e.Nodes.ClusterUndrain(ctx, m); err != nil {
		return fmt.Errorf("%s was drained, and undraining it failed: %v", m.Name, err)
	}
	e.Journal(id, actor, "undrain", m.Name, 0, "%s was drained - by an earlier removal, or by hand: in service as it joins", m.Name)
	return nil
}

// ReplaceOptions are a replacement's choices.
type ReplaceOptions struct {
	// NewImage: the replacement may run another image than the members'
	// - the cluster changing image member by member.
	NewImage bool
}

// Replace puts repl in member old's place in cluster id
// (docs/cluster.md "Recycling members"), the traffic moving once: repl
// joins drained - checked, its certificate, the others told, the
// secrets, the cluster's version, its stick tables learnt from its
// peers -, comes into service as old is drained, and old leaves. A
// failure before old's drain takes repl out again, the cluster as it
// was; old not drained is put back in service first; a failed removal
// leaves old drained and listed, to remove again. repl's variables: its
// own, else old's.
func (e *Engine) Replace(ctx context.Context, id, old string, repl Member, opts ReplaceOptions, actor string) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	om := c.Member(old)
	if om == nil {
		return fmt.Errorf("%s isn't a member", old)
	}
	if c.Member(repl.Name) != nil {
		return fmt.Errorf("%s is a member already: a replacement is a new one", repl.Name)
	}
	if repl.Vars == nil && om.Vars != nil {
		repl.Vars = map[string]string{}
		for k, v := range om.Vars {
			repl.Vars[k] = v
		}
	}
	e.Journal(id, actor, "replace", old, 0, "%s to be replaced by %s, which joins drained", old, repl.Name)
	if err := e.addMember(ctx, id, repl, actor, true, opts.NewImage); err != nil {
		return e.undoJoin(ctx, id, repl.Name, actor, fmt.Errorf("%s joining: %v", repl.Name, err))
	}
	// Learnt (the apply waited for its tables): into service, and old out
	// of it - the traffic moves once.
	if err := e.Nodes.ClusterUndrain(ctx, repl); err != nil {
		return e.undoJoin(ctx, id, repl.Name, actor, fmt.Errorf("%s back in service: %v", repl.Name, err))
	}
	e.setDrained(id, repl.Name, false)
	_ = e.announce(ctx, id, "in", repl.Name, actor) // journaled; Reconcile calls again
	// The balancer stops sending to old first, then old is drained.
	if err := e.leaving(ctx, id, old, false, actor); err != nil {
		rctx, rcancel := context.WithTimeout(ctx, 90*time.Second)
		_, _ = e.Nodes.ClusterDrain(rctx, repl, time.Minute)
		rcancel()
		e.setDrained(id, repl.Name, true)
		return e.undoJoin(ctx, id, repl.Name, actor, err)
	}
	dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	msg, err := e.Nodes.ClusterDrain(dctx, *om, time.Minute)
	cancel()
	if err != nil {
		uerr := e.Nodes.ClusterUndrain(ctx, *om)
		rctx, rcancel := context.WithTimeout(ctx, 90*time.Second)
		_, _ = e.Nodes.ClusterDrain(rctx, repl, time.Minute)
		rcancel()
		e.setDrained(id, repl.Name, true)
		_ = e.announce(ctx, id, "in", old, actor) // old serves on
		cause := fmt.Errorf("%s didn't drain: %v", old, err)
		if uerr != nil {
			cause = fmt.Errorf("%v; and back in service: %v", cause, uerr)
		}
		return e.undoJoin(ctx, id, repl.Name, actor, cause)
	}
	e.setDrained(id, old, true)
	e.Journal(id, actor, "drain", old, 0, "%s drained, %s in service: %s", old, repl.Name, msg)
	if err := e.removeMember(ctx, id, old, false, true, actor); err != nil {
		if cur, ok := e.Store.Get(id); ok && cur.Member(old) == nil {
			// Out, but what follows its departure went wrong somewhere.
			e.Journal(id, actor, "replace", repl.Name, 0, "%s replaced %s, but: %v", repl.Name, old, err)
			return fmt.Errorf("%s replaced %s, but: %w", repl.Name, old, err)
		}
		return &NotRemovedError{Old: old, New: repl.Name, Err: err}
	}
	e.Journal(id, actor, "replace", repl.Name, 0, "%s replaced %s", repl.Name, old)
	return nil
}

// NotRemovedError: a replacement is in service, but the member it
// replaces, drained, couldn't be removed - it stays listed, to remove
// again; the replacement stays.
type NotRemovedError struct {
	Old, New string
	Err      error
}

func (e *NotRemovedError) Error() string {
	return fmt.Sprintf("%s is in service, but %s's removal failed: %v - %s stays drained: remove it again", e.New, e.Old, e.Err, e.Old)
}

func (e *NotRemovedError) Unwrap() error { return e.Err }

// undoJoin takes a replacement that couldn't take over out of cluster
// id again.
func (e *Engine) undoJoin(ctx context.Context, id, name, actor string, cause error) error {
	if c, ok := e.Store.Get(id); ok && c.Member(name) != nil {
		if err := e.RemoveMember(ctx, id, name, true, actor); err != nil {
			return fmt.Errorf("%v; taking %s out again failed too: %v", cause, name, err)
		}
	}
	return fmt.Errorf("%v - %s taken out again, the cluster as it was", cause, name)
}

// RemoveMember takes member name out of cluster id: drained, out of the
// cluster, the others' composition without it. force: even when it
// doesn't answer - a node gone for good.
func (e *Engine) RemoveMember(ctx context.Context, id, name string, force bool, actor string) error {
	return e.removeMember(ctx, id, name, force, false, actor)
}

// removeMember is RemoveMember; drained: the member was drained just
// before (Replace) - not drained, nor journaled, again.
func (e *Engine) removeMember(ctx context.Context, id, name string, force, drained bool, actor string) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	m := c.Member(name)
	if m == nil {
		return fmt.Errorf("%s isn't a member", name)
	}
	if len(c.Members) == 1 {
		return errors.New("the cluster's last member: delete the cluster instead")
	}
	leftDrained := drained
	if !drained {
		if err := e.leaving(ctx, id, name, force, actor); err != nil {
			return err
		}
		dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		msg, err := e.Nodes.ClusterDrain(dctx, *m, time.Minute)
		cancel()
		leftDrained = err == nil
		switch {
		case err == nil:
			e.Journal(id, actor, "drain", name, 0, "%s drained: %s", name, msg)
		case !force:
			_ = e.announce(ctx, id, "in", name, actor) // it serves on
			return fmt.Errorf("%s didn't drain: %v - remove it with force to go on anyway", name, err)
		}
	}
	if err := e.Nodes.ClusterLeave(ctx, *m); err != nil && !force {
		return fmt.Errorf("%s didn't leave: %v - remove it with force to go on anyway", name, err)
	}
	c, err := e.Store.Update(id, func(c *Cluster) error {
		c.Members = slices.DeleteFunc(c.Members, func(x Member) bool { return x.Name == name })
		if c.SecretsFrom == name {
			c.SecretsFrom = c.Members[0].Name
		}
		return nil
	})
	if err != nil {
		return err
	}
	_ = e.Store.UpdateState(id, func(s *State) {
		delete(s.Members, name)
		if !slices.Contains(s.TicketsRenew, name) {
			s.TicketsRenew = append(s.TicketsRenew, name)
		}
	})
	if leftDrained {
		// It may still run the cluster's VRRP or BGP configuration: out
		// of service until someone decides otherwise.
		e.Journal(id, actor, "leave", name, 0, "%s left the cluster, drained: out of service on its own until undrained (cluster undrain) or rebooted", name)
	} else {
		e.Journal(id, actor, "leave", name, 0, "%s left the cluster", name)
	}
	_ = e.announce(ctx, id, "sync", "", actor) // without it; journaled, Reconcile calls again
	var failed []string
	for _, o := range c.Members {
		if _, err := e.Nodes.ClusterSet(ctx, o, e.SetRequest(c, o, nil, nil)); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", o.Name, err))
		} else if err := e.waitLearnt(ctx, c, o, actor); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", o.Name, err))
		}
	}
	// It knew the ring: the cluster's is renewed (a failure, retried by
	// the next check).
	if err := e.RenewTickets(ctx, c, actor); err != nil {
		failed = append(failed, err.Error()+" - the next check tries again")
	}
	if len(failed) > 0 {
		return fmt.Errorf("the members without %s: %s", name, strings.Join(failed, "; "))
	}
	return nil
}

// Remove ends cluster id: every member leaves it. force: even those that
// don't answer.
func (e *Engine) Remove(ctx context.Context, id string, force bool) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	var failed []string
	for _, m := range c.Members {
		if err := e.Nodes.ClusterLeave(ctx, m); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", m.Name, err))
		}
	}
	if len(failed) > 0 && !force {
		return fmt.Errorf("members that didn't leave - %s; delete with force to forget the cluster anyway", strings.Join(failed, "; "))
	}
	return e.Store.Remove(id)
}

// UploadCertificate puts a certificate on the member the secrets come
// from - relayed, never kept - and has the others pull it.
func (e *Engine) UploadCertificate(ctx context.Context, id string, req *janusv1alpha1.CertificateUploadRequest, actor string) error {
	c, ok := e.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	m := c.Member(c.SecretsFrom)
	if m == nil {
		return errors.New("the cluster has no member the secrets come from")
	}
	if err := e.Nodes.CertificateUpload(ctx, *m, req); err != nil {
		return fmt.Errorf("%s: %v", m.Name, err)
	}
	e.Journal(id, actor, "certificate", m.Name, 0, "certificate %s uploaded to %s", req.GetName(), m.Name)
	return e.Pull(ctx, id, nil, []janusv1alpha1.ClusterItem{janusv1alpha1.ClusterItem_CLUSTER_ITEM_RUNTIME_CERTS}, actor)
}
