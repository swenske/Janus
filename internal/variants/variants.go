// Package variants reads variants.mk and versions.mk: the HAProxy branches
// and kernel tracks an image can be built with (a schematic picks one of
// each), the upstream version each one is, and which are the defaults.
// hack/extpack writes them into a release's schematic catalog and an
// image's image.json; hack/upstream follows each variant's pins.
package variants

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/schematic"
)

// Arches every image is built for; variants other than the defaults are
// built for DefaultOnlyArches' complement only.
var (
	Arches            = []string{"amd64", "arm64"}
	DefaultOnlyArches = []string{"arm64"}
)

// Variant is a HAProxy branch or a kernel track.
type Variant struct {
	Component string // schematic.ComponentHAProxy or ComponentKernel
	Name      string // "3.4", "longterm"
	Version   string // "3.4.6", "6.18.55"
	SHA256    string // of the upstream source tarball
	EOL       string // end of upstream support (YYYY-MM-DD), HAProxy only
	Default   bool
}

// Set is everything variants.mk declares.
type Set struct {
	HAProxy []Variant // newest branch first; the first is the default
	Kernel  []Variant // in variants.mk's order
	Retired []schematic.Retired
}

// VersionVar is the versions.mk variable pinning a variant's version.
func VersionVar(component, name string) string {
	if component == schematic.ComponentHAProxy {
		return "HAPROXY_" + strings.ReplaceAll(name, ".", "_") + "_VERSION"
	}
	return "KERNEL_" + strings.ToUpper(name) + "_VERSION"
}

// SumVar is the versions.mk variable pinning a variant's sha256.
func SumVar(component, name string) string {
	return strings.TrimSuffix(VersionVar(component, name), "_VERSION") + "_SHA256"
}

// assign matches a make assignment, "NAME := value..." (value possibly
// empty or several words).
var assign = regexp.MustCompile(`^([A-Za-z0-9_]+)\s*:=[ \t]*(.*?)\s*$`)

// ParseMk returns the "NAME := value" assignments of a makefile.
func ParseMk(data []byte) map[string]string {
	vars := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := assign.FindStringSubmatch(line); m != nil {
			vars[m[1]] = m[2]
		}
	}
	return vars
}

// Load reads dir's variants.mk and versions.mk.
func Load(dir string) (*Set, error) {
	vm, err := os.ReadFile(filepath.Join(dir, "variants.mk"))
	if err != nil {
		return nil, err
	}
	pins, err := os.ReadFile(filepath.Join(dir, "versions.mk"))
	if err != nil {
		return nil, err
	}
	return Parse(vm, pins)
}

// Parse reads variants.mk and versions.mk, checking they agree.
func Parse(variantsMk, versionsMk []byte) (*Set, error) {
	vars, pins := ParseMk(variantsMk), ParseMk(versionsMk)
	var s Set
	pinned := func(component, name string) (Variant, error) {
		v := Variant{Component: component, Name: name,
			Version: pins[VersionVar(component, name)], SHA256: pins[SumVar(component, name)]}
		if v.Version == "" || v.SHA256 == "" {
			return v, fmt.Errorf("variants.mk: %s %s has no %s and %s in versions.mk", component, name,
				VersionVar(component, name), SumVar(component, name))
		}
		return v, nil
	}

	branches := strings.Fields(vars["HAPROXY_BRANCHES"])
	if len(branches) == 0 {
		return nil, fmt.Errorf("variants.mk: no HAPROXY_BRANCHES")
	}
	for i, b := range branches {
		if !schematic.ValidHAProxyBranch(b) {
			return nil, fmt.Errorf("variants.mk: %q isn't a HAProxy branch", b)
		}
		if i > 0 && compareBranches(branches[i-1], b) <= 0 {
			return nil, fmt.Errorf("variants.mk: HAPROXY_BRANCHES must go from the newest to the oldest (%s before %s)", branches[i-1], b)
		}
		v, err := pinned(schematic.ComponentHAProxy, b)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(v.Version, b+".") {
			return nil, fmt.Errorf("versions.mk: %s = %s isn't on the %s branch", VersionVar(v.Component, b), v.Version, b)
		}
		v.EOL = vars["HAPROXY_"+strings.ReplaceAll(b, ".", "_")+"_EOL"]
		if _, err := time.Parse(time.DateOnly, v.EOL); err != nil {
			return nil, fmt.Errorf("variants.mk: HAProxy %s's end of support: %v", b, err)
		}
		v.Default = i == 0
		s.HAProxy = append(s.HAProxy, v)
	}

	def := vars["KERNEL_DEFAULT_TRACK"]
	for _, t := range strings.Fields(vars["KERNEL_TRACKS"]) {
		if !schematic.ValidKernelTrack(t) {
			return nil, fmt.Errorf("variants.mk: %q isn't a kernel track", t)
		}
		v, err := pinned(schematic.ComponentKernel, t)
		if err != nil {
			return nil, err
		}
		v.Default = t == def
		s.Kernel = append(s.Kernel, v)
	}
	if s.Default(schematic.ComponentKernel).Name == "" {
		return nil, fmt.Errorf("variants.mk: KERNEL_DEFAULT_TRACK %q isn't in KERNEL_TRACKS", def)
	}

	for _, r := range strings.Fields(vars["VARIANTS_RETIRED"]) {
		parts := strings.Split(r, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("variants.mk: VARIANTS_RETIRED: %q isn't component:name:last-release", r)
		}
		ret := schematic.Retired{Component: parts[0], Name: parts[1], LastRelease: parts[2]}
		if _, ok := s.Lookup(ret.Component, ret.Name); ok {
			return nil, fmt.Errorf("variants.mk: %s %s is both offered and retired", ret.Component, ret.Name)
		}
		s.Retired = append(s.Retired, ret)
	}
	// The catalog's own checks (names, one default each, retired entries).
	if err := s.CatalogVariants().Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// compareBranches orders "major.minor" branches.
func compareBranches(a, b string) int {
	pa, pb := strings.SplitN(a, ".", 2), strings.SplitN(b, ".", 2)
	for i := range 2 {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x - y
		}
	}
	return 0
}

func (s *Set) list(component string) []Variant {
	if component == schematic.ComponentHAProxy {
		return s.HAProxy
	}
	return s.Kernel
}

// Lookup returns the variant of component named name.
func (s *Set) Lookup(component, name string) (Variant, bool) {
	for _, v := range s.list(component) {
		if v.Name == name {
			return v, true
		}
	}
	return Variant{}, false
}

// Default returns component's default variant.
func (s *Set) Default(component string) Variant {
	for _, v := range s.list(component) {
		if v.Default {
			return v
		}
	}
	return Variant{}
}

// All lists every variant, HAProxy branches first.
func (s *Set) All() []Variant {
	return append(append([]Variant{}, s.HAProxy...), s.Kernel...)
}

// CatalogVariants is the catalog's view of the set: every variant built
// for amd64, the defaults for every architecture too.
func (s *Set) CatalogVariants() *schematic.Catalog {
	conv := func(vs []Variant) []schematic.Variant {
		out := make([]schematic.Variant, 0, len(vs))
		for _, v := range vs {
			arches := []string{"amd64"}
			if v.Default {
				arches = append(arches, DefaultOnlyArches...)
			}
			out = append(out, schematic.Variant{Name: v.Name, Version: v.Version, Default: v.Default, EOL: v.EOL, Arches: arches})
		}
		return out
	}
	return &schematic.Catalog{HAProxy: conv(s.HAProxy), Kernel: conv(s.Kernel), Retired: s.Retired}
}

// Resolve returns the HAProxy branch and kernel track sc gets for arch.
func (s *Set) Resolve(sc *schematic.Schematic, arch string) (haproxy, kernel Variant, err error) {
	c := s.CatalogVariants()
	r, err := c.Resolve(&schematic.Schematic{Customization: schematic.Customization{
		HAProxy: sc.HAProxyBranch(), Kernel: sc.KernelTrack()}}, arch)
	if err != nil {
		return Variant{}, Variant{}, err
	}
	haproxy, _ = s.Lookup(schematic.ComponentHAProxy, r.HAProxy.Name)
	kernel, _ = s.Lookup(schematic.ComponentKernel, r.Kernel.Name)
	return haproxy, kernel, nil
}
