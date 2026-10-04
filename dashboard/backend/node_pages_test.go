package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// TestNodePages: a node's page is on the Controller's origin, its API
// behind the accounts - a reader reads, an operator changes, and a node
// that doesn't trust the fleet yet (reached as admin) only for an admin.
// The test nodes can't be dialed: a request the Controller lets through
// fails at the dial with 502.
func TestNodePages(t *testing.T) {
	a := newAuthApp(t)
	fleetNode := &store.Node{Name: "edge-1", Address: "127.0.0.1:1", CACertPEM: []byte("ca"), Fleet: true}
	oldNode := &store.Node{Name: "edge-old", Address: "127.0.0.1:1", CACertPEM: []byte("ca"), ServiceCertPEM: []byte("c"), ServiceKeyPEM: []byte("k")}
	for _, n := range []*store.Node{fleetNode, oldNode} {
		if err := a.store.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	rita, olga, root := a.login(t, "rita", "rita-password"), a.login(t, "olga", "olga-password"), a.login(t, "root", "root-password")
	fleetAPI, oldAPI := "/nodes/"+fleetNode.ID+"/api/system/services", "/nodes/"+oldNode.ID+"/api/system/services"
	stop := "/nodes/" + fleetNode.ID + "/api/system/services/haproxy/stop"
	for _, c := range []struct {
		name, method, path, cred string
		want                     int
	}{
		{"the page, signed out", "GET", "/nodes/" + fleetNode.ID + "/", "", http.StatusOK},
		{"no slash", "GET", "/nodes/" + fleetNode.ID, "", http.StatusMovedPermanently},
		{"an unknown node", "GET", "/nodes/nope/", rita, http.StatusNotFound},
		{"its API, signed out", "GET", fleetAPI, "", http.StatusUnauthorized},
		{"a reader reads", "GET", fleetAPI, rita, http.StatusBadGateway},
		{"a reader changes", "POST", stop, rita, http.StatusForbidden},
		{"an operator changes", "POST", stop, olga, http.StatusBadGateway},
		{"an operator, a node without the fleet", "GET", oldAPI, olga, http.StatusForbidden},
		{"an admin, a node without the fleet", "GET", oldAPI, root, http.StatusBadGateway},
	} {
		code, body := a.req(t, c.method, c.path, c.cred, nil)
		if code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, strings.TrimSpace(body), c.want)
		}
	}
	entries, _ := a.audit.Recent(10, nil)
	if len(entries) == 0 || entries[0].Path != stop || entries[0].User != "olga" {
		t.Errorf("the audit's last entry: %+v", entries)
	}
	if err := a.removeNode(t.Context(), fleetNode.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := a.req(t, "GET", fleetAPI, root, nil); code != http.StatusNotFound {
		t.Errorf("a removed node's page: %d", code)
	}
}
