package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
	"github.com/swenske/Janus/internal/clusters"
)

// TestClusterReaderRedaction: a reader gets haproxy.cfg masked and no
// VRRP/BGP template or member vars; an admin gets them all (security
// review 2026-11-05 #8, JANUS-2026-013).
func TestClusterReaderRedaction(t *testing.T) {
	a, _, nodes := clusterApp(t, "lb-1")
	c := newCluster(t, a, nodes, "lb-1")
	c, _ = a.clusters.Store.Update(c.ID, func(c *clusters.Cluster) error {
		c.Members[0].Vars = map[string]string{"vrrp_pass": "s3cr3t"}
		return nil
	})
	v, err := a.clusters.Store.NewVersion(c.ID, "admin", "", map[string][]byte{
		"haproxy.cfg":     []byte("userlist u\n  user admin insecure-password s3cr3t\n"),
		"keepalived.conf": []byte("vrrp_instance VI {\n  authentication { auth_pass s3cr3t }\n}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}

	withRole := func(req *http.Request, role auth.Role) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), principalKey{}, principal{User: "u", Role: role}))
	}
	version := func(role auth.Role) clusterVersionView {
		req := httptest.NewRequest("GET", "/api/clusters/"+c.ID+"/versions/"+fmt.Sprint(v.Version), nil)
		req.SetPathValue("id", c.ID)
		req.SetPathValue("n", fmt.Sprint(v.Version))
		rec := httptest.NewRecorder()
		a.handleClusterVersion(rec, withRole(req, role))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: version: %d %s", role, rec.Code, rec.Body)
		}
		var out clusterVersionView
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	admin := version(auth.Admin)
	if !strings.Contains(admin.Content["haproxy.cfg"], "s3cr3t") || admin.Content["keepalived.conf"] == "" {
		t.Errorf("an admin should see the real haproxy.cfg and keepalived.conf: %v", admin.Content)
	}
	reader := version(auth.Reader)
	if strings.Contains(reader.Content["haproxy.cfg"], "s3cr3t") {
		t.Errorf("a reader saw the userlist password in haproxy.cfg: %q", reader.Content["haproxy.cfg"])
	}
	if _, ok := reader.Content["keepalived.conf"]; ok {
		t.Error("a reader got keepalived.conf (auth_pass)")
	}

	view := func(role auth.Role) clusterView {
		req := withRole(httptest.NewRequest("GET", "/api/clusters/"+c.ID, nil), role)
		return a.clusterView(c, req)
	}
	if view(auth.Admin).Members[0].Vars["vrrp_pass"] != "s3cr3t" {
		t.Error("an admin should see member vars")
	}
	if view(auth.Reader).Members[0].Vars != nil {
		t.Error("a reader saw member vars")
	}
}
