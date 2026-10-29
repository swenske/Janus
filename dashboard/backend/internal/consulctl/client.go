package consulctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/httpsclient"
)

// PolicyName is the policy every node's token has, besides its node
// identity (node "<name>" write, service_prefix "" read): the agent's
// own reads, the other members (the node's status lists them), and
// writing the services a node registers.
const PolicyName = "janus-nodes"

// PolicyRules are janus-nodes's rules.
const PolicyRules = `# Made by the Janus Controller: what a Janus node's agent token needs
# besides its node identity - the agent's own reads, and the services
# the node registers (docs/consul.md "The node's services").
agent_prefix "" {
  policy = "read"
}
node_prefix "" {
  policy = "read"
}
service "janus" {
  policy = "write"
}
service "janus-exporter" {
  policy = "write"
}
service "node-exporter" {
  policy = "write"
}
service_prefix "haproxy-" {
  policy = "write"
}
`

// Client is the Consul server's HTTP API with the Controller's token.
type Client struct {
	base  *url.URL
	token string
	hc    *http.Client
}

// New is a client of the server s names.
func New(s Settings) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(s.Address, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("address %q: the server's https:// URL", s.Address)
	}
	if err := s.TLS.TLS().Validate(); err != nil {
		return nil, err
	}
	return &Client{base: u, token: s.Token,
		hc: httpsclient.New(s.TLS.TLS(), httpsclient.Options{MaxBytes: 8 << 20, Timeout: 15 * time.Second, What: "Consul server"})}, nil
}

// APIError is an answer other than 200.
type APIError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *APIError) Error() string {
	what := e.Body
	switch e.Status {
	case http.StatusForbidden:
		what = "the token isn't allowed: " + e.Body
	case http.StatusUnauthorized:
		what = "no token, or an unknown one: " + e.Body
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, what)
}

// notFound: Consul says what was asked for doesn't exist - 404, or 403
// "ACL not found" for a token that's gone.
func notFound(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusNotFound || strings.Contains(strings.ToLower(ae.Body), "not found")
}

func (c *Client) do(ctx context.Context, method, path string, body, into any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := *c.base
	p, q, _ := strings.Cut(path, "?")
	u.Path, u.RawQuery = strings.TrimRight(c.base.Path, "/")+p, q
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("X-Consul-Token", c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, p, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, p, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &APIError{Method: method, Path: p, Status: resp.StatusCode, Body: msg}
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("%s %s: %w", method, p, err)
	}
	return nil
}

// Cluster is what the Controller reads of the Consul cluster - never
// typed by an admin.
type Cluster struct {
	Datacenter string `json:"datacenter"`
	Version    string `json:"version"`
	Server     string `json:"server"` // the node name of the agent asked
	// Servers are the servers' addresses as their peers know them, the
	// hosts the nodes join.
	Servers []string `json:"servers"`
	// GossipKey is the primary key of the LAN pool (secret), "" when the
	// gossip isn't encrypted.
	GossipKey string `json:"-"`
	HasGossip bool   `json:"has_gossip_key"`
}

// Cluster reads the datacenter, the servers and the gossip key.
func (c *Client) Cluster(ctx context.Context) (Cluster, error) {
	var self struct {
		Config struct {
			Datacenter, NodeName, Version string
		}
	}
	if err := c.do(ctx, http.MethodGet, "/v1/agent/self", nil, &self); err != nil {
		return Cluster{}, err
	}
	out := Cluster{Datacenter: self.Config.Datacenter, Version: self.Config.Version, Server: self.Config.NodeName}
	var peers []string
	if err := c.do(ctx, http.MethodGet, "/v1/status/peers", nil, &peers); err != nil {
		return Cluster{}, err
	}
	for _, p := range peers {
		host, _, err := net.SplitHostPort(p)
		if err != nil {
			host = p
		}
		if host != "" && !slices.Contains(out.Servers, host) {
			out.Servers = append(out.Servers, host)
		}
	}
	sort.Strings(out.Servers)
	if len(out.Servers) == 0 {
		return Cluster{}, errors.New("the cluster has no server (no leader elected?)")
	}
	key, err := c.gossipKey(ctx, out.Datacenter)
	if err != nil {
		return Cluster{}, err
	}
	out.GossipKey, out.HasGossip = key, key != ""
	return out, nil
}

// gossipKey is the LAN pool's primary key: what the nodes' agents
// encrypt with.
func (c *Client) gossipKey(ctx context.Context, dc string) (string, error) {
	var rings []struct {
		WAN         bool
		Datacenter  string
		Segment     string
		Keys        map[string]int
		PrimaryKeys map[string]int
	}
	if err := c.do(ctx, http.MethodGet, "/v1/operator/keyring", nil, &rings); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusInternalServerError && strings.Contains(strings.ToLower(ae.Body), "encryption") {
			return "", nil // no gossip encryption
		}
		return "", err
	}
	for _, r := range rings {
		if r.WAN || r.Segment != "" || (r.Datacenter != "" && r.Datacenter != dc) {
			continue
		}
		for _, set := range []map[string]int{r.PrimaryKeys, r.Keys} {
			if len(set) == 1 {
				for k := range set {
					return k, nil
				}
			}
		}
		if len(r.Keys) > 1 {
			return "", errors.New("the gossip keyring has several keys and no primary one: a rotation under way?")
		}
	}
	return "", nil
}

// EnsurePolicy creates or updates janus-nodes; it returns its ID.
func (c *Client) EnsurePolicy(ctx context.Context) (string, error) {
	type policy struct {
		ID          string `json:",omitempty"`
		Name        string
		Description string
		Rules       string
	}
	want := policy{Name: PolicyName, Description: "Janus nodes: made and kept by the Janus Controller", Rules: PolicyRules}
	var cur policy
	err := c.do(ctx, http.MethodGet, "/v1/acl/policy/name/"+PolicyName, nil, &cur)
	switch {
	case err == nil:
		if cur.Rules == want.Rules && cur.Description == want.Description {
			return cur.ID, nil
		}
		want.ID = cur.ID
		var out policy
		if err := c.do(ctx, http.MethodPut, "/v1/acl/policy/"+url.PathEscape(cur.ID), want, &out); err != nil {
			return "", err
		}
		return out.ID, nil
	case notFound(err):
		var out policy
		if err := c.do(ctx, http.MethodPut, "/v1/acl/policy", want, &out); err != nil {
			return "", err
		}
		return out.ID, nil
	}
	return "", err
}

type aclToken struct {
	AccessorID     string `json:",omitempty"`
	SecretID       string `json:",omitempty"`
	Description    string
	Policies       []struct{ Name string }
	NodeIdentities []struct{ NodeName, Datacenter string }
}

func tokenBody(description, node, dc string) aclToken {
	t := aclToken{Description: description}
	t.Policies = []struct{ Name string }{{Name: PolicyName}}
	t.NodeIdentities = []struct{ NodeName, Datacenter string }{{NodeName: node, Datacenter: dc}}
	return t
}

// CreateToken makes a node's token: its node identity and janus-nodes.
func (c *Client) CreateToken(ctx context.Context, description, node, dc string) (accessor, secret string, err error) {
	var out aclToken
	if err := c.do(ctx, http.MethodPut, "/v1/acl/token", tokenBody(description, node, dc), &out); err != nil {
		return "", "", err
	}
	if out.AccessorID == "" || out.SecretID == "" {
		return "", "", errors.New("PUT /v1/acl/token: no accessor or secret in the answer")
	}
	return out.AccessorID, out.SecretID, nil
}

// TokenState is what Consul has of a node's token.
type TokenState int

const (
	TokenOK TokenState = iota
	TokenMissing
	TokenDiffers // another node identity, another policy, another description
)

// CheckToken says how accessor stands against what it should be.
func (c *Client) CheckToken(ctx context.Context, accessor, description, node, dc string) (TokenState, error) {
	var cur aclToken
	if err := c.do(ctx, http.MethodGet, "/v1/acl/token/"+url.PathEscape(accessor), nil, &cur); err != nil {
		if notFound(err) {
			return TokenMissing, nil
		}
		return 0, err
	}
	want := tokenBody(description, node, dc)
	if cur.Description != want.Description || len(cur.NodeIdentities) != 1 || cur.NodeIdentities[0] != want.NodeIdentities[0] ||
		!slices.ContainsFunc(cur.Policies, func(p struct{ Name string }) bool { return p.Name == PolicyName }) {
		return TokenDiffers, nil
	}
	return TokenOK, nil
}

// UpdateToken makes accessor what it should be - its secret kept.
func (c *Client) UpdateToken(ctx context.Context, accessor, description, node, dc string) error {
	body := tokenBody(description, node, dc)
	body.AccessorID = accessor
	return c.do(ctx, http.MethodPut, "/v1/acl/token/"+url.PathEscape(accessor), body, nil)
}

// DeleteToken deletes accessor; gone already is fine.
func (c *Client) DeleteToken(ctx context.Context, accessor string) error {
	err := c.do(ctx, http.MethodDelete, "/v1/acl/token/"+url.PathEscape(accessor), nil, nil)
	if notFound(err) {
		return nil
	}
	return err
}

// Deregister takes node out of the catalog, with its services.
func (c *Client) Deregister(ctx context.Context, dc, node string) error {
	return c.do(ctx, http.MethodPut, "/v1/catalog/deregister", map[string]string{"Datacenter": dc, "Node": node}, nil)
}

// Member is a member of the server's LAN pool.
type Member struct {
	Name   string `json:"name"`
	Addr   string `json:"address"`
	Status string `json:"status"` // alive, leaving, left, failed
}

var memberStatus = map[int]string{0: "none", 1: "alive", 2: "leaving", 3: "left", 4: "failed"}

// Members are the LAN pool's members, by name.
func (c *Client) Members(ctx context.Context) (map[string]Member, error) {
	var members []struct {
		Name   string
		Addr   string
		Status int
	}
	if err := c.do(ctx, http.MethodGet, "/v1/agent/members", nil, &members); err != nil {
		return nil, err
	}
	out := map[string]Member{}
	for _, m := range members {
		out[m.Name] = Member{Name: m.Name, Addr: m.Addr, Status: memberStatus[m.Status]}
	}
	return out, nil
}

// NodeService is a service of a node in the catalog.
type NodeService struct {
	ID      string            `json:"id"`
	Service string            `json:"service"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// NodeServices are node's services in the catalog; none if it isn't
// there.
func (c *Client) NodeServices(ctx context.Context, node string) ([]NodeService, error) {
	var out struct {
		Services []struct {
			ID      string
			Service string
			Meta    map[string]string
		}
	}
	if err := c.do(ctx, http.MethodGet, "/v1/catalog/node-services/"+url.PathEscape(node), nil, &out); err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var list []NodeService
	for _, s := range out.Services {
		list = append(list, NodeService{ID: s.ID, Service: s.Service, Meta: s.Meta})
	}
	return list, nil
}

// Check is one step of a test of the settings, in words.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Test checks what the Controller needs of the server and its token:
// reaching it, the cluster's datacenter and servers, the gossip key, the
// rights on tokens and policies, on the nodes' membership.
func (c *Client) Test(ctx context.Context) (Cluster, []Check) {
	var checks []Check
	add := func(name string, err error, ok string) bool {
		if err != nil {
			checks = append(checks, Check{Name: name, Detail: err.Error()})
			return false
		}
		checks = append(checks, Check{Name: name, OK: true, Detail: ok})
		return true
	}
	cl, err := c.Cluster(ctx)
	if !add("cluster", err, fmt.Sprintf("Consul %s, datacenter %s, servers %s", cl.Version, cl.Datacenter, strings.Join(cl.Servers, ", "))) {
		return cl, checks
	}
	if cl.HasGossip {
		checks = append(checks, Check{Name: "gossip key", OK: true, Detail: "read (keyring = \"read\"): the nodes encrypt their gossip with it"})
	} else {
		checks = append(checks, Check{Name: "gossip key", OK: true, Detail: "the cluster's gossip isn't encrypted - the nodes' won't be either"})
	}
	var self struct {
		Description string
		Policies    []struct{ Name string }
	}
	if add("token", c.do(ctx, http.MethodGet, "/v1/acl/token/self", nil, &self), "") {
		var names []string
		for _, p := range self.Policies {
			names = append(names, p.Name)
		}
		checks[len(checks)-1].Detail = fmt.Sprintf("%q, policies %s", self.Description, strings.Join(names, ", "))
	}
	// The nodes' policy, made or brought up to date: acl = "write" proven
	// without a token made for nothing.
	if _, err := c.EnsurePolicy(ctx); err == nil {
		add("acl", nil, "the policy "+PolicyName+" is there: the nodes' tokens can be made (acl = \"write\")")
	} else {
		add("acl", err, "")
	}
	members, err := c.Members(ctx)
	add("members", err, fmt.Sprintf("%d in the LAN pool", len(members)))
	return cl, checks
}
