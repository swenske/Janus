package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/dashboard/backend/internal/labels"
	"github.com/swenske/Janus/dashboard/backend/internal/logforward"
	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/logship"
)

// The Controller's log forwarding (internal/logforward, docs/guide/
// log-forwarding.md): its own log and audit sent to its targets, and the
// fleet profiles applied to the nodes - each node given the first
// profile that picks it by its labels, and kept with it.

type logForwarding struct {
	store *logforward.Store
	ship  *logship.Shipper
	kick  chan struct{}

	mu    sync.Mutex
	nodes map[string]nodeLogState
}

// nodeLogState is how a node stands with the fleet's profiles.
type nodeLogState struct {
	// State: "applied" (the profile is the node's configuration),
	// "unmanaged" (no profile picks it: its own configuration), "removed"
	// (no profile picks it any more: what this Controller set, taken
	// off), "unsupported" (a release without log forwarding), "error".
	State   string    `json:"state"`
	Profile string    `json:"profile,omitempty"`
	Error   string    `json:"error,omitempty"`
	Checked time.Time `json:"checked,omitzero"`
}

// Poke has the profiles applied again now.
func (l *logForwarding) Poke() {
	if l == nil {
		return
	}
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// startLogForwarding opens the settings, starts the Controller's own
// shipper - its log and its audit - and the loop that applies the
// profiles to the nodes.
func (a *app) startLogForwarding(key logforward.Sealer) error {
	dir := filepath.Join(a.dataDir, "logforward")
	st, err := logforward.OpenStore(dir, key)
	if err != nil {
		return err
	}
	ship := logship.New(logship.Options{
		Meta:       logship.Meta{Agent: "dashboardd", AgentVersion: version},
		Feeds:      []logship.Feed{controllerAuditFeed(a.audit)},
		CursorFile: filepath.Join(dir, logship.CursorFile),
	})
	ship.SetNodeID(a.controllerID)
	ship.Apply(st.Settings().Controller)
	a.logfwd = &logForwarding{store: st, ship: ship, kick: make(chan struct{}, 1), nodes: map[string]nodeLogState{}}
	// The Controller's own log: on its stderr as before, and to its
	// targets that take it.
	log.SetOutput(io.MultiWriter(os.Stderr, &logLines{ship: ship}))
	go a.logForwardLoop(context.Background())
	return nil
}

// logLines hands each line of the Controller's log to its shipper.
type logLines struct {
	ship *logship.Shipper

	mu      sync.Mutex
	partial []byte
}

var goTimePrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? `)

func (w *logLines) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.partial = append(w.partial, p...)
			break
		}
		line := string(append(w.partial, p[:i]...))
		w.partial = w.partial[:0]
		w.ship.Offer(logship.Record{Time: time.Now(), Source: logship.SourceController, Service: "dashboardd", Severity: logship.SevInfo, Message: goTimePrefix.ReplaceAllString(line, "")})
		p = p[i+1:]
	}
	return n, nil
}

// controllerAuditFeed is the Controller's audit as a source its targets
// read behind their cursors.
func controllerAuditFeed(l *audit.Log) logship.Feed {
	return logship.Feed{
		Name: logship.SourceAudit,
		Since: func(id uint64) ([]logship.FeedItem, uint64) {
			entries, last := l.Since(id, 500)
			items := make([]logship.FeedItem, 0, len(entries))
			for _, e := range entries {
				items = append(items, logship.FeedItem{ID: e.ID, Record: controllerAuditRecord(e)})
			}
			return items, last
		},
		Latest:  l.Latest,
		Changed: l.Changed,
	}
}

// controllerAuditRecord is an entry of the Controller's audit as a
// record: "PUT /api/log-forwarding 200 by alice (session) from
// 10.0.0.5", its fields as the node's audit names them.
func controllerAuditRecord(e audit.Entry) logship.Record {
	who := or(e.User, "nobody")
	if e.Via != "" {
		who += " (" + e.Via + ")"
	}
	msg := fmt.Sprintf("%s %s %d by %s", e.Method, e.Path, e.Status, who)
	if e.Client != "" {
		msg += " from " + e.Client
	}
	code, sev := "OK", logship.SevNotice
	if e.Status >= 400 {
		code, sev = fmt.Sprintf("HTTP %d", e.Status), logship.SevWarning
	}
	fields := map[string]any{
		"event": map[string]any{"id": e.ID, "type": e.Method + " " + e.Path},
		"audit": map[string]any{"caller": e.User, "via": e.Via, "peer": e.Client, "code": code, "method": e.Method, "path": e.Path, "status": e.Status},
	}
	return logship.Record{Time: e.Time, Source: logship.SourceAudit, Service: "dashboardd", Severity: sev, Message: msg, Fields: fields, ID: fmt.Sprintf("ca-%d", e.ID)}
}

func (a *app) logForwardLoop(ctx context.Context) {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		a.applyLogProfiles(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.logfwd.kick:
		}
	}
}

func (a *app) applyLogProfiles(ctx context.Context) {
	set := a.logfwd.store.Settings()
	for _, n := range a.store.List() {
		nctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		st := a.ensureLogProfile(nctx, n, set)
		cancel()
		st.Checked = time.Now().UTC()
		a.logfwd.mu.Lock()
		a.logfwd.nodes[n.ID] = st
		a.logfwd.mu.Unlock()
	}
}

// ensureLogProfile gives node the profile that picks it - again when
// what it has differs - or takes this Controller's off it when none does
// any more; a node no profile picks keeps its own configuration.
func (a *app) ensureLogProfile(ctx context.Context, n *store.Node, set logforward.Settings) nodeLogState {
	conn, err := nodeproxy.Conn(n)
	if err != nil {
		return nodeLogState{State: "error", Error: err.Error()}
	}
	c := janusv1alpha1.NewSystemServiceClient(conn)
	cur, err := c.LogForwardingGet(ctx, &emptypb.Empty{})
	if status.Code(err) == codes.Unimplemented {
		return nodeLogState{State: "unsupported", Error: "this node's release can't send its logs: update it"}
	}
	if err != nil {
		return nodeLogState{State: "error", Error: status.Convert(err).Message()}
	}
	mine := strings.HasPrefix(cur.GetConfig().GetManagedBy(), logforward.ManagedByPrefix(a.controllerID))
	p, ok := set.ProfileFor(n.LabelSet())
	if !ok {
		if !mine {
			return nodeLogState{State: "unmanaged"}
		}
		if _, err := c.LogForwardingSet(ctx, &janusv1alpha1.LogForwardingConfig{}); err != nil {
			return nodeLogState{State: "error", Error: status.Convert(err).Message()}
		}
		log.Printf("node %s (%s): no log forwarding profile picks it any more - this Controller's taken off", n.Name, n.ID)
		return nodeLogState{State: "removed"}
	}
	want := p.Config
	want.ManagedBy = logforward.ManagedBy(a.controllerID, p)
	if cur.GetConfig().GetManagedBy() == want.ManagedBy && sameConfig(logship.FromProto(cur.GetConfig()), want.Redacted()) {
		return nodeLogState{State: "applied", Profile: p.Name}
	}
	if _, err := c.LogForwardingSet(ctx, logship.ToProto(want)); err != nil {
		return nodeLogState{State: "error", Profile: p.Name, Error: status.Convert(err).Message()}
	}
	log.Printf("node %s (%s): log forwarding profile %s applied", n.Name, n.ID, p.Name)
	return nodeLogState{State: "applied", Profile: p.Name}
}

// sameConfig compares two configurations as JSON: an empty list and none
// are the same.
func sameConfig(a, b logship.Config) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}

// --- API ---

func (a *app) registerLogForwardingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/log-forwarding", a.gate(auth.Admin, auth.Admin, a.handleLogForwarding))
	mux.HandleFunc("PUT /api/log-forwarding", a.gate(auth.Admin, auth.Admin, a.handleLogForwardingSet))
	mux.HandleFunc("POST /api/log-forwarding/test", a.gate(auth.Admin, auth.Admin, a.handleLogForwardingTest))
}

// profileView is a profile as the API shows and takes it: its selector
// as "key=value,...".
type profileView struct {
	Name     string         `json:"name"`
	Selector string         `json:"selector"`
	Config   logship.Config `json:"config"`
	// Secrets are which each target has: api_key, password, client_key.
	Secrets map[string][]string `json:"secrets,omitempty"`
}

type logForwardingView struct {
	Controller        logship.Config         `json:"controller"`
	ControllerSecrets map[string][]string    `json:"controller_secrets,omitempty"`
	ControllerStatus  []logship.TargetStatus `json:"controller_status"`
	Profiles          []profileView          `json:"profiles"`
	Nodes             []nodeLogView          `json:"nodes"`
}

type nodeLogView struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	nodeLogState
}

func secretsOf(c logship.Config) map[string][]string {
	out := map[string][]string{}
	for _, t := range c.Targets {
		for name := range t.HasSecret() {
			out[t.Name] = append(out[t.Name], name)
		}
		sort.Strings(out[t.Name])
	}
	return out
}

func (a *app) logForwardingView() logForwardingView {
	set := a.logfwd.store.Settings()
	v := logForwardingView{
		Controller: set.Controller.Redacted(), ControllerSecrets: secretsOf(set.Controller),
		ControllerStatus: a.logfwd.ship.Status(), Profiles: []profileView{}, Nodes: []nodeLogView{},
	}
	for _, p := range set.Profiles {
		v.Profiles = append(v.Profiles, profileView{Name: p.Name, Selector: labels.String(p.Selector), Config: p.Config.Redacted(), Secrets: secretsOf(p.Config)})
	}
	a.logfwd.mu.Lock()
	defer a.logfwd.mu.Unlock()
	for _, n := range a.store.List() {
		st, ok := a.logfwd.nodes[n.ID]
		if !ok {
			st = nodeLogState{State: "waiting"}
		}
		v.Nodes = append(v.Nodes, nodeLogView{ID: n.ID, Name: n.Name, Labels: n.LabelSet(), nodeLogState: st})
	}
	return v
}

func (a *app) handleLogForwarding(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.logForwardingView())
}

func (a *app) handleLogForwardingSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Controller logship.Config `json:"controller"`
		Profiles   []profileView  `json:"profiles"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	next := logforward.Settings{Controller: req.Controller}
	for _, p := range req.Profiles {
		sel, err := labels.Parse(p.Selector)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("profile %s: %v", p.Name, err))
			return
		}
		next.Profiles = append(next.Profiles, logforward.Profile{Name: p.Name, Selector: sel, Config: p.Config})
	}
	set, err := a.logfwd.store.Set(next)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.logfwd.ship.Apply(set.Controller)
	a.logfwd.Poke()
	writeJSON(w, http.StatusOK, a.logForwardingView())
}

// handleLogForwardingTest sends one record to a target: one of the
// Controller's from the Controller, one of a profile's from a node -
// with the secrets the Controller keeps for it.
func (a *app) handleLogForwardingTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Profile string `json:"profile"`
		Target  string `json:"target"`
		Node    string `json:"node"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	set := a.logfwd.store.Settings()
	cfg := set.Controller
	if req.Profile != "" {
		found := false
		for _, p := range set.Profiles {
			if p.Name == req.Profile {
				cfg, found = p.Config, true
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, "no such profile")
			return
		}
	}
	var target *logship.Target
	for i := range cfg.Targets {
		if cfg.Targets[i].Name == req.Target {
			target = &cfg.Targets[i]
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "no such target")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	result := map[string]any{"ok": true, "detail": "sent"}
	if req.Profile == "" {
		if err := a.logfwd.ship.Test(ctx, *target); err != nil {
			result = map[string]any{"ok": false, "detail": err.Error()}
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	n, ok := a.store.Get(req.Node)
	if !ok {
		writeError(w, http.StatusBadRequest, "a profile's target is tested from a node: node")
		return
	}
	conn, err := nodeproxy.Conn(n)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	resp, err := janusv1alpha1.NewSystemServiceClient(conn).LogForwardingTest(nodeproxy.WithUser(ctx, nodeproxy.Automation), logship.TargetToProto(*target))
	if err != nil {
		writeError(w, http.StatusBadGateway, status.Convert(err).Message())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": resp.GetOk(), "detail": resp.GetDetail()})
}
