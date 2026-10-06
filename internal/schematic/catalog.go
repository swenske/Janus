package schematic

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Catalog lists what a release can build a schematic with: its
// extensions, HAProxy branches and kernel tracks. A release publishes it
// (schematic-catalog.json); the build refuses a schematic naming anything
// else.
type Catalog struct {
	// Version is the Janus release the catalog belongs to.
	Version    string         `json:"version"`
	Extensions []CatalogEntry `json:"extensions"`
	// HAProxy lists the HAProxy branches the release is built with, Kernel
	// its kernel tracks, one of each marked default. Absent: a release
	// from before images could choose them - its only HAProxy and kernel
	// are the defaults, and a schematic can't name one.
	HAProxy []Variant `json:"haproxy,omitempty"`
	Kernel  []Variant `json:"kernel,omitempty"`
	// Retired lists the branches and tracks earlier releases offered and
	// this one no longer does, with the last release that had each: an
	// image built with one stays on it (no automatic move to another
	// branch), and is told where its updates stopped.
	Retired []Retired `json:"retired,omitempty"`
}

// Components a schematic can choose a variant of.
const (
	ComponentHAProxy = "haproxy"
	ComponentKernel  = "kernel"
)

// Variant is a HAProxy branch or a kernel track a release offers.
type Variant struct {
	// Name is what a schematic names: "3.2", "stable".
	Name string `json:"name"`
	// Version is what the release ships for it: "3.2.25", "7.2.9".
	Version string `json:"version"`
	// Default marks what a schematic naming none of them gets.
	Default bool `json:"default,omitempty"`
	// EOL is the date upstream stops supporting it (YYYY-MM-DD), when it
	// has one: HAProxy branches do, kernel tracks move on by themselves.
	EOL    string   `json:"eol,omitempty"`
	Arches []string `json:"arches"`
}

// Retired is a variant a release no longer offers.
type Retired struct {
	Component string `json:"component"`
	Name      string `json:"name"`
	// LastRelease is the newest release that offered it.
	LastRelease string `json:"last_release"`
}

// RetiredError says a schematic names a variant the catalog's release no
// longer offers.
type RetiredError struct {
	Retired
	Release string
}

func (e *RetiredError) Error() string {
	return fmt.Sprintf("%s %s is no longer offered by Janus %s - the last release with it is %s",
		componentTitle(e.Component), e.Name, e.Release, e.LastRelease)
}

func componentTitle(c string) string {
	if c == ComponentHAProxy {
		return "HAProxy"
	}
	return "the kernel track"
}

// Resolved is what a schematic gets from a release.
type Resolved struct {
	// HAProxy and Kernel are the variants the image is built with; zero
	// when the release offers no choice (a catalog from before variants).
	HAProxy Variant
	Kernel  Variant
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
	if err := checkVariants(ComponentHAProxy, c.HAProxy, ValidHAProxyBranch); err != nil {
		return nil, err
	}
	if err := checkVariants(ComponentKernel, c.Kernel, ValidKernelTrack); err != nil {
		return nil, err
	}
	for _, r := range c.Retired {
		valid := ValidKernelTrack
		switch r.Component {
		case ComponentHAProxy:
			valid = ValidHAProxyBranch
		case ComponentKernel:
		default:
			return nil, fmt.Errorf("catalog: retired: unknown component %q", r.Component)
		}
		if !valid(r.Name) {
			return nil, fmt.Errorf("catalog: retired: invalid %s %q", r.Component, r.Name)
		}
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

func checkVariants(component string, vs []Variant, valid func(string) bool) error {
	defaults := 0
	seen := map[string]bool{}
	for _, v := range vs {
		if !valid(v.Name) {
			return fmt.Errorf("catalog: invalid %s %q", component, v.Name)
		}
		if seen[v.Name] {
			return fmt.Errorf("catalog: %s %s listed twice", component, v.Name)
		}
		seen[v.Name] = true
		if v.Default {
			defaults++
		}
	}
	if len(vs) > 0 && defaults != 1 {
		return fmt.Errorf("catalog: %d default %s variants, want exactly one", defaults, component)
	}
	return nil
}

// Check verifies that the release can build s for arch: every extension
// it names, and its HAProxy branch and kernel track.
func (c *Catalog) Check(s *Schematic, arch string) error {
	_, err := c.Resolve(s, arch)
	return err
}

// Resolve checks s like Check and says which HAProxy branch and kernel
// track the image gets. A variant the release no longer offers is a
// *RetiredError.
func (c *Catalog) Resolve(s *Schematic, arch string) (*Resolved, error) {
	for _, name := range s.Extensions() {
		e, ok := c.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("extension %q isn't available in Janus %s", name, c.Version)
		}
		if !slices.Contains(e.Arches, arch) {
			return nil, fmt.Errorf("extension %q isn't available for %s (only %v)", name, arch, e.Arches)
		}
	}
	var r Resolved
	var err error
	if r.HAProxy, err = c.variant(ComponentHAProxy, c.HAProxy, s.HAProxyBranch(), arch); err != nil {
		return nil, err
	}
	if r.Kernel, err = c.variant(ComponentKernel, c.Kernel, s.KernelTrack(), arch); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Catalog) variant(component string, offered []Variant, name, arch string) (Variant, error) {
	title := componentTitle(component)
	if len(offered) == 0 {
		if name != "" {
			noun := "HAProxy branch"
			if component == ComponentKernel {
				noun = "kernel track"
			}
			return Variant{}, fmt.Errorf("no %s can be chosen in Janus %s", noun, c.Version)
		}
		return Variant{}, nil
	}
	i := slices.IndexFunc(offered, func(v Variant) bool {
		if name == "" {
			return v.Default
		}
		return v.Name == name
	})
	if i < 0 {
		if j := slices.IndexFunc(c.Retired, func(r Retired) bool { return r.Component == component && r.Name == name }); j >= 0 {
			return Variant{}, &RetiredError{Retired: c.Retired[j], Release: c.Version}
		}
		names := make([]string, len(offered))
		for k, v := range offered {
			names[k] = v.Name
		}
		return Variant{}, fmt.Errorf("%s %s isn't offered by Janus %s (only %s)", title, name, c.Version, strings.Join(names, ", "))
	}
	v := offered[i]
	if !slices.Contains(v.Arches, arch) {
		return Variant{}, fmt.Errorf("%s %s isn't available for %s (only %v)", title, v.Name, arch, v.Arches)
	}
	return v, nil
}

// LookupVariant returns the variant of component named name ("" for the
// default) the release offers.
func (c *Catalog) LookupVariant(component, name string) (Variant, bool) {
	vs := c.HAProxy
	if component == ComponentKernel {
		vs = c.Kernel
	}
	i := slices.IndexFunc(vs, func(v Variant) bool { return (name == "" && v.Default) || (name != "" && v.Name == name) })
	if i < 0 {
		return Variant{}, false
	}
	return vs[i], true
}

// Migrate returns s with the extensions the catalog renamed under their
// new names, and the renames it made (old name -> new). A name the
// catalog still offers, or knows nothing about, is kept - Check says
// whether the result can be built. The HAProxy branch and kernel track
// are kept as they are: a variant is never moved to another.
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
	m := s.clone()
	m.Customization.Extensions = exts
	_ = m.Normalize() // names from a schematic and a parsed catalog: valid
	return m, renamed
}
