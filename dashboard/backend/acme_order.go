package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/acmeorder"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme"
	"github.com/swenske/Janus/internal/acme/acmewire"
)

// The certificates the Controller orders for its nodes (path
// CONTROLLER_ORDERED, internal/acmeorder, docs/guide/
// letsencrypt-controller.md "Ordering for the nodes"): every 10 minutes,
// or when kicked, each node whose page chose "Ordered by this
// Controller" is asked which of its certificates are due; for each, the
// node makes a key and its request, janus-acme orders it from the CA the
// node's configuration names - with the Controller's account and DNS
// providers - and the node installs what comes back.

const (
	acmeOrderEvery   = 10 * time.Minute
	acmeIssueTimeout = 15 * time.Minute
	acmeNodeTimeout  = 30 * time.Second
)

type acmeOrdering struct {
	store  *acmeorder.Store
	client string // janus-acme
	kick   chan struct{}

	mu       sync.Mutex
	nodes    map[string]orderedNode // the last pass's, by node ID
	lastPass time.Time
}

// orderedNode is a node the Controller orders for, as the last pass saw
// it.
type orderedNode struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"` // "ordered", "another account", "error"
	Error string `json:"error,omitempty"`
	// Directory is the CA the node's configuration names.
	Directory    string        `json:"directory,omitempty"`
	Certificates []orderedCert `json:"certificates"`
}

type orderedCert struct {
	Name        string          `json:"name"`
	Domains     []string        `json:"domains"`
	Challenge   string          `json:"challenge"`
	DNSProvider string          `json:"dns_provider,omitempty"`
	State       string          `json:"state"` // the node's: pending, valid, due, expired
	NotAfter    int64           `json:"not_after_unix,omitempty"`
	Asked       bool            `json:"asked,omitempty"` // a renewal asked for on the node
	Order       acmeorder.Order `json:"order"`
}

// clientPath is where janus-acme is: next to dashboardd (the image's
// /janus-acme, make dashboard-bin's bin/janus-acme).
func clientPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "janus-acme")
	}
	return "/janus-acme"
}

func (a *app) startACMEOrdering(key acmeorder.Sealer, client string) error {
	st, err := acmeorder.Open(filepath.Join(a.dataDir, "acme"), key)
	if err != nil {
		return err
	}
	if client == "" {
		client = clientPath()
	}
	a.ordering = &acmeOrdering{store: st, client: client, kick: make(chan struct{}, 1), nodes: map[string]orderedNode{}}
	nodeproxy.ACMEChanged = a.ordering.poke
	nodeproxy.ACMEOrdering = a.orderingInfo
	go a.acmeOrderLoop(context.Background())
	return nil
}

func (o *acmeOrdering) poke() {
	select {
	case o.kick <- struct{}{}:
	default:
	}
}

func (a *app) acmeOrderLoop(ctx context.Context) {
	for {
		a.acmeOrderPass(ctx)
		t := time.NewTimer(acmeOrderEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-a.ordering.kick:
			t.Stop()
		}
	}
}

// acmeOrderPass orders what's due on every node ordered by this
// Controller.
func (a *app) acmeOrderPass(ctx context.Context) {
	ord := a.ordering
	set := ord.store.Settings()
	thumb := ord.store.Thumbprint()
	view := map[string]orderedNode{}
	recorded := map[string]bool{}
	for _, id := range ord.store.Nodes() {
		recorded[id] = true
	}
	for _, n := range a.store.List() {
		node, ordered := a.orderFor(ctx, n, set, thumb)
		switch {
		case ordered:
			view[n.ID] = node
		case recorded[n.ID] && node.State == "error":
			view[n.ID] = node // was ordered for: say why it can't be seen now
		case recorded[n.ID]:
			if err := ord.store.Forget(n.ID, nil); err != nil {
				log.Printf("lets encrypt: %v", err)
			}
		}
	}
	ord.mu.Lock()
	ord.nodes, ord.lastPass = view, time.Now()
	ord.mu.Unlock()
}

// orderFor orders node n's certificates that are due; ordered says
// whether the node is one the Controller orders for.
func (a *app) orderFor(ctx context.Context, n *store.Node, set acmeorder.Settings, thumb string) (orderedNode, bool) {
	node := orderedNode{ID: n.ID, Name: n.Name, State: "ordered"}
	conn, err := nodeproxy.Conn(n)
	if err != nil {
		node.State, node.Error = "error", err.Error()
		return node, false
	}
	c := janusv1alpha1.NewHAProxyServiceClient(conn)
	call := func() (context.Context, context.CancelFunc) {
		cctx, cancel := context.WithTimeout(ctx, acmeNodeTimeout)
		return nodeproxy.WithUser(cctx, nodeproxy.ACMEOrderingUser), cancel
	}
	cctx, cancel := call()
	st, err := c.ACMEStatus(cctx, &emptypb.Empty{})
	cancel()
	if err != nil {
		node.State, node.Error = "error", status.Convert(err).Message()
		return node, false
	}
	if st.GetPathMode() != janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_CONTROLLER_ORDERED || st.GetOrderedBy() != "" {
		return node, false // not ordered by the Controller; or a cluster member another member's certificates are pulled to
	}
	node.Directory = st.GetDirectory()
	if st.GetAccountThumbprint() != thumb {
		node.State = "another account"
		node.Error = "the node answers HTTP-01 for another account (" + st.GetAccountThumbprint() + "): choose \"Ordered by this Controller\" again on its page"
		return node, true
	}
	cctx, cancel = call()
	got, err := c.ACMEGetConfig(cctx, &emptypb.Empty{})
	cancel()
	if err != nil {
		node.State, node.Error = "error", status.Convert(err).Message()
		return node, true
	}
	cfg := got.GetConfig()
	now := time.Now()
	var names []string
	for _, cs := range st.GetCertificates() {
		cert := certificateNamed(cfg, cs.GetName())
		if cert == nil {
			continue
		}
		names = append(names, cert.Name)
		oc := orderedCert{
			Name: cert.Name, Domains: cert.Domains, Challenge: cert.Challenge, DNSProvider: cert.DnsProvider,
			State: cs.GetState(), NotAfter: cs.GetNotAfterUnix(), Asked: cs.GetInProgress(),
		}
		o := a.ordering.store.Order(n.ID, cert.Name)
		due := cs.GetState() != "valid" || cs.GetInProgress()
		if due && set.AcceptTerms && !now.Before(o.NextAttempt) {
			before := o.LastSuccess
			o = a.orderOne(ctx, n, c, call, cfg, cert, set, o)
			if o.LastSuccess.After(before) { // installed: what the node says now
				oc.State, oc.Asked, oc.NotAfter = "valid", false, o.NotAfter.Unix()
			}
		}
		oc.Order = o
		node.Certificates = append(node.Certificates, oc)
	}
	if err := a.ordering.store.Forget(n.ID, names); err != nil {
		log.Printf("lets encrypt: %v", err)
	}
	return node, true
}

func certificateNamed(cfg *janusv1alpha1.ACMEConfig, name string) *janusv1alpha1.ACMECertificate {
	for _, c := range cfg.GetCertificates() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// orderOne orders one certificate for node n: its request, the order,
// the chain installed - or why not, told to the node too.
func (a *app) orderOne(ctx context.Context, n *store.Node, c janusv1alpha1.HAProxyServiceClient, call func() (context.Context, context.CancelFunc),
	cfg *janusv1alpha1.ACMEConfig, cert *janusv1alpha1.ACMECertificate, set acmeorder.Settings, o acmeorder.Order) acmeorder.Order {
	now := time.Now()
	where := fmt.Sprintf("node %s, certificate %s", n.Name, cert.Name)
	fail := func(msg string) acmeorder.Order {
		o.Failed(msg, now)
		log.Printf("lets encrypt: %s: %s - next attempt %s", where, msg, o.NextAttempt.Format(time.RFC3339))
		cctx, cancel := call()
		_, err := c.ACMEInstallCertificate(cctx, &janusv1alpha1.ACMEInstallCertificateRequest{Name: cert.Name, Error: msg, NextAttemptUnix: o.NextAttempt.Unix()})
		cancel()
		if err != nil {
			log.Printf("lets encrypt: %s: tell the node: %v", where, status.Convert(err).Message())
		}
		if err := a.ordering.store.SetOrder(n.ID, cert.Name, o); err != nil {
			log.Printf("lets encrypt: %v", err)
		}
		return o
	}
	cctx, cancel := call()
	csr, err := c.ACMECertificateRequest(cctx, &janusv1alpha1.ACMECertificateRequestRequest{Name: cert.Name})
	cancel()
	if err != nil {
		return fail("the node's request: " + status.Convert(err).Message())
	}
	acc := cfg.GetAccount()
	req := &acmewire.Request{
		Action: acmewire.ActionIssue, Directory: acme.ResolveDirectory(acc.GetDirectory()), DirectoryCA: acc.GetDirectoryCa(),
		AccountKey: string(a.ordering.store.AccountKey()), Email: set.Email, AcceptTerms: set.AcceptTerms,
		EABKeyID: set.EABKeyID, EABHMACKey: set.EABHMACKey,
		Domains: cert.Domains, KeyType: cert.KeyType, Challenge: cert.Challenge, Profile: cert.Profile, CSR: csr.GetCsrPem(),
	}
	if cert.Challenge == acmewire.ChallengeDNS01 {
		p, ok := set.Provider(cert.DnsProvider)
		if !ok {
			return fail(fmt.Sprintf("the Controller has no DNS provider named %q - add it on its Let's Encrypt tab", cert.DnsProvider))
		}
		req.DNSProvider, req.DNSEnv, req.DNSResolvers = p.Type, p.Settings, p.Resolvers
		req.DNSPropagationWait = time.Duration(p.PropagationWaitSeconds) * time.Second
	}
	log.Printf("lets encrypt: %s: ordering it (%s, %s)", where, cert.Challenge, strings.Join(cert.Domains, ", "))
	res, err := acmeorder.RunClient(a.ordering.client, req, acmeIssueTimeout, prefixWriter{prefix: "lets encrypt: janus-acme: "})
	if err != nil {
		return fail(err.Error())
	}
	cctx, cancel = call()
	st, err := c.ACMEInstallCertificate(cctx, &janusv1alpha1.ACMEInstallCertificateRequest{Name: cert.Name, ChainPem: res.Certificate})
	cancel()
	if err != nil {
		return fail("the node refused what the CA issued: " + status.Convert(err).Message())
	}
	o.Succeeded(st.GetSerial(), time.Unix(st.GetNotAfterUnix(), 0), now)
	log.Printf("lets encrypt: %s: ordered and installed - serial %s, valid until %s", where, st.GetSerial(), time.Unix(st.GetNotAfterUnix(), 0).UTC().Format(time.RFC3339))
	if err := a.ordering.store.SetOrder(n.ID, cert.Name, o); err != nil {
		log.Printf("lets encrypt: %v", err)
	}
	return o
}

// prefixWriter logs each line written with a prefix.
type prefixWriter struct{ prefix string }

func (w prefixWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			log.Print(w.prefix + line)
		}
	}
	return len(p), nil
}

var _ io.Writer = prefixWriter{}

// orderingView is what the Let's Encrypt tab shows of the ordering.
type orderingView struct {
	Settings   acmeorder.Settings `json:"settings"` // secrets empty
	Thumbprint string             `json:"thumbprint"`
	// Ready: something can be ordered - the CA's terms accepted, the
	// client there.
	Ready    bool          `json:"ready"`
	Client   bool          `json:"client"`
	Nodes    []orderedNode `json:"nodes"`
	LastPass time.Time     `json:"last_pass,omitzero"`
}

func (a *app) orderingView() orderingView {
	ord := a.ordering
	set := ord.store.Settings()
	_, err := os.Stat(ord.client)
	v := orderingView{Settings: set.Redacted(), Thumbprint: ord.store.Thumbprint(), Client: err == nil}
	v.Ready = set.AcceptTerms && v.Client
	ord.mu.Lock()
	for _, n := range ord.nodes {
		v.Nodes = append(v.Nodes, n)
	}
	v.LastPass = ord.lastPass
	ord.mu.Unlock()
	sort.Slice(v.Nodes, func(i, j int) bool { return v.Nodes[i].Name < v.Nodes[j].Name })
	return v
}

// orderingInfo is what a node page needs to choose "Ordered by this
// Controller".
func (a *app) orderingInfo() nodeproxy.OrderingInfo {
	if a.ordering == nil {
		return nodeproxy.OrderingInfo{}
	}
	v := a.orderingView()
	info := nodeproxy.OrderingInfo{Available: true, Ready: v.Ready, Thumbprint: v.Thumbprint}
	for _, p := range v.Settings.DNSProviders {
		info.DNSProviders = append(info.DNSProviders, nodeproxy.NamedProvider{Name: p.Name, Type: p.Type})
	}
	switch {
	case !v.Client:
		info.Why = "this Controller's image has no ACME client (janus-acme)"
	case !v.Settings.AcceptTerms:
		info.Why = "an admin sets ordering up on the Controller's Let's Encrypt tab - its account, the CA's terms"
	}
	return info
}

func (a *app) registerACMEOrderingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/acme", a.gate(auth.Admin, auth.Admin, a.handleACMEOrdering))
	mux.HandleFunc("PUT /api/acme", a.gate(auth.Admin, auth.Admin, a.handleACMEOrderingSet))
	mux.HandleFunc("POST /api/acme/run", a.gate(auth.Admin, auth.Admin, a.handleACMEOrderingRun))
}

func (a *app) handleACMEOrdering(w http.ResponseWriter, r *http.Request) {
	if a.ordering == nil {
		writeError(w, http.StatusServiceUnavailable, "ordering isn't running")
		return
	}
	writeJSON(w, http.StatusOK, a.orderingView())
}

// handleACMEOrderingSet saves the account's settings and the DNS
// providers: secrets sent empty keep the saved ones.
func (a *app) handleACMEOrderingSet(w http.ResponseWriter, r *http.Request) {
	if a.ordering == nil {
		writeError(w, http.StatusServiceUnavailable, "ordering isn't running")
		return
	}
	var set acmeorder.Settings
	if !decodeBody(w, r, &set) {
		return
	}
	if _, err := a.ordering.store.Set(set); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.ordering.poke()
	writeJSON(w, http.StatusOK, a.orderingView())
}

// handleACMEOrderingRun orders what's due now, failed ones included -
// their backoff dropped.
func (a *app) handleACMEOrderingRun(w http.ResponseWriter, r *http.Request) {
	if a.ordering == nil {
		writeError(w, http.StatusServiceUnavailable, "ordering isn't running")
		return
	}
	for _, id := range a.ordering.store.Nodes() {
		if n, ok := a.store.Get(id); ok {
			a.ordering.mu.Lock()
			certs := a.ordering.nodes[id].Certificates
			a.ordering.mu.Unlock()
			for _, c := range certs {
				o := a.ordering.store.Order(n.ID, c.Name)
				if !o.NextAttempt.IsZero() {
					o.NextAttempt = time.Time{}
					if err := a.ordering.store.SetOrder(n.ID, c.Name, o); err != nil {
						writeError(w, http.StatusInternalServerError, err.Error())
						return
					}
				}
			}
		}
	}
	a.ordering.poke()
	w.WriteHeader(http.StatusAccepted)
}
