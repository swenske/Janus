// Package consultest is a Consul server's HTTP API as the Controller
// uses it - ACL tokens and policies, the keyring, the catalog -, for
// tests.
package consultest

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Token is a token the server has.
type Token struct {
	AccessorID, SecretID, Description string
	Policies                          []struct{ Name string }
	NodeIdentities                    []struct{ NodeName, Datacenter string }
}

// Server is the fake: over TLS (Server.Certificate), refusing any token
// but Token.
type Server struct {
	*httptest.Server
	Token      string
	Datacenter string
	Peers      []string
	GossipKey  string // "": no encryption

	mu           sync.Mutex
	Policies     map[string]string // name -> rules
	PolicyIDs    map[string]string // name -> ID
	Tokens       map[string]*Token // by accessor
	Deregistered []string          // node names
	Members      map[string]int    // name -> status (1 alive, 4 failed)
	Services     map[string][]string
	Calls        []string
}

// New starts a fake server; it stops with the test.
func New(t *testing.T, token string) *Server {
	t.Helper()
	s := &Server{Token: token, Datacenter: "lgs", Peers: []string{"10.1.0.10:8300"}, GossipKey: "pUqJrVyVRj5jsiYEkM/tFQYfWyJIv4s3XkvDwy7Cu5s=",
		Policies: map[string]string{}, PolicyIDs: map[string]string{}, Tokens: map[string]*Token{}, Members: map[string]int{}, Services: map[string][]string{}}
	s.Server = httptest.NewTLSServer(s)
	t.Cleanup(s.Close)
	return s
}

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// TokenFor is the token whose node identity is node, nil if none.
func (s *Server) TokenFor(node string) *Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.Tokens {
		for _, ni := range t.NodeIdentities {
			if ni.NodeName == node {
				c := *t
				return &c
			}
		}
	}
	return nil
}

// TokenCount is how many tokens it has.
func (s *Server) TokenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Tokens)
}

// DeregisteredNodes are the nodes taken out of the catalog.
func (s *Server) DeregisteredNodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Deregistered...)
}

// DeleteTokenOf deletes node's token, as an operator would in Consul.
func (s *Server) DeleteTokenOf(node string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for acc, t := range s.Tokens {
		for _, ni := range t.NodeIdentities {
			if ni.NodeName == node {
				delete(s.Tokens, acc)
			}
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := r.URL.Path
	s.Calls = append(s.Calls, r.Method+" "+p)
	if r.Header.Get("X-Consul-Token") != s.Token {
		http.Error(w, "ACL not found", http.StatusForbidden)
		return
	}
	js := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == "GET" && p == "/v1/agent/self":
		js(map[string]any{"Config": map[string]any{"Datacenter": s.Datacenter, "NodeName": "consul-1", "Version": "2.1.0"}})
	case r.Method == "GET" && p == "/v1/status/peers":
		js(s.Peers)
	case r.Method == "GET" && p == "/v1/operator/keyring":
		if s.GossipKey == "" {
			http.Error(w, "Agent does not have gossip encryption enabled", http.StatusInternalServerError)
			return
		}
		keys := map[string]int{s.GossipKey: 2}
		js([]map[string]any{{"WAN": true, "Datacenter": s.Datacenter, "Keys": keys, "PrimaryKeys": keys}, {"WAN": false, "Datacenter": s.Datacenter, "Keys": keys, "PrimaryKeys": keys}})
	case r.Method == "GET" && strings.HasPrefix(p, "/v1/acl/policy/name/"):
		name := strings.TrimPrefix(p, "/v1/acl/policy/name/")
		rules, ok := s.Policies[name]
		if !ok {
			http.Error(w, "ACL not found", http.StatusForbidden)
			return
		}
		js(map[string]any{"ID": s.PolicyIDs[name], "Name": name, "Rules": rules, "Description": "Janus nodes: made and kept by the Janus Controller"})
	case r.Method == "PUT" && (p == "/v1/acl/policy" || strings.HasPrefix(p, "/v1/acl/policy/")):
		var in struct{ ID, Name, Rules, Description string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		id := s.PolicyIDs[in.Name]
		if id == "" {
			id = uuid()
		}
		s.Policies[in.Name], s.PolicyIDs[in.Name] = in.Rules, id
		js(map[string]any{"ID": id, "Name": in.Name})
	case r.Method == "GET" && p == "/v1/acl/token/self":
		js(map[string]any{"Description": "janus-controller", "Policies": []map[string]string{{"Name": "janus-controller"}}})
	case r.Method == "PUT" && p == "/v1/acl/token":
		var t Token
		_ = json.NewDecoder(r.Body).Decode(&t)
		t.AccessorID, t.SecretID = uuid(), uuid()
		s.Tokens[t.AccessorID] = &t
		js(t)
	case strings.HasPrefix(p, "/v1/acl/token/"):
		acc := strings.TrimPrefix(p, "/v1/acl/token/")
		t, ok := s.Tokens[acc]
		if !ok {
			http.Error(w, "ACL not found", http.StatusForbidden)
			return
		}
		switch r.Method {
		case "GET":
			js(t)
		case "PUT":
			var in Token
			_ = json.NewDecoder(r.Body).Decode(&in)
			t.Description, t.Policies, t.NodeIdentities = in.Description, in.Policies, in.NodeIdentities
			js(t)
		case "DELETE":
			delete(s.Tokens, acc)
			js(true)
		}
	case r.Method == "PUT" && p == "/v1/catalog/deregister":
		var in struct{ Datacenter, Node string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.Deregistered = append(s.Deregistered, in.Node)
		delete(s.Members, in.Node)
		delete(s.Services, in.Node)
		js(true)
	case r.Method == "GET" && p == "/v1/agent/members":
		var out []map[string]any
		for name, st := range s.Members {
			out = append(out, map[string]any{"Name": name, "Addr": "10.1.0.20", "Status": st})
		}
		js(out)
	case r.Method == "GET" && strings.HasPrefix(p, "/v1/catalog/node-services/"):
		node := strings.TrimPrefix(p, "/v1/catalog/node-services/")
		svcs, ok := s.Services[node]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var list []map[string]any
		for i, name := range svcs {
			list = append(list, map[string]any{"ID": fmt.Sprintf("janus:%d", i), "Service": name})
		}
		js(map[string]any{"Node": map[string]string{"Node": node}, "Services": list})
	default:
		http.NotFound(w, r)
	}
}

// SetGossipKey sets the LAN pool's key, "" for none.
func (s *Server) SetGossipKey(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.GossipKey = k
}

// Policy is the rules of the policy named name.
func (s *Server) Policy(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Policies[name]
}

// SetMember makes name a member of the LAN pool (1 alive, 4 failed),
// with services in the catalog.
func (s *Server) SetMember(name string, status int, services ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Members[name] = status
	s.Services[name] = services
}

// Accept makes token the only one the server takes.
func (s *Server) Accept(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Token = token
}
