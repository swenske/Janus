package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/swenske/Janus/internal/schematic"
	"github.com/swenske/Janus/internal/variants"
)

func writeManifest(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "manifest.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// layers refuses an extension without the one it requires, with the
// image factory's message.
func TestLayersRequirements(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "bird", `{"name":"bird","version":"1","description":"d","arches":["amd64"]}`)
	writeManifest(t, dir, "prometheus-bird-exporter", `{"name":"prometheus-bird-exporter","version":"1","description":"d","arches":["amd64"],"requires":["bird"]}`)
	c, err := treeCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRequirements([]string{"bird", "prometheus-bird-exporter"}); err != nil {
		t.Errorf("both: %v", err)
	}
	want := `extension "prometheus-bird-exporter" requires the "bird" extension: choose it too`
	if err := c.CheckRequirements([]string{"prometheus-bird-exporter"}); err == nil || err.Error() != want {
		t.Errorf("alone: %v", err)
	}
	if err := catalogRequirements(c); err != nil {
		t.Errorf("catalog: %v", err)
	}
	// A release built without bird would offer an exporter no schematic
	// can have: the catalog is refused.
	c.Extensions = c.Extensions[1:]
	if err := catalogRequirements(c); err == nil {
		t.Error("a catalog without the requirement was accepted")
	}
}

// Every manifest of this tree is valid and names requirements this tree
// has, by their current names.
func TestTreeRequirements(t *testing.T) {
	c, err := treeCatalog(filepath.Join("..", "..", "extensions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Extensions) == 0 {
		t.Fatal("no extension found")
	}
	if err := catalogRequirements(c); err != nil {
		t.Error(err)
	}
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
}

// A catalog without arm64 (raspberry-pi suspended) offers nothing for it,
// and the factory says so for every part of a schematic.
func TestDropArch(t *testing.T) {
	set, err := variants.Load(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	c := set.CatalogVariants()
	tree, err := treeCatalog(filepath.Join("..", "..", "extensions"))
	if err != nil {
		t.Fatal(err)
	}
	c.Extensions = tree.Extensions
	dropArch(c, "arm64")
	for _, v := range append(append([]schematic.Variant{}, c.HAProxy...), c.Kernel...) {
		if !slices.Equal(v.Arches, []string{"amd64"}) {
			t.Errorf("%s: arches %v", v.Name, v.Arches)
		}
	}
	for _, e := range c.Extensions {
		if !slices.Equal(e.Arches, []string{"amd64"}) {
			t.Errorf("%s: arches %v", e.Name, e.Arches)
		}
	}
	if _, err := c.Resolve(schematic.Default(), "arm64"); err == nil {
		t.Error("the default schematic resolves for arm64")
	}
	if _, err := c.Resolve(schematic.Default(), "amd64"); err != nil {
		t.Errorf("amd64: %v", err)
	}
	// The set's own catalog is left alone: arm64 builds by hand.
	for _, v := range set.CatalogVariants().Kernel {
		if v.Default && !slices.Contains(v.Arches, "arm64") {
			t.Error("dropArch changed the set's catalog")
		}
	}
}
