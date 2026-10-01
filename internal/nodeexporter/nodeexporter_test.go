package nodeexporter

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// The manifest's command line is what a node without saved settings
// runs before janusd configures the service: it must be the defaults.
func TestManifestRunsTheDefaults(t *testing.T) {
	data, err := os.ReadFile("../../extensions/prometheus-node-exporter/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Services []struct {
			ID   string   `json:"id"`
			Args []string `json:"args"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Services) != 1 || m.Services[0].ID != ServiceID {
		t.Fatalf("manifest services: %+v", m.Services)
	}
	if want := Args(DefaultConfig()); !slices.Equal(m.Services[0].Args, want) {
		t.Fatalf("manifest args %v\nwant %v", m.Services[0].Args, want)
	}
}

func TestValidate(t *testing.T) {
	c := Config{Enabled: true, Collectors: []string{"netdev", "cpu", "netdev"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Port != DefaultPort || strings.Join(c.Collectors, ",") != "cpu,netdev" {
		t.Fatalf("normalized: %+v", c)
	}
	c = Config{}
	if err := c.Validate(); err != nil || !slices.Equal(c.Collectors, DefaultCollectors()) {
		t.Fatalf("no collectors: %+v, %v", c, err)
	}
	for _, bad := range []Config{
		{Port: 70000},
		{Port: 9505},
		{Address: "not-an-ip"},
		{Collectors: []string{"textfile"}},
		{Collectors: []string{"cpu --web.config.file=/x"}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestArgs(t *testing.T) {
	got := Args(Config{Address: "fd00::1", Port: 9200, Collectors: []string{"cpu", "softnet"}})
	want := []string{"--web.listen-address=[fd00::1]:9200", "--collector.disable-defaults", "--collector.cpu", "--collector.softnet"}
	if !slices.Equal(got, want) {
		t.Fatalf("args %v, want %v", got, want)
	}
}

func TestLoadSave(t *testing.T) {
	Dir = t.TempDir()
	if cfg, isDefault, err := Load(); err != nil || !isDefault || !cfg.Enabled || cfg.Port != DefaultPort {
		t.Fatalf("nothing saved: %+v %v %v", cfg, isDefault, err)
	}
	in := Config{Enabled: false, Address: "192.0.2.10", Port: 9111, Collectors: []string{"cpu", "meminfo"}}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	out, isDefault, err := Load()
	if err != nil || isDefault || out.Enabled || out.Address != in.Address || out.Port != 9111 || !slices.Equal(out.Collectors, in.Collectors) {
		t.Fatalf("loaded %+v %v %v", out, isDefault, err)
	}
}
