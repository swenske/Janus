package nodeexporter

import (
	"encoding/json"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
)

// The manifest's command line is what a node without saved settings
// runs before janusd configures the service: it must be the defaults -
// with no main interface known, every address.
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
	if want := Args(DefaultConfig(), nil); !slices.Equal(m.Services[0].Args, want) {
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
		{Address: "automatic"},
		{Collectors: []string{"textfile"}},
		{Collectors: []string{"cpu --web.config.file=/x"}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestArgs(t *testing.T) {
	main := []netip.Addr{netip.MustParseAddr("10.0.2.15"), netip.MustParseAddr("2001:db8::10")}
	got := Args(Config{Address: "fd00::1", Port: 9200, Collectors: []string{"cpu", "softnet"}}, main)
	want := []string{"--web.listen-address=[fd00::1]:9200", "--collector.disable-defaults", "--collector.cpu", "--collector.softnet"}
	if !slices.Equal(got, want) {
		t.Fatalf("args %v, want %v", got, want)
	}
	for _, c := range []struct {
		address string
		main    []netip.Addr
		want    []string
	}{
		{Auto, main, []string{"--web.listen-address=10.0.2.15:9100", "--web.listen-address=[2001:db8::10]:9100"}},
		{Auto, nil, []string{"--web.listen-address=:9100"}},
		{"", main, []string{"--web.listen-address=:9100"}},
	} {
		args := Args(Config{Address: c.address, Port: DefaultPort, Collectors: []string{"cpu"}}, c.main)
		if !slices.Equal(args[:len(c.want)], c.want) || args[len(c.want)] != "--collector.disable-defaults" {
			t.Errorf("%q with %v: %v", c.address, c.main, args)
		}
	}
}

type fakeSupervisor struct {
	args    []string
	enabled bool
	set     int
}

func (f *fakeSupervisor) Configure(_ string, args []string, enabled bool) error {
	f.args, f.enabled = args, enabled
	return nil
}

func (f *fakeSupervisor) SetArgs(_ string, args []string) error {
	f.args = args
	f.set++
	return nil
}

func TestFollow(t *testing.T) {
	Dir = t.TempDir()
	f := &fakeSupervisor{}
	main := []netip.Addr{netip.MustParseAddr("10.0.2.15")}
	// Nothing saved: the default, Auto - followed.
	if err := Follow(f, main); err != nil || f.set != 1 || f.args[0] != "--web.listen-address=10.0.2.15:9100" {
		t.Fatalf("default: %v %+v", err, f)
	}
	// A fixed address isn't moved, nor a disabled exporter.
	if err := Apply(f, Config{Enabled: true, Address: "192.0.2.10", Port: DefaultPort, Collectors: DefaultCollectors()}, main); err != nil {
		t.Fatal(err)
	}
	if err := Follow(f, []netip.Addr{netip.MustParseAddr("10.0.2.16")}); err != nil || f.set != 1 || f.args[0] != "--web.listen-address=192.0.2.10:9100" {
		t.Fatalf("a fixed address: %v %+v", err, f)
	}
	if err := Apply(f, Config{Enabled: false, Address: Auto, Port: DefaultPort, Collectors: DefaultCollectors()}, main); err != nil {
		t.Fatal(err)
	}
	if err := Follow(f, []netip.Addr{netip.MustParseAddr("10.0.2.16")}); err != nil || f.set != 1 || f.enabled {
		t.Fatalf("disabled: %v %+v", err, f)
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
