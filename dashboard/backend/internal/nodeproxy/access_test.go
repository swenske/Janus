package nodeproxy

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// withRoles is the TLS state of a request whose verified client
// certificate carries roles.
func withRoles(roles ...string) *tls.ConnectionState {
	return &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{Subject: pkix.Name{CommonName: "c", Organization: roles}}}}}
}

// TestNodePageNeedsAdminCertificate: the Controller relays as admin
// whatever certificate opened the page, so only an os:admin one opens
// it - the page itself and every API call. The test node has no usable
// credential: a request let through fails at the dial with 502.
func TestNodePageNeedsAdminCertificate(t *testing.T) {
	node := &store.Node{ID: "access-test", Name: "n", Address: "127.0.0.1:1", CACertPEM: []byte("not a certificate")}
	h, err := newHandler(node, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, method, path string
		tls                *tls.ConnectionState
		want               int
	}{
		{"reader opens the page", "GET", "/", withRoles("os:reader"), http.StatusForbidden},
		{"reader reads a view", "GET", "/api/system/services", withRoles("os:reader"), http.StatusForbidden},
		{"reader stops HAProxy", "POST", "/api/system/services/haproxy/stop", withRoles("os:reader"), http.StatusForbidden},
		{"no role at all", "GET", "/api/system/services", withRoles(), http.StatusForbidden},
		{"no verified chain", "GET", "/api/system/services", &tls.ConnectionState{}, http.StatusForbidden},
		{"admin reads a view", "GET", "/api/system/services", withRoles("os:admin"), http.StatusBadGateway},
		{"admin and reader", "POST", "/api/system/services/haproxy/stop", withRoles("os:reader", "os:admin"), http.StatusBadGateway},
	} {
		r := httptest.NewRequest(c.method, "https://controller.example:9500"+c.path, nil)
		r.TLS = c.tls
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: status %d (%s), want %d", c.name, w.Code, strings.TrimSpace(w.Body.String()), c.want)
		}
		if c.want == http.StatusForbidden && !strings.Contains(w.Body.String(), "needs an os:admin certificate") {
			t.Errorf("%s: no explanation in %q", c.name, w.Body.String())
		}
	}
}
