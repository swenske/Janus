package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme"
	"github.com/swenske/Janus/internal/clusters"
)

// A cluster's Let's Encrypt (docs/guide/clusters.md "Let's Encrypt"):
// the configuration the member that orders runs - the cluster's own once
// it manages the domain acme -, each member's state, and the Controller's
// relay and ordering for the page's "Through this Controller" and
// "Ordered by this Controller". PUT makes a configuration the cluster's
// (clusters.Engine.ConfigureACME): its secrets go to the orderer, never
// into a version nor this Controller's data.

// clusterACMEMember is a member's letsencrypt extension as the page shows
// it.
type clusterACMEMember struct {
	Name string `json:"name"`
	// State: running, not_enabled (no extension in its image)...
	State string `json:"state,omitempty"`
	// PathMode: how it reaches the CA - direct, proxy, controller_relay,
	// controller_ordered.
	PathMode     string            `json:"path_mode,omitempty"`
	OrderedBy    string            `json:"ordered_by,omitempty"`
	Configured   bool              `json:"configured"`
	Thumbprint   string            `json:"thumbprint,omitempty"`
	Certificates []json.RawMessage `json:"certificates"`
	Error        string            `json:"error,omitempty"`
}

type clusterACMEView struct {
	// Managed: the cluster manages Let's Encrypt's configuration - the
	// same on every member, locked there.
	Managed bool   `json:"managed"`
	Orderer string `json:"orderer"`
	// Config: the orderer's configuration, its secrets empty; IsDefault:
	// it has none saved.
	Config    json.RawMessage `json:"config,omitempty"`
	IsDefault bool            `json:"is_default"`
	// Status: the orderer's ACMEStatus, as a node page reads it.
	Status     json.RawMessage     `json:"status,omitempty"`
	Error      string              `json:"error,omitempty"`
	Members    []clusterACMEMember `json:"members"`
	Controller struct {
		Relay    nodeproxy.RelayInfo    `json:"relay"`
		Ordering nodeproxy.OrderingInfo `json:"ordering"`
	} `json:"controller"`
}

var acmeJSON = protojson.MarshalOptions{UseProtoNames: true}

// lowerEnum is an enum's name without its prefix, lowercased.
func lowerEnum(name, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(name, prefix))
}

// acmeStatusJSON is st as a node page's /api/haproxy/acme gives it.
func acmeStatusJSON(st *janusv1alpha1.ACMEStatusResponse) (json.RawMessage, error) {
	data, err := acmeJSON.Marshal(st)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	out["state"] = lowerEnum(st.GetState().String(), "MODULE_STATE_")
	out["path_mode"] = lowerEnum(st.GetPathMode().String(), "ACME_PATH_MODE_")
	return json.Marshal(out)
}

func (a *app) handleClusterACME(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(nodeproxy.ForCluster(r.Context(), c.ID), 20*time.Second)
	defer cancel()
	v := clusterACMEView{Managed: c.Manages("acme"), Orderer: c.SecretsFrom, Members: make([]clusterACMEMember, len(c.Members))}
	if nodeproxy.ACMERelay != nil {
		v.Controller.Relay = nodeproxy.ACMERelay()
	}
	if nodeproxy.ACMEOrdering != nil {
		v.Controller.Ordering = nodeproxy.ACMEOrdering()
	}
	var wg sync.WaitGroup
	for i, m := range c.Members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := clusterACMEMember{Name: m.Name, Certificates: []json.RawMessage{}}
			defer func() { v.Members[i] = out }()
			st, err := a.clusters.Nodes.ACMEStatus(ctx, m)
			if err != nil {
				out.Error = err.Error()
				return
			}
			out.State = lowerEnum(st.GetState().String(), "MODULE_STATE_")
			out.PathMode = lowerEnum(st.GetPathMode().String(), "ACME_PATH_MODE_")
			out.OrderedBy, out.Configured, out.Thumbprint = st.GetOrderedBy(), st.GetConfigured(), st.GetAccountThumbprint()
			for _, cs := range st.GetCertificates() {
				if b, err := acmeJSON.Marshal(cs); err == nil {
					out.Certificates = append(out.Certificates, b)
				}
			}
			if m.Name != c.SecretsFrom {
				return
			}
			if v.Status, err = acmeStatusJSON(st); err != nil {
				v.Error = err.Error()
				return
			}
			if st.GetState() != janusv1alpha1.ModuleState_MODULE_STATE_RUNNING {
				return
			}
			cfg, err := a.clusters.Nodes.ACMEConfig(ctx, m)
			if err != nil {
				v.Error = m.Name + ": " + err.Error()
				return
			}
			v.IsDefault = !st.GetConfigured()
			if v.Config, err = acme.Marshal(cfg); err != nil {
				v.Error = err.Error()
			}
		}()
	}
	wg.Wait()
	if c.Member(c.SecretsFrom) == nil {
		v.Error = "the cluster has no member the secrets come from: the one that orders"
	}
	writeJSON(w, http.StatusOK, v)
}

// clusterACMERequest is a configuration for the cluster: JSON as a node
// page sends it - a secret left empty keeps the orderer's.
type clusterACMERequest struct {
	Config json.RawMessage `json:"config"`
	Note   string          `json:"note"`
}

func (req clusterACMERequest) parse() (*janusv1alpha1.ACMEConfig, error) {
	if len(req.Config) == 0 {
		return &janusv1alpha1.ACMEConfig{}, nil
	}
	return acme.ParseConfig(req.Config)
}

// handleClusterACMECheck checks a configuration on the member that orders,
// changing nothing: {accepted, errors}.
func (a *app) handleClusterACMECheck(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var req clusterACMERequest
	if !decodeBody(w, r, &req) {
		return
	}
	cfg, err := req.parse()
	if err != nil {
		writeError(w, http.StatusBadRequest, "config: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.checkClusterACME(r.Context(), c, cfg))
}

type acmeCheck struct {
	Accepted bool     `json:"accepted"`
	Errors   []string `json:"errors,omitempty"`
}

func (a *app) checkClusterACME(ctx context.Context, c *clusters.Cluster, cfg *janusv1alpha1.ACMEConfig) acmeCheck {
	o := c.Member(c.SecretsFrom)
	if o == nil {
		return acmeCheck{Errors: []string{"the cluster has no member the secrets come from: the one that orders"}}
	}
	cctx, cancel := context.WithTimeout(nodeproxy.ForCluster(ctx, c.ID), 30*time.Second)
	defer cancel()
	if err := a.clusters.Nodes.ACMEApply(cctx, *o, cfg, true); err != nil {
		return acmeCheck{Errors: strings.Split(strings.TrimPrefix(err.Error(), "letsencrypt.json: "), "; ")}
	}
	return acmeCheck{Accepted: true}
}

// handleClusterACMESet makes a configuration the cluster's: checked on
// the orderer first (400 with the node's errors), then given to it and
// carried by the cluster's next version, in the background (202).
func (a *app) handleClusterACMESet(w http.ResponseWriter, r *http.Request) {
	c, ok := a.clusterOr404(w, r)
	if !ok {
		return
	}
	var req clusterACMERequest
	if !decodeBody(w, r, &req) {
		return
	}
	cfg, err := req.parse()
	if err != nil {
		writeError(w, http.StatusBadRequest, "config: "+err.Error())
		return
	}
	if a.clusters.isBusy(c.ID) {
		writeError(w, http.StatusConflict, "an operation runs on cluster "+c.Name+": wait for it to end")
		return
	}
	if chk := a.checkClusterACME(r.Context(), c, cfg); !chk.Accepted {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": c.SecretsFrom + " refused the configuration: " + strings.Join(chk.Errors, "; "), "errors": chk.Errors})
		return
	}
	actor := requestPrincipal(r).User
	a.runCluster(w, c, "configuring Let's Encrypt", actor, func(ctx context.Context) error {
		_, err := a.clusters.ConfigureACME(ctx, c.ID, cfg, req.Note, actor)
		return err
	})
}
