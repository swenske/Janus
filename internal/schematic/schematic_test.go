package schematic

import (
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
