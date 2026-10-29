package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/clusters"
	"github.com/swenske/Janus/internal/pki"
)

// The clusters' API (docs/cluster.md): readers see them, admins change
// them - a cluster's composition is how its nodes are set up. What takes
// a while runs in the background (clusterEngine.start): 202, and the
// cluster's phase until it's done; 409 while another operation runs.

func (a *app) registerClusterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/clusters", a.gate(auth.Reader, auth.Admin, a.handleClusters))
	mux.HandleFunc("GET /api/metrics", a.gate(auth.Reader, auth.Admin, a.handleMetrics))
	mux.HandleFunc("POST /api/clusters", a.gate(auth.Reader, auth.Admin, a.handleClusterCreate))
	mux.HandleFunc("GET /api/clusters/{id}", a.gate(auth.Reader, auth.Admin, a.handleCluster))
	mux.HandleFunc("PATCH /api/clusters/{id}", a.gate(auth.Reader, auth.Admin, a.handleClusterPatch))
	mux.HandleFunc("DELETE /api/clusters/{id}", a.gate(auth.Reader, auth.Admin, a.handleClusterDelete))
	mux.HandleFunc("POST /api/clusters/{id}/members", a.gate(auth.Reader, auth.Admin, a.handleClusterMemberAdd))
	mux.HandleFunc("DELETE /api/clusters/{id}/members/{name}", a.gate(auth.Reader, auth.Admin, a.handleClusterMemberRemove))
	mux.HandleFunc("POST /api/clusters/{id}/members/{name}/drain", a.gate(auth.Reader, auth.Admin, a.handleClusterDrain(true)))
	mux.HandleFunc("POST /api/clusters/{id}/members/{name}/undrain", a.gate(auth.Reader, auth.Admin, a.handleClusterDrain(false)))
	mux.HandleFunc("GET /api/clusters/{id}/versions", a.gate(auth.Reader, auth.Admin, a.handleClusterVersions))
	mux.HandleFunc("POST /api/clusters/{id}/versions", a.gate(auth.Reader, auth.Admin, a.handleClusterVersionNew))
	mux.HandleFunc("GET /api/clusters/{id}/versions/{n}", a.gate(auth.Reader, auth.Admin, a.handleClusterVersion))
	mux.HandleFunc("POST /api/clusters/{id}/apply", a.gate(auth.Reader, auth.Admin, a.handleClusterApply))
	mux.HandleFunc("POST /api/clusters/{id}/pull", a.gate(auth.Reader, auth.Admin, a.handleClusterPull))
	mux.HandleFunc("POST /api/clusters/{id}/recycle", a.gate(auth.Reader, auth.Admin, a.handleClusterRecycle))
	mux.HandleFunc("POST /api/clusters/{id}/upgrade", a.gate(auth.Reader, auth.Admin, a.handleClusterUpgrade))
	mux.HandleFunc("POST /api/clusters/{id}/scale", a.gate(auth.Reader, auth.Admin, a.handleClusterScale))
	mux.HandleFunc("PUT /api/clusters/{id}/autoscale", a.gate(auth.Reader, auth.Admin, a.handleClusterAutoscale))
	mux.HandleFunc("PUT /api/clusters/{id}/balancer", a.gate(auth.Reader, auth.Admin, a.handleClusterBalancer))
	mux.HandleFunc("DELETE /api/clusters/{id}/balancer", a.gate(auth.Reader, auth.Admin, a.handleClusterBalancerDelete))
	mux.HandleFunc("POST /api/clusters/{id}/balancer/sync", a.gate(auth.Reader, auth.Admin, a.handleClusterBalancerSync))
	mux.HandleFunc("POST /api/clusters/{id}/certificates", a.gate(auth.Reader, auth.Admin, a.handleClusterCertificate))
	mux.HandleFunc("GET /api/clusters/{id}/journal", a.gate(auth.Reader, auth.Admin, a.handleClusterJournal))
	mux.HandleFunc("GET /api/clusters/{id}/router", a.gate(auth.Reader, auth.Admin, a.handleClusterRouter))
	// What the loop does every 10 minutes, now: it may renew a member's
	// certificate, so it's an admin's.
	mux.HandleFunc("POST /api/clusters/{id}/check", a.gate(auth.Reader, auth.Admin, a.handleClusterCheck))
}

func (a *app) handleClusterCheck(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	if a.clusters.isBusy(c.ID) {
		writeError(w, http.StatusConflict, "an operation runs on cluster "+c.Name+": wait for it to end")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	a.clusters.reconcile(ctx, c)
	if cur, ok := a.clusters.Store.Get(c.ID); ok {
		c = cur
	}
	writeJSON(w, http.StatusOK, a.clusterView(c))
}

type clusterMemberView struct {
	clusters.Member
	NodeName  string `json:"node_name"`
	NodeFound bool   `json:"node_found"`
	// MachineID: its node runs on one of this Controller's machines - a
	// recycle can make it a new one.
	MachineID string `json:"machine_id,omitempty"`
	// HostID and HostName: the physical host its machine runs on.
	HostID   string               `json:"host_id,omitempty"`
	HostName string               `json:"host_name,omitempty"`
	State    clusters.MemberState `json:"state"`
}

type clusterView struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	PeersPort      int       `json:"peers_port"`
	Managed        []string  `json:"managed"`
	SecretsFrom    string    `json:"secrets_from"`
	Desired        int       `json:"desired"`
	Created        time.Time `json:"created"`
	Updated        time.Time `json:"updated"`
	Phase          string    `json:"phase,omitempty"`
	Error          string    `json:"error,omitempty"`
	Busy           bool      `json:"busy"`
	CAFingerprint  string    `json:"ca_fingerprint,omitempty"`
	TicketsRotated time.Time `json:"tickets_rotated,omitzero"`
	// TicketsRenew: members that left while the ring is still the one
	// they knew.
	TicketsRenew []string            `json:"tickets_renew,omitempty"`
	Placement    *clusters.Placement `json:"placement,omitempty"`
	// Autoscale is the horizontal auto-scaling's policy; ScaleStatus what
	// it says now, ScaleJudged the member count it judges (observing: the
	// one it simulates), Load the members' last reading.
	Autoscale   *clusters.Autoscale   `json:"autoscale,omitempty"`
	ScaleStatus string                `json:"scale_status,omitempty"`
	ScaleJudged int                   `json:"scale_judged,omitempty"`
	Load        *clusters.ScaleSample `json:"load,omitempty"`
	// Balancer is its hook, BalancerState its last call.
	Balancer      *clusters.Balancer      `json:"balancer,omitempty"`
	BalancerState *clusters.BalancerState `json:"balancer_state,omitempty"`
	Members       []clusterMemberView     `json:"members"`
	// Problems are what an operator should look at, worded for one.
	Problems []string `json:"problems"`
}

func (a *app) clusterView(c *clusters.Cluster) clusterView {
	st := a.clusters.Store.State(c.ID)
	v := clusterView{ID: c.ID, Name: c.Name, PeersPort: c.PeersPort, Managed: nonNil(c.Managed), SecretsFrom: c.SecretsFrom,
		Desired: c.Desired, Created: c.Created, Updated: c.Updated, Phase: c.Phase, Error: c.Error, Busy: a.clusters.isBusy(c.ID),
		TicketsRotated: st.TicketsRotated, TicketsRenew: st.TicketsRenew, Placement: c.Placement, Members: []clusterMemberView{}, Problems: []string{},
		Autoscale: c.Autoscale}
	if c.Autoscale != nil && c.Autoscale.Mode != clusters.ScaleOff {
		v.ScaleStatus, v.ScaleJudged = st.ScaleStatus, st.Scale.Members
	}
	v.Load = a.clusterScaler.load(c.ID)
	if caPEM, err := a.clusters.Store.CACertPEM(c.ID); err == nil {
		if fp, err := pemFingerprint(caPEM); err == nil {
			v.CAFingerprint = fp
		}
	}
	if c.Error != "" {
		v.Problems = append(v.Problems, c.Error)
	}
	if len(st.TicketsRenew) > 0 && !v.Busy {
		v.Problems = append(v.Problems, fmt.Sprintf("the TLS ticket key ring is still the one %s knew: renewing it failed, the next check tries again", strings.Join(st.TicketsRenew, ", ")))
	}
	for _, m := range c.Members {
		mv := clusterMemberView{Member: m, State: st.Members[m.Name]}
		if n, ok := a.store.Get(m.NodeID); ok {
			mv.NodeName, mv.NodeFound = n.Name, true
		}
		if mach := a.machineOf(m.NodeID); mach != nil {
			mv.MachineID = mach.ID
		}
		mv.HostID, mv.HostName = a.memberHost(m)
		v.Members = append(v.Members, mv)
		v.Problems = append(v.Problems, memberProblems(c, mv)...)
	}
	v.Problems = append(v.Problems, placementProblems(c, v.Members)...)
	if c.Balancer != nil {
		bs := st.Balancer
		v.Balancer, v.BalancerState = c.Balancer, &bs
		if bs.Error != "" {
			v.Problems = append(v.Problems, "the balancer in front wasn't told the members: "+bs.Error+" - the next check calls it again")
		}
	}
	return v
}

// memberProblems are what's wrong with member mv, worded for an operator.
func memberProblems(c *clusters.Cluster, mv clusterMemberView) []string {
	s, name := mv.State, mv.Name
	var out []string
	switch {
	case !mv.NodeFound:
		out = append(out, name+": its node is no longer one of the Controller's")
	case s.Error != "":
		out = append(out, name+": "+s.Error)
	}
	if c.Desired > 0 && s.Applied != c.Desired && c.Phase == "" {
		out = append(out, fmt.Sprintf("%s runs version %d, the cluster's is %d", name, s.Applied, c.Desired))
	}
	if len(s.Drift) > 0 {
		out = append(out, fmt.Sprintf("%s has drifted: %s changed on the node", name, strings.Join(s.Drift, ", ")))
	}
	if len(s.Released) > 0 {
		out = append(out, fmt.Sprintf("%s's users took back %s", name, strings.Join(s.Released, ", ")))
	}
	if !s.Checked.IsZero() && s.PeersUp < s.PeersTotal {
		out = append(out, fmt.Sprintf("%s sees %d of its %d peers", name, s.PeersUp, s.PeersTotal))
	}
	if s.Drained {
		out = append(out, name+" is drained: out of service")
	}
	return out
}

func pemFingerprint(p []byte) (string, error) {
	c, err := parseCertPEM(p)
	if err != nil {
		return "", err
	}
	return pki.Fingerprint(c.Raw), nil
}

func (a *app) clusterOr404(w http.ResponseWriter, r *http.Request) (*clusters.Cluster, bool) {
	c, ok := a.clusters.Store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such cluster")
		return nil, false
	}
	return c, true
}

// run starts an operation on cluster c and answers 202 with the cluster,
// or 409 when another runs.
func (a *app) runCluster(w http.ResponseWriter, c *clusters.Cluster, phase, actor string, fn func(ctx context.Context) error) {
	if !a.clusters.start(c.ID, phase, actor, fn) {
		writeError(w, http.StatusConflict, "an operation already runs on cluster "+c.Name+": wait for it to end")
		return
	}
	if cur, ok := a.clusters.Store.Get(c.ID); ok {
		c = cur
	}
	writeJSON(w, http.StatusAccepted, a.clusterView(c))
}

func (a *app) handleClusters(w http.ResponseWriter, _ *http.Request) {
	out := []clusterView{}
	for _, c := range a.clusters.Store.List() {
		out = append(out, a.clusterView(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleCluster(w http.ResponseWriter, r *http.Request) {
	if c, ok := a.clusterOr404(w, r); ok {
		writeJSON(w, http.StatusOK, a.clusterView(c))
	}
}

type clusterMemberRequest struct {
	NodeID  string            `json:"node_id"`
	Name    string            `json:"name"`
	Address string            `json:"address"`
	API     string            `json:"api_address"`
	Vars    map[string]string `json:"vars"`
}

// member makes a member of req: its name the node's when none is given,
// its address the node's own when it can be.
func (a *app) member(req clusterMemberRequest) (clusters.Member, error) {
	n, ok := a.store.Get(req.NodeID)
	if !ok {
		return clusters.Member{}, fmt.Errorf("no node %s", req.NodeID)
	}
	if other := a.clusters.Store.ClusterOf(n.ID); other != "" {
		return clusters.Member{}, fmt.Errorf("%s is a member of a cluster already", n.Name)
	}
	m := clusters.Member{NodeID: n.ID, Name: req.Name, Address: req.Address, API: req.API, Vars: req.Vars, Joined: time.Now().UTC()}
	if m.Name == "" {
		m.Name = strings.ToLower(n.Name)
	}
	if m.Address == "" {
		host, _, _ := net.SplitHostPort(n.Addr())
		if ip := net.ParseIP(host); ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			return clusters.Member{}, fmt.Errorf("%s: give its address - the one the other members reach it on (the Controller reaches it at %s)", n.Name, n.Addr())
		}
		m.Address = host
	}
	if ip := net.ParseIP(m.Address); ip == nil {
		return clusters.Member{}, fmt.Errorf("%s: %q isn't an IP address", n.Name, m.Address)
	}
	return m, nil
}

func (a *app) handleClusterCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string                 `json:"name"`
		PeersPort int                    `json:"peers_port"`
		Managed   []string               `json:"managed"`
		Source    string                 `json:"source"` // a node id: its configuration is version 1, its secrets everyone's
		Members   []clusterMemberRequest `json:"members"`
		// Machines: the cluster made of new machines instead of nodes.
		Machines *clusterMachinesRequest `json:"machines"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Managed == nil {
		req.Managed = []string{"haproxy", "files"}
	}
	for _, d := range req.Managed {
		if d != "haproxy" && d != "files" {
			writeError(w, http.StatusBadRequest, "a cluster starts managing HAProxy's configuration and files; VRRP, BGP and the firewall come with a version carrying their template (apply it with managed)")
			return
		}
	}
	if req.Machines != nil {
		if len(req.Members) > 0 || req.Source != "" {
			writeError(w, http.StatusBadRequest, "machines or members and source: new machines, or nodes already there")
			return
		}
		a.clusterOnHypervisors(w, r, req.Name, req.PeersPort, req.Managed, *req.Machines)
		return
	}
	if len(req.Members) == 0 {
		writeError(w, http.StatusBadRequest, "a cluster has members")
		return
	}
	var members []clusters.Member
	source := ""
	for _, mr := range req.Members {
		m, err := a.member(mr)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if m.NodeID == req.Source {
			source = m.Name
		}
		members = append(members, m)
	}
	if source == "" {
		writeError(w, http.StatusBadRequest, "source: one of the members - its configuration becomes the cluster's, its certificates and keys everyone's")
		return
	}
	c, err := a.clusters.Store.Create(req.Name, req.PeersPort, req.Managed)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if c, err = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		c.Members, c.SecretsFrom = members, source
		return nil
	}); err != nil {
		_ = a.clusters.Store.Remove(c.ID)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	noteAuditSubject(r, audit.KindCluster, c.ID, c.Name)
	actor := requestPrincipal(r).User
	a.clusters.Journal(c.ID, actor, "create", "", 0, "cluster %s created with %d members, from %s", c.Name, len(members), source)
	a.runCluster(w, c, "creating", actor, func(ctx context.Context) error { return a.clusters.Create(ctx, c.ID, source, actor) })
}

func (a *app) handleClusterPatch(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var req struct {
		SecretsFrom *string                      `json:"secrets_from"`
		Vars        map[string]map[string]string `json:"vars"` // by member: what its templates use from the next apply on
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if a.clusters.isBusy(c.ID) {
		writeError(w, http.StatusConflict, "an operation runs on cluster "+c.Name)
		return
	}
	c, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		if req.SecretsFrom != nil {
			c.SecretsFrom = *req.SecretsFrom
		}
		for name, vars := range req.Vars {
			m := c.Member(name)
			if m == nil {
				return fmt.Errorf("%s isn't a member", name)
			}
			m.Vars = vars
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.clusterView(c))
}

// handleClusterDelete ends cluster id: every member leaves it (force:
// even those that don't answer); machines=destroy also destroys the
// members that are this Controller's machines, once they left - the
// others stay as they are.
func (a *app) handleClusterDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "true"
	destroy := false
	switch r.URL.Query().Get("machines") {
	case "":
	case "destroy":
		destroy = true
	default:
		writeError(w, http.StatusBadRequest, "machines: destroy, or nothing to keep them")
		return
	}
	var vms []string // the members' machines, by ID
	if destroy {
		for _, m := range c.Members {
			if mach := a.machineOf(m.NodeID); mach != nil {
				vms = append(vms, mach.ID)
			}
		}
	}
	actor := requestPrincipal(r).User
	a.runCluster(w, c, "deleting", actor, func(ctx context.Context) error {
		if err := a.clusters.Remove(ctx, c.ID, force); err != nil {
			return err
		}
		for _, id := range vms {
			a.destroyMachine(id)
		}
		if len(vms) > 0 {
			log.Printf("clusters: cluster %s deleted by %s, its %d member machine(s) being destroyed", c.Name, actor, len(vms))
		}
		return nil
	})
}

func (a *app) handleClusterMemberAdd(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var req clusterMemberRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := a.member(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if c.Member(m.Name) != nil {
		writeError(w, http.StatusBadRequest, "a member is named "+m.Name+" already")
		return
	}
	actor := requestPrincipal(r).User
	a.runCluster(w, c, "adding "+m.Name, actor, func(ctx context.Context) error { return a.clusters.AddMember(ctx, c.ID, m, actor) })
}

func (a *app) handleClusterMemberRemove(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if c.Member(name) == nil {
		writeError(w, http.StatusNotFound, name+" isn't a member")
		return
	}
	force := r.URL.Query().Get("force") == "true"
	actor := requestPrincipal(r).User
	a.runCluster(w, c, "removing "+name, actor, func(ctx context.Context) error { return a.clusters.RemoveMember(ctx, c.ID, name, force, actor) })
}

func (a *app) handleClusterDrain(drain bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := a.clusterOr404(w, r)
		if !ok {
			return
		}
		name := r.PathValue("name")
		m := c.Member(name)
		if m == nil {
			writeError(w, http.StatusNotFound, name+" isn't a member")
			return
		}
		actor := requestPrincipal(r).User
		phase := map[bool]string{true: "draining ", false: "undraining "}[drain] + name
		a.runCluster(w, c, phase, actor, func(ctx context.Context) error {
			if !drain {
				return a.clusters.Undrain(ctx, c.ID, name, actor)
			}
			return a.clusters.Drain(ctx, c.ID, name, actor)
		})
	}
}

func (a *app) handleClusterVersions(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	vs, err := a.clusters.Store.Versions(c.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, nonNil(vs))
}

type clusterVersionView struct {
	clusters.VersionInfo
	Content map[string]string `json:"content"`
}

func (a *app) handleClusterVersion(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "a version is a number")
		return
	}
	v, err := a.clusters.Store.Version(c.ID, n)
	if errors.Is(err, clusters.ErrNoVersion) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := clusterVersionView{VersionInfo: v.VersionInfo, Content: map[string]string{}}
	for p, b := range v.Content {
		out.Content[p] = string(b)
	}
	writeJSON(w, http.StatusOK, out)
}

// maxVersionBody bounds a new version's request: its files, as text.
const maxVersionBody = 16 << 20

func (a *app) handleClusterVersionNew(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var req struct {
		// Base: the version the new one starts from (0: the desired one;
		// -1: none) - Files then add or replace, Delete takes away.
		Base   int               `json:"base"`
		Files  map[string]string `json:"files"`
		Delete []string          `json:"delete"`
		Note   string            `json:"note"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxVersionBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode request: "+err.Error())
		return
	}
	files := map[string][]byte{}
	base := req.Base
	if base == 0 {
		base = c.Desired
	}
	if base > 0 {
		v, err := a.clusters.Store.Version(c.ID, base)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("version %d: %v", base, err))
			return
		}
		for p, b := range v.Content {
			files[p] = b
		}
	}
	for p, s := range req.Files {
		if !utf8.ValidString(s) {
			writeError(w, http.StatusBadRequest, p+": a version holds text files")
			return
		}
		files[p] = []byte(s)
	}
	for _, p := range req.Delete {
		delete(files, p)
	}
	info, err := a.clusters.Store.NewVersion(c.ID, requestPrincipal(r).User, req.Note, files)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.clusters.Journal(c.ID, requestPrincipal(r).User, "version", "", info.Version, "version %d made (%d files)", info.Version, len(info.Files))
	writeJSON(w, http.StatusCreated, info)
}

func (a *app) handleClusterApply(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var req struct {
		Version int      `json:"version"`
		Managed []string `json:"managed"` // what the cluster manages from this apply on; empty: as before
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if _, err := a.clusters.Store.Version(c.ID, req.Version); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("version %d: %v", req.Version, err))
		return
	}
	if req.Managed != nil {
		for _, d := range req.Managed {
			if !slices.Contains(clusters.Domains, d) {
				writeError(w, http.StatusBadRequest, "unknown domain "+d)
				return
			}
		}
		// Turning auto-scaling on is refused on a VRRP cluster; so is
		// bringing VRRP in under auto-scaling (v2026.10.24-5's lab).
		if as := c.Autoscale; as != nil && as.Mode != clusters.ScaleOff && slices.Contains(req.Managed, "vrrp") && !slices.Contains(c.Managed, "vrrp") {
			writeError(w, http.StatusConflict, "auto-scaling is "+as.Mode+": a VRRP cluster can't scale (one member holds each virtual IP, a member more adds no capacity) - set it off first")
			return
		}
		if a.clusters.isBusy(c.ID) {
			writeError(w, http.StatusConflict, "an operation runs on cluster "+c.Name)
			return
		}
		if _, err := a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
			c.Managed = req.Managed
			return nil
		}); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	actor := requestPrincipal(r).User
	a.runCluster(w, c, fmt.Sprintf("applying version %d", req.Version), actor, func(ctx context.Context) error {
		return a.clusters.Apply(ctx, c.ID, req.Version, nil, actor)
	})
}

func (a *app) handleClusterPull(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	actor := requestPrincipal(r).User
	a.runCluster(w, c, "pulling the secrets", actor, func(ctx context.Context) error { return a.clusters.Pull(ctx, c.ID, nil, nil, actor) })
}

func (a *app) handleClusterCertificate(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var req struct {
		Name    string   `json:"name"`
		PEM     string   `json:"pem"`
		CrtList string   `json:"crt_list"`
		SNI     []string `json:"sni"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	up := &janusv1alpha1.CertificateUploadRequest{Name: req.Name, PemBundle: []byte(req.PEM), CrtList: req.CrtList, Sni: req.SNI}
	actor := requestPrincipal(r).User
	a.runCluster(w, c, "uploading certificate "+req.Name, actor, func(ctx context.Context) error {
		return a.clusters.UploadCertificate(ctx, c.ID, up, actor)
	})
}

func (a *app) handleClusterJournal(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 1000 {
		n = 100
	}
	writeJSON(w, http.StatusOK, nonNil(a.clusters.Store.Entries(c.ID, n)))
}

// The balancer hook (internal/clusters/balancer.go): PUT sets it - the
// secret write-only, absent to keep it, "" to remove it - and calls it
// with the members at once; the settings are kept even when that call
// fails, which the answer says (and the cluster's problems).
func (a *app) handleClusterBalancer(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var req struct {
		clusters.Balancer
		Secret *string `json:"secret"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	b := req.Balancer
	if err := b.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := a.clusters.SetBalancer(ctx, c.ID, &b, req.Secret, requestPrincipal(r).User)
	c, _ = a.clusters.Store.Get(c.ID)
	if err != nil && (c == nil || c.Balancer == nil) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.clusterView(c))
}

func (a *app) handleClusterBalancerDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	if err := a.clusters.SetBalancer(r.Context(), c.ID, nil, nil, requestPrincipal(r).User); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	c, _ = a.clusters.Store.Get(c.ID)
	writeJSON(w, http.StatusOK, a.clusterView(c))
}

// handleClusterBalancerSync calls the hook with the members now: 502
// with its failure.
func (a *app) handleClusterBalancerSync(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	if c.Balancer == nil {
		writeError(w, http.StatusBadRequest, "cluster "+c.Name+" has no balancer hook")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := a.clusters.SyncBalancer(ctx, c.ID, requestPrincipal(r).User); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	c, _ = a.clusters.Store.Get(c.ID)
	writeJSON(w, http.StatusOK, a.clusterView(c))
}
