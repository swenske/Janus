package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/logforward"
	"github.com/swenske/Janus/internal/logship"
)

// plainSealer seals nothing: the settings' secrets as they are.
type plainSealer struct{}

func (plainSealer) Seal(p []byte, _ string) ([]byte, error) { return p, nil }
func (plainSealer) Open(s []byte, _ string) ([]byte, error) { return s, nil }

// A target is tested as the page shows it, saved or not - what the
// operator sees is what's sent - with the saved target's secrets where it
// leaves them out; without config, only a saved target.
func TestLogForwardingTestDraft(t *testing.T) {
	a, _ := newTestApp(t)
	if err := a.startLogForwarding(plainSealer{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	lines := make(chan string, 10)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					lines <- sc.Text()
				}
			}()
		}
	}()

	test := func(body string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		a.handleLogForwardingTest(w, httptest.NewRequest(http.MethodPost, "/api/log-forwarding/test", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	draft := `{"name": "nas", "syslog": {"address": "` + ln.Addr().String() + `", "transport": "tcp", "format": "rfc3164", "framing": "lf"}}`

	// Not saved: tested all the same, as shown.
	code, out := test(`{"target": "nas", "config": ` + draft + `}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("a draft target: %d %v", code, out)
	}
	select {
	case l := <-lines:
		if !strings.Contains(l, "log forwarding test") {
			t.Errorf("the server got %q", l)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the draft target's test record never came")
	}

	// Without config, a target not saved is unknown.
	if code, _ := test(`{"target": "nas"}`); code != http.StatusNotFound {
		t.Errorf("a target not saved, without config: %d, want 404", code)
	}
	// A draft that can't work is said so.
	if code, out := test(`{"target": "x", "config": {"name": "x", "syslog": {"address": ""}}}`); code != http.StatusBadRequest {
		t.Errorf("a draft without an address: %d %v, want 400", code, out)
	}
	// A profile not saved yet: its target needs a node to be tested from.
	if code, out := test(`{"profile": "new", "target": "nas", "config": ` + draft + `}`); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "node") {
		t.Errorf("a new profile's target without a node: %d %v", code, out)
	}
}

// A target paused on the page (its disabled flag, the rest as read back -
// no secret) keeps its secrets, the Controller's own and a profile's;
// the view says it's paused and which secrets it has; a node given the
// profile compares equal to it once paused, and differs from the
// profile resumed - so the loop applies a pause, then leaves it.
func TestLogForwardingPausedTargets(t *testing.T) {
	a, _ := newTestApp(t)
	auditLog, err := audit.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.audit = auditLog // the Controller's own audit: a source of its targets
	if err := a.startLogForwarding(plainSealer{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	put := func(body string) logForwardingView {
		t.Helper()
		w := httptest.NewRecorder()
		a.handleLogForwardingSet(w, httptest.NewRequest(http.MethodPut, "/api/log-forwarding", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("PUT: %d %s", w.Code, w.Body)
		}
		var v logForwardingView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	// Nothing listens there: the targets never send, which is fine.
	put(`{"controller": {"targets": [{"name": "es", "elasticsearch": {"url": "https://127.0.0.1:1", "api_key": "controller-secret"}}]},
	      "profiles": [{"name": "web", "selector": "team=web", "config": {"targets": [{"name": "es", "elasticsearch": {"url": "https://127.0.0.1:1", "username": "janus", "password": "fleet-secret"}}]}}]}`)
	// As the page sends them paused: read back, disabled, no secret.
	v := put(`{"controller": {"targets": [{"name": "es", "sources": ["controller", "audit"], "disabled": true, "elasticsearch": {"url": "https://127.0.0.1:1", "namespace": "default"}}]},
	      "profiles": [{"name": "web", "selector": "team=web", "config": {"targets": [{"name": "es", "disabled": true, "elasticsearch": {"url": "https://127.0.0.1:1", "namespace": "default", "username": "janus"}}]}}]}`)
	if !v.Controller.Targets[0].Disabled || !v.Profiles[0].Config.Targets[0].Disabled {
		t.Fatalf("not paused: %+v", v)
	}
	if got := v.ControllerSecrets["es"]; len(got) != 1 || got[0] != "api_key" {
		t.Errorf("the Controller's target's secrets: %v", got)
	}
	if got := v.Profiles[0].Secrets["es"]; len(got) != 1 || got[0] != "password" {
		t.Errorf("the profile's target's secrets: %v", got)
	}
	if st := v.ControllerStatus; len(st) != 1 || st[0].State != "disabled" {
		t.Errorf("the Controller's paused target: %+v", st)
	}
	set := a.logfwd.store.Settings()
	if set.Controller.Targets[0].Elasticsearch.APIKey != "controller-secret" || set.Profiles[0].Config.Targets[0].Elasticsearch.Password != "fleet-secret" {
		t.Fatalf("a secret was lost: %+v", set)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "secret\"") {
		t.Fatalf("a secret in the view: %s", raw)
	}

	// The profile's loop: a node holding the paused profile is applied as
	// is; the profile resumed differs from it.
	p := set.Profiles[0]
	want := p.Config
	want.ManagedBy = logforward.ManagedBy(a.controllerID, p)
	node := logship.FromProto(logship.ToProto(want.Redacted())) // what the node gives back
	if !sameConfig(node, want.Redacted()) {
		t.Error("a node holding the paused profile differs from it")
	}
	resumed := want
	resumed.Targets = append([]logship.Target(nil), want.Targets...)
	resumed.Targets[0].Disabled = false
	if sameConfig(node, resumed.Redacted()) {
		t.Error("the profile resumed is the same as paused")
	}
}
