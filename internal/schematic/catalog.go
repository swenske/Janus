package schematic

import (
	"encoding/json"
	"fmt"
	"slices"
)

// Catalog lists the extensions a release can build a schematic with. A
// release publishes it (schematic-catalog.json); the build refuses a
// schematic naming anything else.
type Catalog struct {
	// Version is the Janus release the catalog belongs to.
	Version    string         `json:"version"`
	Extensions []CatalogEntry `json:"extensions"`
}

type CatalogEntry struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Arches      []string `json:"arches"`
	// URL of the upstream project, for the builder's description.
	Homepage string `json:"homepage,omitempty"`
	// Replaces lists the names the extension had before: a schematic
	// naming one gets this extension instead (Migrate).
	Replaces []string `json:"replaces,omitempty"`
}

// ParseCatalog reads a catalog.
func ParseCatalog(data []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	for _, e := range c.Extensions {
		if !ValidName(e.Name) {
			return nil, fmt.Errorf("catalog: invalid extension name %q", e.Name)
		}
		for _, old := range e.Replaces {
			if !ValidName(old) {
				return nil, fmt.Errorf("catalog: %s: invalid former name %q", e.Name, old)
			}
		}
	}
	return &c, nil
}

// Lookup returns the entry for name.
func (c *Catalog) Lookup(name string) (CatalogEntry, bool) {
	i := slices.IndexFunc(c.Extensions, func(e CatalogEntry) bool { return e.Name == name })
	if i < 0 {
		return CatalogEntry{}, false
	}
	return c.Extensions[i], true
}

// Check verifies that every extension s names is in the catalog and
// available for arch.
func (c *Catalog) Check(s *Schematic, arch string) error {
	for _, name := range s.Extensions() {
		e, ok := c.Lookup(name)
		if !ok {
			return fmt.Errorf("extension %q isn't available in Janus %s", name, c.Version)
		}
		if !slices.Contains(e.Arches, arch) {
			return fmt.Errorf("extension %q isn't available for %s (only %v)", name, arch, e.Arches)
		}
	}
	return nil
}

// Migrate returns s with the extensions the catalog renamed under their
// new names, and the renames it made (old name -> new). A name the
// catalog still offers, or knows nothing about, is kept - Check says
// whether the result can be built.
func (c *Catalog) Migrate(s *Schematic) (*Schematic, map[string]string) {
	renamed := map[string]string{}
	exts := []string{}
	for _, name := range s.Extensions() {
		if _, ok := c.Lookup(name); !ok {
			for _, e := range c.Extensions {
				if slices.Contains(e.Replaces, name) {
					renamed[name] = e.Name
					name = e.Name
					break
				}
			}
		}
		exts = append(exts, name)
	}
	m := &Schematic{Customization: Customization{Extensions: exts}}
	_ = m.Normalize() // names from a schematic and a parsed catalog: valid
	return m, renamed
}
