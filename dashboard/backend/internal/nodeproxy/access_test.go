package nodeproxy

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// TestAccessPage: the page learns what its account may call; replacing
// the node's CA is refused while the Controller holds a credential that
// CA issued, and wants one way to hand over the new admin credential.
func TestAccessPage(t *testing.T) {
	node := &store.Node{ID: "access-test", Name: "n", Address: "127.0.0.1:1", CACertPEM: []byte("not a certificate")}
	h, err := newHandler(node, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(u User, method, path, body string) (int, string) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r = r.WithContext(WithUser(r.Context(), u))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, rec.Body.String()
	}
	reader := User{Name: "rita", Roles: []string{"os:reader"}}
	if code, body := do(reader, "GET", "/api/me", ""); code != http.StatusOK || !strings.Contains(body, `"HAProxyService/ShowInfo"`) || strings.Contains(body, `"HAProxyService/ApplyConfig"`) {
		t.Errorf("a reader's /api/me: %d %s", code, body)
	}

	admin := User{Name: "root", Roles: []string{"os:admin"}}
	if code, body := do(admin, "POST", "/api/access/rotate-ca", `{"console":true}`); code != http.StatusConflict || !strings.Contains(body, "fleet") {
		t.Errorf("not on the fleet: %d %s", code, body)
	}
	node.Fleet = true
	for _, body := range []string{`{}`, `{"console":true,"admin_public_key":"-----BEGIN PUBLIC KEY-----"}`} {
		if code, _ := do(admin, "POST", "/api/access/rotate-ca", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, code)
		}
	}
	// Then it goes to the node (here one that can't be dialed).
	if code, _ := do(admin, "POST", "/api/access/rotate-ca", `{"console":true}`); slices.Contains([]int{http.StatusConflict, http.StatusBadRequest}, code) {
		t.Errorf("on the fleet: %d", code)
	}
}
