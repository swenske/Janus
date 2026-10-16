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
