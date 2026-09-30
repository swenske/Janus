package nodeproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// TestPerNodeCSRF: the client certificate opening a per-node origin is
// attached by the browser to requests *any* site triggers, so a request
// from another origin must be refused before it reaches the node. The
// test node has no usable credential, so a request that gets past the
// check fails at the dial with 502 - never a 403.
func TestPerNodeCSRF(t *testing.T) {
	node := &store.Node{ID: "csrf-test", Name: "n", Address: "127.0.0.1:1", CACertPEM: []byte("not a certificate")}
	h, err := newHandler(node, nil)
	if err != nil {
		t.Fatal(err)
	}

	type req struct {
		method, path, body string
		headers            map[string]string
	}
	cases := []struct {
		name string
		req  req
		want int
	}{
		{"cross-site POST power", req{"POST", "/api/system/power", `{"action":"reboot"}`, map[string]string{"Sec-Fetch-Site": "cross-site"}}, http.StatusForbidden},
		{"same-site POST (another port on the same host)", req{"POST", "/api/system/services/haproxy/stop", "", map[string]string{"Sec-Fetch-Site": "same-site"}}, http.StatusForbidden},
		{"old browser, foreign Origin", req{"POST", "/api/system/services/haproxy/stop", "", map[string]string{"Origin": "https://evil.example"}}, http.StatusForbidden},
		{"cross-site DELETE certificate", req{"DELETE", "/api/haproxy/certs/site.pem", "", map[string]string{"Sec-Fetch-Site": "cross-site"}}, http.StatusForbidden},
		{"cross-site GET pcap", req{"GET", "/api/pcap?interface=eth0&duration=5&promisc=true", "", map[string]string{"Sec-Fetch-Site": "cross-site"}}, http.StatusForbidden},

		{"same-origin POST", req{"POST", "/api/system/services/haproxy/stop", "", map[string]string{"Sec-Fetch-Site": "same-origin"}}, http.StatusBadGateway},
		{"non-browser POST (no headers)", req{"POST", "/api/system/power", `{"action":"reboot"}`, nil}, http.StatusBadGateway},
		{"same-origin GET pcap", req{"GET", "/api/pcap?interface=eth0&duration=5", "", map[string]string{"Sec-Fetch-Site": "same-origin"}}, http.StatusBadGateway},
		{"cross-site GET read-only view", req{"GET", "/api/system/services", "", map[string]string{"Sec-Fetch-Site": "cross-site"}}, http.StatusBadGateway},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.req.method, "https://controller.example:9500"+c.req.path, strings.NewReader(c.req.body))
		for k, v := range c.req.headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: status %d (%s), want %d", c.name, w.Code, strings.TrimSpace(w.Body.String()), c.want)
		}
	}
}
