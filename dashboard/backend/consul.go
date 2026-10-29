package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/consulctl"
	"github.com/swenske/Janus/dashboard/backend/internal/labels"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// The nodes' Consul agents (internal/consulctl, docs/guide/
// consul-controller.md): every 10 minutes - every minute while a node
// can't take its layer -, or when poked (the settings, a node admitted,
// its labels, its extensions, a cluster's members), each node the
// selector picks and whose image has the consul extension is given its
// own token in Consul and the managed layer; a node no longer picked,
// or gone, has the layer taken off, its token deleted and its entry in
// the catalog deregistered.

const (
	consulEvery     = 10 * time.Minute
	consulRetry     = time.Minute
	consulNodeLimit = 30 * time.Second
)

// consulNodes is how the loop reaches a node - a fake in the tests.
type consulNodes interface {
	ManagedGet(ctx context.Context, n *store.Node) (*janusv1alpha1.ConsulManagedGetResponse, error)
	ManagedSet(ctx context.Context, n *store.Node, m *janusv1alpha1.ConsulManaged) (*janusv1alpha1.ConsulManagedSetResponse, error)
}

type grpcConsulNodes struct{}

func (grpcConsulNodes) ManagedGet(ctx context.Context, n *store.Node) (*janusv1alpha1.ConsulManagedGetResponse, error) {
	conn, err := nodeproxy.Conn(n)
	if err != nil {
		return nil, err
	}
	return janusv1alpha1.NewNetworkServiceClient(conn).ConsulManagedGet(nodeproxy.WithUser(ctx, nodeproxy.ConsulUser), &emptypb.Empty{})
}

func (grpcConsulNodes) ManagedSet(ctx context.Context, n *store.Node, m *janusv1alpha1.ConsulManaged) (*janusv1alpha1.ConsulManagedSetResponse, error) {
	conn, err := nodeproxy.Conn(n)
	if err != nil {
		return nil, err
	}
	return janusv1alpha1.NewNetworkServiceClient(conn).ConsulManagedSet(nodeproxy.WithUser(ctx, nodeproxy.ConsulUser), m)
}

type consulMgmt struct {
	store *consulctl.Store
	nodes consulNodes
	kick  chan struct{}
	pass  sync.Mutex // one pass at a time

	mu         sync.Mutex
	state      map[string]consulNodeState // the last pass's, by node ID
	cluster    consulctl.Cluster
	clusterErr string
	lastPass   time.Time
}

// consulNodeState is how a node stands with the Controller's Consul.
type consulNodeState struct {
	// State: "applied" (the node has the layer), "not picked" (the
	// selector doesn't pick it), "removed" (no longer picked: the layer
	// taken off, its token deleted), "no extension" (its image has no
	// consul), "unsupported" (a release without the managed layer),
	// "another controller" (another Controller's layer), "error",
	// "off" (the integration is off), "waiting" (not seen yet).
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// NodeName is its node name in Consul.
	NodeName string `json:"node_name,omitempty"`
	// TokenAccessor is its token's accessor ID - not a secret.
	TokenAccessor string `json:"token_accessor,omitempty"`
	// Member is its agent's state in the LAN pool: alive, failed, left,
	// "" when it isn't a member.
	Member string `json:"member,omitempty"`
	// Services and URLs are its services in the catalog, its haproxy-url
	// instances among them.
	Services int       `json:"services"`
	URLs     int       `json:"urls"`
	Checked  time.Time `json:"checked,omitzero"`
	// Pushed: this pass gave it its layer - the next one, soon, sees its
	// agent join and its services come.
	Pushed bool `json:"-"`
}

// Poke has the nodes' layers checked now.
func (c *consulMgmt) Poke() {
	if c == nil {
		return
	}
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (a *app) startConsul(key consulctl.Sealer) error {
	st, err := consulctl.Open(filepath.Join(a.dataDir, "consul"), key)
	if err != nil {
		return err
	}
	a.consul = &consulMgmt{store: st, nodes: grpcConsulNodes{}, kick: make(chan struct{}, 1), state: map[string]consulNodeState{}}
	go a.consulLoop(context.Background())
	return nil
}

func (a *app) consulLoop(ctx context.Context) {
	for {
		wait := consulEvery
		if a.consulPass(ctx) {
			wait = consulRetry
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-a.consul.kick:
			t.Stop()
		}
	}
}

// consulName is a node's name in Consul: the Controller's, with what a
// node name can't hold as '-'.
func consulName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := strings.TrimLeft(b.String(), "._-")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

var metaKeyCleaner = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// consulMeta is the node's metadata in Consul: its labels ('.' and '/'
// as '_'), its ID and its cluster's name.
func (a *app) consulMeta(n *store.Node, set consulctl.Settings) map[string]string {
	meta := map[string]string{"janus_node_id": n.ID}
	if set.NodeMetaFromLabels {
		for k, v := range a.nodeLabels(n) {
			k = metaKeyCleaner.ReplaceAllString(k, "_")
			if strings.HasPrefix(k, "consul-") || strings.HasPrefix(k, "janus_") {
				continue
			}
			meta[k] = v
		}
	}
	if c := a.clusterOf(n.ID); c != "" {
		meta["janus_cluster"] = c
	}
	return meta
}

// clusterOf is the name of the cluster node is a member of, "" if none.
func (a *app) clusterOf(node string) string {
	if a.clusters == nil || a.clusters.Engine == nil || a.clusters.Store == nil {
		return ""
	}
	for _, c := range a.clusters.Store.List() {
		for _, m := range c.Members {
			if m.NodeID == node {
				return c.Name
			}
		}
	}
	return ""
}

// advertiseOf is the address the node's agent advertises: its address at
// the Controller when that's an IP the others can reach, else the node
// picks its main address.
func advertiseOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return ""
	}
	return ip.String()
}

// consulManagedBy is the managed_by of the layer this Controller gives a
// node: who, and a hash of the layer - secrets included, so a new token
// or gossip key is another layer.
func (a *app) consulManagedBy(m *janusv1alpha1.ConsulManaged) string {
	b, _ := json.Marshal(struct {
		Datacenter, NodeName, Advertise, Encrypt, Token, CA string
		Join                                                []string
		Meta                                                map[string]string
		Services                                            [4]bool
	}{m.GetDatacenter(), m.GetNodeName(), m.GetAdvertiseAddr(), m.GetEncrypt(), m.GetAgentToken(), m.GetCaPem(), m.GetRetryJoin(), m.GetNodeMeta(),
		[4]bool{m.GetServices().GetJanus(), m.GetServices().GetExporters(), m.GetServices().GetFrontends(), m.GetServices().GetUrls()}})
	sum := sha256.Sum256(b)
	return consulManagedByPrefix(a.controllerID) + hex.EncodeToString(sum[:4])
}

func consulManagedByPrefix(controllerID string) string {
	id := controllerID
	if len(id) > 8 {
		id = id[:8]
	}
	return "janus-controller " + id + " consul "
}

// tokenDescription is a node's token's description in Consul.
func (a *app) tokenDescription(n *store.Node) string {
	return fmt.Sprintf("janus node %s (%s) - Controller %s", n.Name, n.ID, a.controllerID)
}

// consulPass checks every node; it says whether one couldn't be brought
// to what it should be - worth trying again soon.
func (a *app) consulPass(ctx context.Context) (again bool) {
	c := a.consul
	c.pass.Lock()
	defer c.pass.Unlock()
	set := c.store.Settings()
	nodes := a.store.List()
	view := map[string]consulNodeState{}
	defer func() {
		c.mu.Lock()
		c.state, c.lastPass = view, time.Now()
		c.mu.Unlock()
	}()

	var client *consulctl.Client
	var cl consulctl.Cluster
	var clErr error
	if set.Address != "" && set.Token != "" {
		client, clErr = consulctl.New(set)
		if clErr == nil {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			cl, clErr = client.Cluster(cctx)
			if clErr == nil {
				var id string
				id, clErr = client.EnsurePolicy(cctx)
				if clErr == nil {
					clErr = c.store.SetPolicyID(id)
				}
			}
			cancel()
		}
	}
	c.mu.Lock()
	c.cluster, c.clusterErr = cl, ""
	if clErr != nil {
		c.clusterErr = clErr.Error()
	}
	c.mu.Unlock()
	if clErr != nil {
		log.Printf("consul: %v", clErr)
		again = true
	}
	reachable := client != nil && clErr == nil

	// The members and the names each node has in Consul.
	var members map[string]consulctl.Member
	if reachable {
		mctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		members, _ = client.Members(mctx)
		cancel()
	}
	names := map[string]string{} // Consul node name -> node ID, the first
	for _, n := range nodes {
		name := consulName(n.Name)
		if _, taken := names[name]; !taken {
			names[name] = n.ID
		}
	}

	for _, n := range nodes {
		nctx, cancel := context.WithTimeout(ctx, consulNodeLimit)
		st := a.consulNode(nctx, n, set, client, cl, reachable, names)
		if reachable && st.NodeName != "" {
			if m, ok := members[st.NodeName]; ok {
				st.Member = m.Status
			}
			if svcs, err := client.NodeServices(nctx, st.NodeName); err == nil {
				st.Services = len(svcs)
				for _, s := range svcs {
					if s.Service == "haproxy-url" {
						st.URLs++
					}
				}
			}
		}
		cancel()
		st.Checked = time.Now().UTC()
		if st.State == "error" || st.State == "unsupported" || st.Pushed {
			again = true
		}
		view[n.ID] = st
	}

	// The nodes gone: their tokens deleted, their entries deregistered.
	present := map[string]bool{}
	for _, n := range nodes {
		present[n.ID] = true
	}
	for _, id := range c.store.Nodes() {
		tok, _ := c.store.Token(id)
		if present[id] && !tok.Removed {
			continue
		}
		if !reachable {
			again = true
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := a.dropConsulToken(dctx, client, id, tok)
		cancel()
		if err != nil {
			log.Printf("consul: node %s: %v", id, err)
			again = true
		}
	}
	return again
}

// dropConsulToken deletes a node's token and its entry in the catalog,
// then forgets them.
func (a *app) dropConsulToken(ctx context.Context, client *consulctl.Client, id string, tok consulctl.NodeToken) error {
	if err := client.DeleteToken(ctx, tok.AccessorID); err != nil {
		return fmt.Errorf("delete its token: %w", err)
	}
	if err := client.Deregister(ctx, tok.Datacenter, tok.NodeName); err != nil {
		return fmt.Errorf("deregister %s: %w", tok.NodeName, err)
	}
	log.Printf("consul: node %s (%s): its token deleted, deregistered from the catalog", tok.NodeName, id)
	return a.consul.store.Forget(id)
}

// consulNode brings node to what the settings say of it.
func (a *app) consulNode(ctx context.Context, n *store.Node, set consulctl.Settings, client *consulctl.Client, cl consulctl.Cluster, reachable bool, names map[string]string) consulNodeState {
	c := a.consul
	name := consulName(n.Name)
	st := consulNodeState{NodeName: name}
	tok, hasTok := c.store.Token(n.ID)
	if hasTok && tok.Removed {
		// Leaving the Controller (consulNodeRemoved): its layer is off,
		// its token goes with this pass - nothing given back meanwhile.
		return consulNodeState{State: "removed", NodeName: name}
	}
	if hasTok {
		st.TokenAccessor = tok.AccessorID
	}
	cur, err := c.nodes.ManagedGet(ctx, n)
	// A release without the managed layer has none to take off: it says
	// so only when it should have one - else it's off or not picked.
	old := status.Code(err) == codes.Unimplemented
	if old && set.Enabled && set.Picks(a.nodeLabels(n)) {
		return consulNodeState{State: "unsupported", Error: "this node's release can't take a managed Consul layer: update it"}
	}
	if err != nil && !old {
		st.State, st.Error = "error", status.Convert(err).Message()
		return st
	}
	if !old && cur.GetState() == janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
		st.State = "no extension"
	}
	have := cur.GetManaged().GetManagedBy()
	mine := strings.HasPrefix(have, consulManagedByPrefix(a.controllerID))
	if have != "" && !mine {
		return consulNodeState{State: "another controller", Error: "its layer is " + have}
	}
	wanted := set.Enabled && st.State != "no extension" && set.Picks(a.nodeLabels(n))

	if !wanted {
		if mine {
			if _, err := c.nodes.ManagedSet(ctx, n, &janusv1alpha1.ConsulManaged{}); err != nil {
				st.State, st.Error = "error", "taking the layer off: "+status.Convert(err).Message()
				return st
			}
			log.Printf("node %s (%s): its Consul layer taken off", n.Name, n.ID)
		}
		if hasTok {
			if !reachable {
				st.State, st.Error = "error", "its token can't be deleted: Consul isn't reachable"
				return st
			}
			if err := a.dropConsulToken(ctx, client, n.ID, tok); err != nil {
				st.State, st.Error = "error", err.Error()
				return st
			}
			st.TokenAccessor = ""
			mine = true
		}
		switch {
		case st.State == "no extension":
		case !set.Enabled:
			st.State = "off"
		case mine:
			st.State = "removed"
		default:
			st.State = "not picked"
		}
		return st
	}
	if !reachable {
		st.State, st.Error = "error", "Consul isn't reachable"
		return st
	}
	if names[name] != n.ID {
		st.State, st.Error = "error", fmt.Sprintf("another node is named %s in Consul: node names must differ", name)
		return st
	}

	// Its token: made once, made again if deleted in Consul, brought back
	// to what it should be if changed (renamed node: another identity).
	desc := a.tokenDescription(n)
	if hasTok && (tok.Server != set.Address || tok.Datacenter != cl.Datacenter) {
		_ = client.DeleteToken(ctx, tok.AccessorID) // another cluster's, likely gone with it
		hasTok = false
	}
	if hasTok {
		ts, err := client.CheckToken(ctx, tok.AccessorID, desc, name, cl.Datacenter)
		switch {
		case err != nil:
			st.State, st.Error = "error", "its token: "+err.Error()
			return st
		case ts == consulctl.TokenMissing:
			hasTok = false
		case ts == consulctl.TokenDiffers:
			if err := client.UpdateToken(ctx, tok.AccessorID, desc, name, cl.Datacenter); err != nil {
				st.State, st.Error = "error", "its token: "+err.Error()
				return st
			}
			if tok.NodeName != name {
				_ = client.Deregister(ctx, tok.Datacenter, tok.NodeName) // its old name
			}
			tok.NodeName = name
			if err := c.store.SetToken(n.ID, tok); err != nil {
				st.State, st.Error = "error", err.Error()
				return st
			}
		}
	}
	if !hasTok {
		acc, secret, err := client.CreateToken(ctx, desc, name, cl.Datacenter)
		if err != nil {
			st.State, st.Error = "error", "its token: "+err.Error()
			return st
		}
		tok = consulctl.NodeToken{NodeName: name, Datacenter: cl.Datacenter, AccessorID: acc, SecretID: secret, Server: set.Address, Created: time.Now().UTC()}
		if err := c.store.SetToken(n.ID, tok); err != nil {
			_ = client.DeleteToken(ctx, acc)
			st.State, st.Error = "error", err.Error()
			return st
		}
		log.Printf("node %s (%s): its Consul token made (accessor %s)", n.Name, n.ID, acc)
	}
	st.TokenAccessor = tok.AccessorID

	want := &janusv1alpha1.ConsulManaged{
		Datacenter: cl.Datacenter, NodeName: name, RetryJoin: cl.Servers, AdvertiseAddr: advertiseOf(n.Addr()),
		Encrypt: cl.GossipKey, AgentToken: tok.SecretID, CaPem: set.RPCCAPEM, NodeMeta: a.consulMeta(n, set),
		Services: &janusv1alpha1.ConsulManagedServices{Janus: set.Services.Janus, Exporters: set.Services.Exporters,
			Frontends: set.Services.Frontends, Urls: set.Services.URLs},
	}
	want.ManagedBy = a.consulManagedBy(want)
	if have == want.ManagedBy {
		st.State = "applied"
		return st
	}
	resp, err := c.nodes.ManagedSet(ctx, n, want)
	if err != nil {
		st.State, st.Error = "error", status.Convert(err).Message()
		return st
	}
	if !resp.GetAccepted() {
		st.State, st.Error = "error", "the node refused the layer: "+strings.Join(resp.GetErrors(), "; ")
		return st
	}
	log.Printf("node %s (%s): Consul layer %s applied", n.Name, n.ID, want.ManagedBy)
	st.State, st.Pushed = "applied", true
	return st
}

// consulNodeRemoved is a node leaving the Controller: its layer taken off
// while it's still reachable (a few seconds at most), its token deleted
// and its entry deregistered by the next pass.
func (a *app) consulNodeRemoved(n *store.Node) {
	c := a.consul
	if c == nil {
		return
	}
	if _, ok := c.store.Token(n.ID); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cur, err := c.nodes.ManagedGet(ctx, n); err == nil && strings.HasPrefix(cur.GetManaged().GetManagedBy(), consulManagedByPrefix(a.controllerID)) {
		if _, err := c.nodes.ManagedSet(ctx, n, &janusv1alpha1.ConsulManaged{}); err != nil {
			log.Printf("node %s (%s): its Consul layer stays: %v", n.Name, n.ID, err)
		}
	}
	if err := c.store.MarkRemoved(n.ID); err != nil {
		log.Printf("consul: node %s: %v", n.ID, err)
	}
	c.Poke()
}

// --- API ---

func (a *app) registerConsulRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/consul", a.gate(auth.Admin, auth.Admin, a.handleConsul))
	mux.HandleFunc("PUT /api/consul", a.sessionGate(auth.Admin, auth.Admin, a.handleConsulSet))
	mux.HandleFunc("POST /api/consul/test", a.sessionGate(auth.Admin, auth.Admin, a.handleConsulTest))
	mux.HandleFunc("POST /api/consul/run", a.gate(auth.Admin, auth.Admin, a.handleConsulRun))
	mux.HandleFunc("GET /api/consul/nodes", a.gate(auth.Admin, auth.Admin, a.handleConsulNodes))
}

// consulSettingsView is the settings as the API shows and takes them:
// the token never comes back (has_token), the selector as
// "key=value,...".
type consulSettingsView struct {
	Enabled            bool              `json:"enabled"`
	Address            string            `json:"address"`
	TLS                consulctl.Trust   `json:"tls"`
	Token              string            `json:"token,omitempty"`
	HasToken           bool              `json:"has_token"`
	RPCCAPEM           string            `json:"rpc_ca_pem"`
	Selector           string            `json:"selector"`
	Services           consulctl.Toggles `json:"services"`
	NodeMetaFromLabels bool              `json:"node_meta_from_labels"`
}

type consulView struct {
	Settings consulSettingsView `json:"settings"`
	// Cluster is what the last pass read of Consul; Error why it
	// couldn't.
	Cluster  *consulctl.Cluster `json:"cluster,omitempty"`
	Error    string             `json:"error,omitempty"`
	LastPass time.Time          `json:"last_pass,omitzero"`
	Policy   string             `json:"policy"`
}

func viewConsulSettings(s consulctl.Settings) consulSettingsView {
	return consulSettingsView{Enabled: s.Enabled, Address: s.Address, TLS: s.TLS, HasToken: s.Token != "", RPCCAPEM: s.RPCCAPEM,
		Selector: labels.String(s.Selector), Services: s.Services, NodeMetaFromLabels: s.NodeMetaFromLabels}
}

func (v consulSettingsView) settings() (consulctl.Settings, error) {
	sel, err := labels.Parse(v.Selector)
	if err != nil {
		return consulctl.Settings{}, fmt.Errorf("selector: %w", err)
	}
	return consulctl.Settings{Enabled: v.Enabled, Address: v.Address, TLS: v.TLS, Token: v.Token, RPCCAPEM: v.RPCCAPEM,
		Selector: sel, Services: v.Services, NodeMetaFromLabels: v.NodeMetaFromLabels}, nil
}

func (a *app) consulView() consulView {
	c := a.consul
	v := consulView{Settings: viewConsulSettings(c.store.Settings()), Policy: consulctl.PolicyName}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cluster.Datacenter != "" {
		cl := c.cluster
		v.Cluster = &cl
	}
	v.Error, v.LastPass = c.clusterErr, c.lastPass
	return v
}

func (a *app) handleConsul(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.consulView())
}

func (a *app) handleConsulSet(w http.ResponseWriter, r *http.Request) {
	var req consulSettingsView
	if !decodeBody(w, r, &req) {
		return
	}
	next, err := req.settings()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := a.consul.store.Set(next); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.consul.Poke()
	writeJSON(w, http.StatusOK, a.consulView())
}

// handleConsulTest tests the settings as the page shows them, saved or
// not - the token they leave out is the saved one, for the same
// address: the server reached, its cluster read, the token's rights.
func (a *app) handleConsulTest(w http.ResponseWriter, r *http.Request) {
	var req consulSettingsView
	if !decodeBody(w, r, &req) {
		return
	}
	draft, err := req.settings()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	draft.MergeSecrets(a.consul.store.Settings())
	draft.Enabled = true
	if errs := draft.Validate(); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, strings.Join(errs, "; "))
		return
	}
	client, err := consulctl.New(draft)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cl, checks := client.Test(ctx)
	ok := !slices.ContainsFunc(checks, func(c consulctl.Check) bool { return !c.OK })
	out := map[string]any{"ok": ok, "checks": checks}
	if cl.Datacenter != "" {
		out["cluster"] = cl
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) handleConsulRun(w http.ResponseWriter, _ *http.Request) {
	a.consul.Poke()
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

type consulNodeView struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	consulNodeState
}

func (a *app) handleConsulNodes(w http.ResponseWriter, _ *http.Request) {
	c := a.consul
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []consulNodeView{}
	for _, n := range a.store.List() {
		st, ok := c.state[n.ID]
		if !ok {
			st = consulNodeState{State: "waiting"}
		}
		out = append(out, consulNodeView{ID: n.ID, Name: n.Name, Labels: a.nodeLabels(n), consulNodeState: st})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}
