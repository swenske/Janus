package schematic

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestIDIgnoresOrderAndDuplicates(t *testing.T) {
	a, err := Parse([]byte(`{"customization":{"extensions":["qemu-guest-agent","node-exporter"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte(`{"customization":{"extensions":["node-exporter","qemu-guest-agent","node-exporter"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != b.ID() {
		t.Errorf("same choices, different IDs: %s %s", a.ID(), b.ID())
	}
	if got := string(a.Canonical()); got != `{"customization":{"extensions":["node-exporter","qemu-guest-agent"]}}` {
		t.Errorf("canonical form: %s", got)
	}
	if a.ID() == DefaultID() {
		t.Error("an extension doesn't change the ID")
	}
}

func TestDefault(t *testing.T) {
	for _, doc := range []string{`{"customization":{}}`, `{"customization":{"extensions":[]}}`, `{}`} {
		s, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		if s.ID() != DefaultID() {
			t.Errorf("%s isn't the default schematic", doc)
		}
	}
	// Pinned: changing the canonical form would change every ID already
	// issued, and nodes would no longer find their updates.
	if DefaultID() != "a055fbb697e2d0abb0c5911e7702b07040f49eb71befeaf9b90495a905327f47" {
		t.Errorf("default ID changed: %s", DefaultID())
	}
	if got := string(Default().Canonical()); got != `{"customization":{}}` {
		t.Errorf("default canonical form: %s", got)
	}
}

func TestParseRefuses(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown field":     `{"customization":{"extensions":["a"]},"overlay":{}}`,
		"unknown nested":    `{"customization":{"kernelArgs":["x"]}}`,
		"bad name":          `{"customization":{"extensions":["Node_Exporter"]}}`,
		"path in name":      `{"customization":{"extensions":["../etc"]}}`,
		"trailing data":     `{"customization":{}} {}`,
		"not json":          `customization: {}`,
		"leading dash name": `{"customization":{"extensions":["-x"]}}`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var many []string
	for i := 0; i < MaxExtensions+1; i++ {
		many = append(many, `"e`+strings.Repeat("x", i)+`"`)
	}
	if _, err := Parse([]byte(`{"customization":{"extensions":[` + strings.Join(many, ",") + `]}}`)); err == nil {
		t.Error("too many extensions accepted")
	}
}

func TestFromCmdline(t *testing.T) {
	id := strings.Repeat("ab", 32)
	if got, ok := FromCmdline("console=ttyS0 " + CmdlineArg(id) + " ro"); !ok || got != id {
		t.Errorf("got %q %v", got, ok)
	}
	if got, ok := FromCmdline("console=ttyS0 ro"); !ok || got != DefaultID() {
		t.Errorf("no parameter: got %q %v, want the default schematic", got, ok)
	}
	if _, ok := FromCmdline("janus.schematic=nothex"); ok {
		t.Error("malformed ID accepted")
	}
}

func TestYAML(t *testing.T) {
	s, _ := Parse([]byte(`{"customization":{"extensions":["qemu-guest-agent","node-exporter"]}}`))
	want := "customization:\n  extensions:\n    - node-exporter\n    - qemu-guest-agent\n"
	if s.YAML() != want {
		t.Errorf("YAML:\n%s", s.YAML())
	}
	if Default().YAML() != "customization: {}\n" {
		t.Errorf("default YAML: %q", Default().YAML())
	}
}

func TestCatalogCheck(t *testing.T) {
	c, err := ParseCatalog([]byte(`{"version":"v1","extensions":[
		{"name":"node-exporter","arches":["amd64","arm64"]},
		{"name":"qemu-guest-agent","arches":["amd64"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	both, _ := Parse([]byte(`{"customization":{"extensions":["node-exporter","qemu-guest-agent"]}}`))
	if err := c.Check(both, "amd64"); err != nil {
		t.Errorf("amd64: %v", err)
	}
	if err := c.Check(both, "arm64"); err == nil {
		t.Error("qemu-guest-agent accepted on arm64")
	}
	unknown, _ := Parse([]byte(`{"customization":{"extensions":["bird"]}}`))
	if err := c.Check(unknown, "amd64"); err == nil {
		t.Error("an extension outside the catalog accepted")
	}
	if err := c.Check(Default(), "arm64"); err != nil {
		t.Errorf("default schematic: %v", err)
	}
}

func TestCatalogMigrate(t *testing.T) {
	c, err := ParseCatalog([]byte(`{"version":"v2","extensions":[
		{"name":"prometheus-node-exporter","arches":["amd64"],"replaces":["node-exporter"]},
		{"name":"qemu-guest-agent","arches":["amd64"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	old := &Schematic{Customization: Customization{Extensions: []string{"qemu-guest-agent", "node-exporter"}}}
	m, renamed := c.Migrate(old)
	if got := m.Extensions(); len(got) != 2 || got[0] != "prometheus-node-exporter" || got[1] != "qemu-guest-agent" {
		t.Fatalf("migrated = %v", got)
	}
	if len(renamed) != 1 || renamed["node-exporter"] != "prometheus-node-exporter" {
		t.Fatalf("renamed = %v", renamed)
	}
	if err := c.Check(m, "amd64"); err != nil {
		t.Fatalf("the migrated schematic doesn't build: %v", err)
	}
	if m.ID() == old.ID() {
		t.Fatal("a rename must give another schematic")
	}

	// Nothing to rename: the same schematic; an unknown name stays.
	cur := &Schematic{Customization: Customization{Extensions: []string{"prometheus-node-exporter", "unknown"}}}
	m, renamed = c.Migrate(cur)
	if len(renamed) != 0 || m.ID() != cur.ID() {
		t.Fatalf("migrated %v -> %v (%v)", cur.Extensions(), m.Extensions(), renamed)
	}

	if _, err := ParseCatalog([]byte(`{"version":"v2","extensions":[{"name":"a","replaces":["Not Valid"]}]}`)); err == nil {
		t.Fatal("an invalid former name was accepted")
	}
}

func TestVariantIDs(t *testing.T) {
	// Pinned like the default ID: the canonical form puts the fields in
	// Customization's order and leaves out what isn't set.
	for doc, want := range map[string]string{
		`{"customization":{"haproxy":"3.2"}}`:                                                       "a61a31f5721c12f561a397efd26a2e4b481c4ccc3506c17d54d868b070767dc1",
		`{"customization":{"kernel":"longterm"}}`:                                                   "b38088e282bf172ceeb50fb1e7e9f8dd5342a5741a335917da2c9ad87e465541",
		`{"customization":{"kernel":"longterm","haproxy":"3.0","extensions":["qemu-guest-agent"]}}`: "5cafac9ce1190f1c2ebb845d7d6d13007f056a9ddf135a5d57bab4211a0b9f37",
	} {
		s, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", doc, err)
		}
		if s.ID() != want {
			t.Errorf("%s: ID %s, canonical %s", doc, s.ID(), s.Canonical())
		}
	}
	s, _ := Parse([]byte(`{"customization":{"kernel":"longterm","haproxy":"3.0","extensions":["qemu-guest-agent"]}}`))
	if got := string(s.Canonical()); got != `{"customization":{"extensions":["qemu-guest-agent"],"haproxy":"3.0","kernel":"longterm"}}` {
		t.Errorf("canonical form: %s", got)
	}
	// Unset and empty are the same: the release's default.
	empty, _ := Parse([]byte(`{"customization":{"haproxy":"","kernel":""}}`))
	if empty.ID() != DefaultID() {
		t.Error("empty variants aren't the default schematic")
	}
	// A variant equal to today's default stays a choice of its own: the
	// default moves, the ID mustn't.
	stable, _ := Parse([]byte(`{"customization":{"kernel":"stable"}}`))
	if stable.ID() == DefaultID() {
		t.Error("an explicit kernel track was dropped")
	}
}

func TestParseRefusesVariants(t *testing.T) {
	for _, doc := range []string{
		`{"customization":{"haproxy":"3.2.1"}}`,
		`{"customization":{"haproxy":"3"}}`,
		`{"customization":{"haproxy":"03.2"}}`,
		`{"customization":{"haproxy":"latest"}}`,
		`{"customization":{"haproxy":3.2}}`,
		`{"customization":{"kernel":"Stable"}}`,
		`{"customization":{"kernel":"6.18"}}`,
		`{"customization":{"kernel":"long-term"}}`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", doc)
		}
	}
}

func TestVariantYAML(t *testing.T) {
	s, _ := Parse([]byte(`{"customization":{"extensions":["qemu-guest-agent"],"haproxy":"3.0","kernel":"longterm"}}`))
	want := "customization:\n  extensions:\n    - qemu-guest-agent\n  haproxy: \"3.0\"\n  kernel: longterm\n"
	if s.YAML() != want {
		t.Errorf("YAML:\n%s", s.YAML())
	}
	k, _ := Parse([]byte(`{"customization":{"kernel":"longterm"}}`))
	if k.YAML() != "customization:\n  kernel: longterm\n" {
		t.Errorf("YAML:\n%s", k.YAML())
	}
}

const variantCatalog = `{"version":"v2","extensions":[
	{"name":"qemu-guest-agent","arches":["amd64"]},
	{"name":"prometheus-node-exporter","arches":["amd64","arm64"],"replaces":["node-exporter"]}],
	"haproxy":[
		{"name":"3.4","version":"3.4.6","default":true,"eol":"2031-04-01","arches":["amd64","arm64"]},
		{"name":"3.2","version":"3.2.25","eol":"2030-04-01","arches":["amd64"]}],
	"kernel":[
		{"name":"stable","version":"7.2.9","default":true,"arches":["amd64","arm64"]},
		{"name":"longterm","version":"6.18.55","arches":["amd64"]}],
	"retired":[{"component":"haproxy","name":"3.0","last_release":"v1"}]}`

func TestCatalogResolve(t *testing.T) {
	c, err := ParseCatalog([]byte(variantCatalog))
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Resolve(Default(), "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if r.HAProxy.Version != "3.4.6" || r.Kernel.Version != "7.2.9" {
		t.Errorf("default: %+v", r)
	}
	pinned, _ := Parse([]byte(`{"customization":{"haproxy":"3.2","kernel":"longterm"}}`))
	r, err = c.Resolve(pinned, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if r.HAProxy.Name != "3.2" || r.Kernel.Name != "longterm" {
		t.Errorf("pinned: %+v", r)
	}
	if err := c.Check(pinned, "arm64"); err == nil {
		t.Error("an amd64-only variant accepted on arm64")
	}
	unknown, _ := Parse([]byte(`{"customization":{"haproxy":"2.6"}}`))
	if err := c.Check(unknown, "amd64"); err == nil || !strings.Contains(err.Error(), "only 3.4, 3.2") {
		t.Errorf("unknown branch: %v", err)
	}
	retired, _ := Parse([]byte(`{"customization":{"haproxy":"3.0"}}`))
	var re *RetiredError
	if err := c.Check(retired, "amd64"); !errors.As(err, &re) || re.LastRelease != "v1" {
		t.Errorf("retired branch: %v", err)
	}

	// A catalog from before variants: only the default image, which a
	// schematic can't name.
	old, _ := ParseCatalog([]byte(`{"version":"v1","extensions":[{"name":"qemu-guest-agent","arches":["amd64"]}]}`))
	if r, err := old.Resolve(Default(), "amd64"); err != nil || r.HAProxy.Name != "" {
		t.Errorf("old catalog, default: %+v %v", r, err)
	}
	if err := old.Check(pinned, "amd64"); err == nil {
		t.Error("an old catalog accepted a HAProxy branch")
	}

	// Migrate renames extensions and never moves a variant.
	m, renamed := c.Migrate(&Schematic{Customization: Customization{Extensions: []string{"node-exporter"}, HAProxy: "3.2"}})
	if renamed["node-exporter"] != "prometheus-node-exporter" || m.HAProxyBranch() != "3.2" {
		t.Errorf("migrated: %s %v", m.Canonical(), renamed)
	}
}

func TestParseCatalogRefusesVariants(t *testing.T) {
	for name, doc := range map[string]string{
		"no default":       `{"version":"v","haproxy":[{"name":"3.4","arches":["amd64"]}]}`,
		"two defaults":     `{"version":"v","kernel":[{"name":"stable","default":true},{"name":"longterm","default":true}]}`,
		"bad branch":       `{"version":"v","haproxy":[{"name":"3.4.6","default":true}]}`,
		"listed twice":     `{"version":"v","haproxy":[{"name":"3.4","default":true},{"name":"3.4"}]}`,
		"retired unknown":  `{"version":"v","retired":[{"component":"openssl","name":"3.5","last_release":"v1"}]}`,
		"retired bad name": `{"version":"v","retired":[{"component":"haproxy","name":"x","last_release":"v1"}]}`,
	} {
		if _, err := ParseCatalog([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestImageInfo(t *testing.T) {
	c, _ := ParseCatalog([]byte(variantCatalog))
	sc, _ := Parse([]byte(`{"customization":{"extensions":["qemu-guest-agent"],"kernel":"longterm"}}`))
	r, err := c.Resolve(sc, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	info := NewImageInfo(sc, r, "v2", "amd64")
	// As extpack writes it: indented, the schematic too.
	data, _ := json.MarshalIndent(info, "", "  ")
	got, gotSc, err := ParseImageInfo(data)
	if err != nil {
		t.Fatal(err)
	}
	if gotSc.ID() != sc.ID() || string(got.Schematic) != string(sc.Canonical()) || got.HAProxy != (ImageComponent{Variant: "3.4", Version: "3.4.6", Default: true}) ||
		got.Kernel != (ImageComponent{Variant: "longterm", Version: "6.18.55", Pinned: true}) {
		t.Errorf("image info: %s", data)
	}
	// A schematic that isn't its ID's.
	bad := strings.Replace(string(data), `"kernel": "longterm"`, `"kernel": "stable"`, 1)
	if bad == string(data) {
		t.Fatal("the test's edit changed nothing")
	}
	if _, _, err := ParseImageInfo([]byte(bad)); err == nil {
		t.Error("a schematic that doesn't match its ID accepted")
	}
}
