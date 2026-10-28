package birdexporter

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type fakeSupervisor struct {
	args    [][]string
	enabled []bool
}

func (f *fakeSupervisor) Configure(id string, args []string, enabled bool) error {
	if id != ServiceID {
		panic(id)
	}
	f.args, f.enabled = append(f.args, args), append(f.enabled, enabled)
	return nil
}

func (f *fakeSupervisor) SetArgs(id string, args []string) error {
	if id != ServiceID {
		panic(id)
	}
	f.args = append(f.args, args)
	return nil
}

func useDir(t *testing.T) {
	t.Helper()
	prev := Dir
	Dir = t.TempDir()
	t.Cleanup(func() { Dir = prev })
}

// Auto listens on the main interface's IPv4 address, else its IPv6 one;
// BIRD is read over the restricted socket only, OSPF left out.
func TestArgs(t *testing.T) {
	v4, v6 := netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8::10")
	def := DefaultConfig()
	for _, c := range []struct {
		cfg  Config
		main []netip.Addr
		want string
	}{
		{def, []netip.Addr{v6, v4}, "-web.listen-address=192.0.2.10:9324"},
		{def, []netip.Addr{v6}, "-web.listen-address=[2001:db8::10]:9324"},
		{def, nil, "-web.listen-address=:9324"},
		{Config{Enabled: true, Address: "198.51.100.7", Port: 9400}, []netip.Addr{v4}, "-web.listen-address=198.51.100.7:9400"},
		{Config{Enabled: true, Address: "", Port: 9324}, []netip.Addr{v4}, "-web.listen-address=:9324"},
	} {
		args := Args(c.cfg, c.main)
		if args[0] != c.want {
			t.Errorf("%+v %v: %v", c.cfg, c.main, args)
		}
		for _, a := range []string{"-bird.socket=/run/janus/bird/bird-ro.ctl", "-bird.v2", "-format.new", "-proto.ospf=false"} {
			if !slices.Contains(args, a) {
				t.Errorf("%+v: no %s in %v", c.cfg, a, args)
			}
		}
	}
}

func TestValidate(t *testing.T) {
	for _, c := range []Config{{Port: 70000}, {Port: 9505}, {Address: "not an address"}, {Address: "eth0"}} {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	c := Config{Address: "2001:db8::1"}
	if err := c.Validate(); err != nil || c.Port != DefaultPort {
		t.Errorf("%+v: %v", c, err)
	}
}

// Saved, loaded back, run; a stopped exporter stays stopped when the
// network changes; a broken file runs the defaults and says so.
func TestApplyBootFollow(t *testing.T) {
	useDir(t)
	v4 := netip.MustParseAddr("192.0.2.10")
	if c, isDefault, err := Load(); err != nil || !isDefault || c != DefaultConfig() {
		t.Fatalf("nothing saved: %+v %v %v", c, isDefault, err)
	}
	var s fakeSupervisor
	if err := Boot(&s, []netip.Addr{v4}); err != nil || s.args[0][0] != "-web.listen-address=192.0.2.10:9324" || !s.enabled[0] {
		t.Fatalf("Boot: %v %v %v", err, s.args, s.enabled)
	}
	if err := Follow(&s, []netip.Addr{netip.MustParseAddr("192.0.2.11")}); err != nil || s.args[1][0] != "-web.listen-address=192.0.2.11:9324" {
		t.Fatalf("Follow: %v %v", err, s.args)
	}
	off := Config{Enabled: false, Address: Auto, Port: 9324}
	if err := Apply(&s, off, []netip.Addr{v4}); err != nil || s.enabled[len(s.enabled)-1] {
		t.Fatalf("Apply disabled: %v %v", err, s.enabled)
	}
	n := len(s.args)
	if err := Follow(&s, []netip.Addr{v4}); err != nil || len(s.args) != n {
		t.Errorf("a stopped exporter was given new arguments: %v", s.args)
	}
	if c, isDefault, err := Load(); err != nil || isDefault || c != off {
		t.Errorf("loaded back: %+v %v %v", c, isDefault, err)
	}
	if err := os.WriteFile(filepath.Join(Dir, configFile), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Boot(&s, nil); err == nil || !s.enabled[len(s.enabled)-1] {
		t.Errorf("a broken file: %v, enabled %v", err, s.enabled)
	}
}
