package nodeproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pki"
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
	reader := User{Name: "rita", Perms: []Perm{{Role: "os:reader"}}}
	if code, body := do(reader, "GET", "/api/me", ""); code != http.StatusOK || !strings.Contains(body, `"HAProxyService/ShowInfo"`) || strings.Contains(body, `"HAProxyService/ApplyConfig"`) {
		t.Errorf("a reader's /api/me: %d %s", code, body)
	}

	admin := User{Name: "root", Perms: []Perm{{Role: "os:admin"}}}
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

// TestPermFor: a call goes with the first permission that allows it -
// its role and domains named to the node -, none and it's refused here.
func TestPermFor(t *testing.T) {
	u := User{Name: "tf", Perms: []Perm{{Role: "os:operator", Domains: []string{"haproxy"}}, {Role: "os:reader"}}}
	for _, c := range []struct {
		method string
		ok     bool
		role   string
	}{
		{"/janus.v1alpha1.HAProxyService/ApplyConfig", true, "os:operator"},
		{"/janus.v1alpha1.SystemService/Stats", true, "os:operator"},
		{"/janus.v1alpha1.NetworkService/NetworkConfigGet", true, "os:reader"},
		{"/janus.v1alpha1.SystemService/Reboot", false, ""},
		{"/janus.v1alpha1.NetworkService/NetworkConfigApply", false, ""},
	} {
		ctx := WithUser(context.Background(), u)
		out, err := actingFor(ctx, c.method)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.method, err)
			continue
		}
		if !c.ok {
			continue
		}
		md, _ := metadata.FromOutgoingContext(out)
		if got := md.Get(pki.AsRolesKey); len(got) != 1 || got[0] != c.role {
			t.Errorf("%s: roles %v", c.method, got)
		}
		if d := md.Get(pki.AsDomainsKey); c.role == "os:operator" && (len(d) != 1 || d[0] != "haproxy") {
			t.Errorf("%s: domains %v", c.method, d)
		}
	}
	if may := u.May(); !slices.Contains(may, "HAProxyService/ApplyConfig") || slices.Contains(may, "SystemService/Reboot") {
		t.Errorf("may %v", may)
	}
}
