package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
)

// Each account's preferences through the API: the defaults until it
// chooses, its own choice not another's, an interval not offered
// refused, {} back to the defaults - and a token has none.
func TestPreferencesAPI(t *testing.T) {
	a := newAuthApp(t)
	rita, olga := a.login(t, "rita", "rita-password"), a.login(t, "olga", "olga-password")
	get := func(cred string) preferencesView {
		t.Helper()
		code, body := a.req(t, "GET", "/api/me/preferences", cred, nil)
		if code != http.StatusOK {
			t.Fatalf("GET: %d %s", code, body)
		}
		var v preferencesView
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	v := get(rita)
	if v.NodesRefreshSeconds != 15 || v.HypervisorsRefreshSeconds != 15 || !*v.NodeStats || !*v.NodeCharts || len(v.Choices["nodes"]) == 0 {
		t.Fatalf("defaults: %+v", v)
	}
	if code, body := a.req(t, "PUT", "/api/me/preferences", rita, map[string]any{"nodes_refresh_seconds": 60, "node_charts": false}); code != http.StatusOK {
		t.Fatalf("PUT: %d %s", code, body)
	}
	if v = get(rita); v.NodesRefreshSeconds != 60 || *v.NodeCharts || !*v.NodeStats || v.Chosen.NodesRefreshSeconds != 60 || v.Chosen.NodeStats != nil {
		t.Errorf("rita's: %+v", v)
	}
	if v = get(olga); v.NodesRefreshSeconds != 15 || !*v.NodeCharts {
		t.Errorf("olga's changed with rita's: %+v", v)
	}
	if code, _ := a.req(t, "PUT", "/api/me/preferences", rita, map[string]any{"nodes_refresh_seconds": 1}); code != http.StatusBadRequest {
		t.Errorf("a 1 s refresh: %d", code)
	}
	if code, _ := a.req(t, "PUT", "/api/me/preferences", rita, map[string]any{}); code != http.StatusOK {
		t.Errorf("a reset: %d", code)
	}
	if v = get(rita); v.NodesRefreshSeconds != 15 || !*v.NodeCharts {
		t.Errorf("after a reset: %+v", v)
	}
	// The time zone and the rate unit.
	if code, body := a.req(t, "PUT", "/api/me/preferences", rita, map[string]any{"time_zone": "utc", "rate_unit": "bits"}); code != http.StatusOK {
		t.Fatalf("utc and bits: %d %s", code, body)
	}
	if v = get(rita); v.TimeZone != "utc" || v.RateUnit != "bits" {
		t.Errorf("utc and bits: %+v", v)
	}
	if code, _ := a.req(t, "PUT", "/api/me/preferences", rita, map[string]any{"time_zone": "Europe/Paris"}); code != http.StatusBadRequest {
		t.Errorf("a named zone: %d", code)
	}
	if v = get(olga); v.TimeZone != "local" || v.RateUnit != "bytes" {
		t.Errorf("olga's defaults: %+v", v)
	}
	// An admin's floors: the choices start there, an account below them
	// follows them, a choice below them is refused - and only an admin
	// sets them.
	root := a.login(t, "root", "root-password")
	if code, _ := a.req(t, "PUT", "/api/me/preferences", olga, map[string]any{"nodes_refresh_seconds": 5}); code != http.StatusOK {
		t.Fatal("olga at 5 s")
	}
	if code, _ := a.req(t, "PUT", "/api/settings", olga, map[string]any{"nodes_refresh_floor_seconds": 30}); code != http.StatusForbidden {
		t.Errorf("an operator set a floor: %d", code)
	}
	if code, body := a.req(t, "PUT", "/api/settings", root, map[string]any{"nodes_refresh_floor_seconds": 30, "hypervisors_refresh_floor_seconds": 60}); code != http.StatusOK {
		t.Fatalf("the floors: %d %s", code, body)
	}
	if code, _ := a.req(t, "PUT", "/api/settings", root, map[string]any{"nodes_refresh_floor_seconds": 7}); code != http.StatusBadRequest {
		t.Errorf("a 7 s floor: %d", code)
	}
	v = get(olga)
	if v.NodesRefreshSeconds != 30 || v.HypervisorsRefreshSeconds != 60 || v.Chosen.NodesRefreshSeconds != 5 || v.Choices["nodes"][0] != 30 || v.Floors["nodes"] != 30 {
		t.Errorf("olga under the floors: %+v", v)
	}
	if code, _ := a.req(t, "PUT", "/api/me/preferences", olga, map[string]any{"nodes_refresh_seconds": 10}); code != http.StatusBadRequest {
		t.Errorf("10 s under a 30 s floor: %d", code)
	}
	if code, _ := a.req(t, "PUT", "/api/me/preferences", olga, map[string]any{"nodes_refresh_seconds": 60}); code != http.StatusOK {
		t.Errorf("60 s over a 30 s floor: %d", code)
	}

	secret, _, err := a.tokens.Create("rita", auth.Reader, "ci", time.Hour, auth.TokenScope{})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := a.req(t, "GET", "/api/me/preferences", "Bearer "+secret, nil); code != http.StatusForbidden {
		t.Errorf("a token: %d", code)
	}
}
