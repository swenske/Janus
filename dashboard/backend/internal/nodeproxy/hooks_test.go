package nodeproxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// A locked machine's node page refuses what its manager would undo - the
// network, an update - and says so; the rest still goes to the node
// (here, a node that can't be dialed: 502).
func TestLockedNodePage(t *testing.T) {
	node := &store.Node{ID: "lock-test", Name: "n", Address: "127.0.0.1:1", CACertPEM: []byte("not a certificate"), MachineID: "m"}
	prevLocked, prevChanged := LockedBy, NodeChanged
	defer func() { LockedBy, NodeChanged = prevLocked, prevChanged }()
	locked := "terraform"
	LockedBy = func(*store.Node) string { return locked }
	changed := 0
	NodeChanged = func(*store.Node) { changed++ }
	h, err := newHandler(node, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) (int, string) {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code, rec.Body.String()
	}
	for _, p := range []string{"/api/network/apply", "/api/lifecycle/upgrade-url", "/api/lifecycle/upgrade-relay", "/api/lifecycle/upgrade-upload"} {
		if code, body := do("POST", p, `{"reference":"https://x/"}`); code != http.StatusLocked || !strings.Contains(body, "terraform") {
			t.Errorf("%s on a locked node: %d %s", p, code, body)
		}
	}
	if changed != 0 {
		t.Errorf("a refused change triggered %d syncs", changed)
	}
	if code, _ := do("POST", "/api/system/power", `{"action":"reboot"}`); code == http.StatusLocked {
		t.Error("a reboot was refused on a locked node")
	}
	if code, body := do("GET", "/api/node", ""); code != http.StatusOK || !strings.Contains(body, `"locked_by":"terraform"`) {
		t.Errorf("/api/node: %d %s", code, body)
	}

	locked = ""
	if code, _ := do("POST", "/api/network/apply", `{"config":{}}`); code == http.StatusLocked {
		t.Error("a released node's network is still refused")
	}
	if changed != 1 {
		t.Errorf("a network change from the page triggered %d syncs, want 1", changed)
	}
}
