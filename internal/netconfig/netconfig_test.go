package netconfig

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

const full = `{
  "hostname": "lb1.example.net",
  "interfaces": [
    {"name": "wan", "mac": "52:54:00:12:34:56", "mode": "ADDRESSING_MODE_STATIC",
     "addresses": ["192.0.2.10/24", "2001:db8::10/64"], "gateway": "192.0.2.1", "gateway6": "fe80::1", "mtu": 1500},
    {"name": "eth1", "mode": "ADDRESSING_MODE_NONE"},
    {"name": "eth1.100", "vlan": {"parent": "eth1", "id": 100}, "mode": "ADDRESSING_MODE_STATIC", "addresses": ["10.10.0.5/24"], "gateway": "10.10.0.1"},
    {"name": "eth1.200", "vlan": {"parent": "eth1", "id": 200}, "mode": "ADDRESSING_MODE_STATIC", "addresses": ["10.20.0.5/16"]}
  ],
  "dns": {"servers": ["192.0.2.53", "2001:db8::53"], "search": ["example.net"]},
  "ntp": {"servers": ["ntp1.example.net", "192.0.2.123:1123"]}
}`

func TestParseFullConfig(t *testing.T) {
	cfg, err := Parse([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GetHostname() != "lb1.example.net" || len(cfg.GetInterfaces()) != 4 {
		t.Fatalf("parsed %v", cfg)
	}
	if dhcp, _ := Parse([]byte(`{"interfaces": [{"name": "eth0"}]}`)); dhcp.GetInterfaces()[0].GetMode() != janusv1alpha1.AddressingMode_ADDRESSING_MODE_DHCP {
		t.Error("an interface without a mode should be DHCP")
	}
	// Round trip through the stored form.
	data, err := Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse(data)
	if err != nil || !Equal(cfg, again) {
		t.Fatalf("round trip: %v\n%s", err, data)
	}
	if !strings.Contains(string(data), `"addresses"`) || !strings.Contains(string(data), `"gateway6"`) {
		t.Errorf("stored form isn't snake_case:\n%s", data)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":       `{"hostnme": "x"}`,
		"bad hostname":        `{"hostname": "-lb"}`,
		"long hostname":       `{"hostname": "` + strings.Repeat("a", 65) + `"}`,
		"empty name":          `{"interfaces": [{"mode": "ADDRESSING_MODE_NONE"}]}`,
		"long name":           `{"interfaces": [{"name": "abcdefghijklmnop"}]}`,
		"slash in name":       `{"interfaces": [{"name": "eth/0"}]}`,
		"listed twice":        `{"interfaces": [{"name": "eth0"}, {"name": "eth0"}]}`,
		"bad mac":             `{"interfaces": [{"name": "eth0", "mac": "zz"}]}`,
		"same mac twice":      `{"interfaces": [{"name": "a", "mac": "52:54:00:00:00:01"}, {"name": "b", "mac": "52:54:00:00:00:01"}]}`,
		"vlan id 0":           `{"interfaces": [{"name": "v", "mode": "ADDRESSING_MODE_NONE", "vlan": {"parent": "eth0", "id": 0}}]}`,
		"vlan id 4095":        `{"interfaces": [{"name": "v", "mode": "ADDRESSING_MODE_NONE", "vlan": {"parent": "eth0", "id": 4095}}]}`,
		"vlan no parent":      `{"interfaces": [{"name": "v", "mode": "ADDRESSING_MODE_NONE", "vlan": {"id": 10}}]}`,
		"vlan own parent":     `{"interfaces": [{"name": "v", "mode": "ADDRESSING_MODE_NONE", "vlan": {"parent": "v", "id": 10}}]}`,
		"stacked vlan":        `{"interfaces": [{"name": "a", "mode": "ADDRESSING_MODE_NONE", "vlan": {"parent": "eth0", "id": 10}}, {"name": "b", "mode": "ADDRESSING_MODE_NONE", "vlan": {"parent": "a", "id": 20}}]}`,
		"vlan on disabled":    `{"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_DISABLED"}, {"name": "v", "mode": "ADDRESSING_MODE_NONE", "vlan": {"parent": "eth0", "id": 20}}]}`,
		"duplicate vlan":      `{"interfaces": [{"name": "a", "mode": "ADDRESSING_MODE_NONE", "vlan": {"parent": "eth0", "id": 10}}, {"name": "b", "mode": "ADDRESSING_MODE_NONE", "vlan": {"parent": "eth0", "id": 10}}]}`,
		"vlan with mac":       `{"interfaces": [{"name": "a", "mac": "52:54:00:00:00:01", "mode": "ADDRESSING_MODE_NONE", "vlan": {"parent": "eth0", "id": 10}}]}`,
		"two DHCP interfaces": `{"interfaces": [{"name": "eth0"}, {"name": "eth1"}]}`,
		"DHCP on a VLAN":      `{"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_NONE"}, {"name": "v", "vlan": {"parent": "eth0", "id": 10}}]}`,
		"mtu too small":       `{"interfaces": [{"name": "eth0", "mtu": 60}]}`,
		"dhcp with address":   `{"interfaces": [{"name": "eth0", "addresses": ["192.0.2.1/24"]}]}`,
		"static no address":   `{"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC"}]}`,
		"not CIDR":            `{"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.0.2.1"]}]}`,
		"loopback address":    `{"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["127.0.0.2/8"]}]}`,
		"address twice":       `{"interfaces": [{"name": "a", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["10.0.0.1/24"]}, {"name": "b", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["10.0.0.1/24"]}]}`,
		"gateway off-link":    `{"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.0.2.10/24"], "gateway": "198.51.100.1"}]}`,
		"gateway not v4":      `{"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["192.0.2.10/24"], "gateway": "2001:db8::1"}]}`,
		"gateway6 off-link":   `{"interfaces": [{"name": "eth0", "mode": "ADDRESSING_MODE_STATIC", "addresses": ["2001:db8::10/64"], "gateway6": "2001:db9::1"}]}`,
		"too many dns":        `{"dns": {"servers": ["1.1.1.1", "8.8.8.8", "9.9.9.9", "1.0.0.1"]}}`,
		"dns not an IP":       `{"dns": {"servers": ["dns.example"]}}`,
		"bad search":          `{"dns": {"search": ["exa mple"]}}`,
		"too many ntp":        `{"ntp": {"servers": ["a.example", "b.example", "c.example"]}}`,
		"ntp bad port":        `{"ntp": {"servers": ["a.example:0"]}}`,
		"ntp bad host":        `{"ntp": {"servers": ["bad_host"]}}`,
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted %s", name, doc)
		}
	}
}

func TestValidateAcceptsEmptyAndDHCP(t *testing.T) {
	for _, doc := range []string{`{}`, `{"interfaces": [{"name": "eth0"}]}`, `{"ntp": {"servers": ["[2001:db8::123]:123", "2001:db8::124"]}}`} {
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("%s: %v", doc, err)
		}
	}
}

func TestSaveLoad(t *testing.T) {
	Dir = t.TempDir()
	cfg, isDefault, err := Load()
	if err != nil || !isDefault || len(cfg.GetInterfaces()) != 0 || Exists() {
		t.Fatalf("empty dir: %v %v %v", cfg, isDefault, err)
	}
	want, _ := Parse([]byte(full))
	if err := Save(want); err != nil {
		t.Fatal(err)
	}
	got, isDefault, err := Load()
	if err != nil || isDefault || !Equal(got, want) || !Exists() {
		t.Fatalf("after Save: %v %v %v", got, isDefault, err)
	}
	entries, _ := os.ReadDir(Dir)
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
	if err := Save(&janusv1alpha1.NetworkConfig{Hostname: "-bad"}); err == nil {
		t.Error("an invalid configuration was saved")
	}
	if err := os.WriteFile(filepath.Join(Dir, FileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(); err == nil {
		t.Error("a corrupt stored configuration loaded")
	}
}

func TestEffective(t *testing.T) {
	empty := &janusv1alpha1.NetworkConfig{}
	if s, src := EffectiveNTP(empty, nil); !reflect.DeepEqual(s, []string{"pool.ntp.org"}) || src != SourceDefault {
		t.Errorf("no DHCP: %v %s", s, src)
	}
	if s, src := EffectiveNTP(empty, []string{"10.0.0.1", "10.0.0.1", "10.0.0.2", "10.0.0.3"}); !reflect.DeepEqual(s, []string{"10.0.0.1", "10.0.0.2"}) || src != SourceDHCP {
		t.Errorf("DHCP: %v %s", s, src)
	}
	cfg, _ := Parse([]byte(full))
	if s, src := EffectiveNTP(cfg, []string{"10.0.0.1"}); len(s) != 2 || src != SourceConfigured {
		t.Errorf("configured: %v %s", s, src)
	}

	servers, search := EffectiveDNS(empty, []string{"10.0.0.53"}, []string{"lan"})
	if !reflect.DeepEqual(servers, []string{"10.0.0.53"}) || !reflect.DeepEqual(search, []string{"lan"}) {
		t.Errorf("DHCP DNS: %v %v", servers, search)
	}
	servers, search = EffectiveDNS(cfg, []string{"10.0.0.53"}, []string{"lan"})
	if !reflect.DeepEqual(servers, []string{"192.0.2.53", "2001:db8::53"}) || !reflect.DeepEqual(search, []string{"example.net"}) {
		t.Errorf("configured DNS: %v %v", servers, search)
	}
	rc := string(ResolvConf(servers, search))
	if !strings.Contains(rc, "search example.net\n") || !strings.Contains(rc, "nameserver 2001:db8::53\n") {
		t.Errorf("resolv.conf:\n%s", rc)
	}
}

func TestDefaultHostname(t *testing.T) {
	mac, _ := net.ParseMAC("52:54:00:ab:cd:ef")
	if h := DefaultHostname(mac); h != "janus-abcdef" {
		t.Errorf("DefaultHostname = %s", h)
	}
	if err := ValidateHostname(DefaultHostname(mac)); err != nil {
		t.Error(err)
	}
}

func TestSplitNTPServer(t *testing.T) {
	for in, want := range map[string]string{
		"pool.ntp.org":        "pool.ntp.org:123",
		"10.0.2.2:1123":       "10.0.2.2:1123",
		"2001:db8::1":         "2001:db8::1:123",
		"[2001:db8::1]:10123": "2001:db8::1:10123",
	} {
		h, p, err := SplitNTPServer(in)
		if err != nil || h+":"+strconv.Itoa(p) != want {
			t.Errorf("%s -> %s %d %v, want %s", in, h, p, err, want)
		}
	}
}
