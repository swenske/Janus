package variants

import (
	"strings"
	"testing"

	"github.com/swenske/Janus/internal/schematic"
)

// TestRepository: the tree's own variants.mk and versions.mk agree.
func TestRepository(t *testing.T) {
	s, err := Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Default(schematic.ComponentHAProxy).Default || s.Default(schematic.ComponentHAProxy).Name != s.HAProxy[0].Name {
		t.Errorf("the default HAProxy branch isn't the newest: %+v", s.HAProxy)
	}
	if s.Default(schematic.ComponentKernel).Version == "" {
		t.Errorf("no default kernel track: %+v", s.Kernel)
	}
}

const pins = `
HAPROXY_3_4_VERSION := 3.4.6
HAPROXY_3_4_SHA256  := aa
HAPROXY_3_2_VERSION := 3.2.25
HAPROXY_3_2_SHA256  := bb
KERNEL_STABLE_VERSION := 7.2.9
KERNEL_STABLE_SHA256  := cc
KERNEL_LONGTERM_VERSION := 6.18.55
KERNEL_LONGTERM_SHA256  := dd
`

const good = `
# comment := ignored
HAPROXY_BRANCHES := 3.4 3.2
HAPROXY_3_4_EOL  := 2031-04-01
HAPROXY_3_2_EOL  := 2030-04-01
KERNEL_TRACKS        := stable longterm
KERNEL_DEFAULT_TRACK := stable
VARIANTS_RETIRED := haproxy:3.0:v2027.06.01
`

func TestParse(t *testing.T) {
	s, err := Parse([]byte(good), []byte(pins))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.HAProxy) != 2 || !s.HAProxy[0].Default || s.HAProxy[1].Default || s.HAProxy[1].SHA256 != "bb" || s.HAProxy[1].EOL != "2030-04-01" {
		t.Errorf("HAProxy: %+v", s.HAProxy)
	}
	if d := s.Default(schematic.ComponentKernel); d.Name != "stable" || d.Version != "7.2.9" {
		t.Errorf("default kernel: %+v", d)
	}
	if len(s.Retired) != 1 || s.Retired[0].LastRelease != "v2027.06.01" {
		t.Errorf("retired: %+v", s.Retired)
	}
	sc, _ := schematic.Parse([]byte(`{"customization":{"haproxy":"3.2","kernel":"longterm"}}`))
	h, k, err := s.Resolve(sc, "amd64")
	if err != nil || h.Version != "3.2.25" || k.Version != "6.18.55" {
		t.Errorf("resolve: %+v %+v %v", h, k, err)
	}
	if _, _, err := s.Resolve(sc, "arm64"); err == nil {
		t.Error("arm64 got a variant other than the defaults")
	}
	h, k, err = s.Resolve(schematic.Default(), "arm64")
	if err != nil || h.Name != "3.4" || k.Name != "stable" {
		t.Errorf("arm64 defaults: %+v %+v %v", h, k, err)
	}
	c := s.CatalogVariants()
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	if VersionVar(schematic.ComponentHAProxy, "3.2") != "HAPROXY_3_2_VERSION" || SumVar(schematic.ComponentKernel, "longterm") != "KERNEL_LONGTERM_SHA256" {
		t.Error("pin names")
	}
}

func TestParseRefuses(t *testing.T) {
	for name, edit := range map[string][2]string{
		"no pin":        {"3.4 3.2", "3.4 3.2 3.0"},
		"oldest first":  {"3.4 3.2", "3.2 3.4"},
		"no EOL":        {"HAPROXY_3_2_EOL  := 2030-04-01", ""},
		"bad EOL":       {"2030-04-01", "Q2 2030"},
		"bad branch":    {"3.4 3.2", "3.4 3.2.1"},
		"default track": {"KERNEL_DEFAULT_TRACK := stable", "KERNEL_DEFAULT_TRACK := mainline"},
		"retired twice": {"haproxy:3.0:", "haproxy:3.2:"},
		"retired form":  {"haproxy:3.0:v2027.06.01", "haproxy-3.0"},
	} {
		doc := strings.Replace(good, edit[0], edit[1], 1)
		if doc == good {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		if _, err := Parse([]byte(doc), []byte(pins)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A pin on another branch than its name.
	if _, err := Parse([]byte(good), []byte(strings.Replace(pins, "3.2.25", "3.4.7", 1))); err == nil {
		t.Error("HAPROXY_3_2_VERSION = 3.4.7 accepted")
	}
	if _, err := Parse([]byte("HAPROXY_BRANCHES :=\nKERNEL_TRACKS := stable\nKERNEL_DEFAULT_TRACK := stable\n"), []byte(pins)); err == nil {
		t.Error("no HAProxy branch accepted")
	}
}
