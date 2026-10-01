// Package extensions is the node side of optional extensions: software an
// image carries beyond the base system (node_exporter, qemu-guest-agent,
// later bird or keepalived), chosen per image through its schematic (see
// internal/schematic and docs/image-factory.md).
//
// An extension is baked into the read-only rootfs at build time, with a
// manifest in Dir describing it and the services janusd runs for it.
// There is no install or removal on a running node: changing extensions
// means another image, built from another schematic.
package extensions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/swenske/Janus/internal/schematic"
)

// Dir is where an image carries its extension manifests - under /usr,
// not /etc, which rootfs/init overmounts with a tmpfs at boot. A var so
// tests can point it elsewhere.
var Dir = "/usr/lib/janus/extensions"

// Manifest describes one extension. The same document serves the build
// (extensions/<name>/manifest.json, where Arches and Labels are read) and
// the node (Dir/<name>.json, where Services are run).
type Manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Homepage    string `json:"homepage,omitempty"`
	// Arches the extension is built for ("amd64", "arm64").
	Arches []string `json:"arches,omitempty"`
	// Replaces lists the names the extension had before, for the
	// catalog: a node built with one is offered this one instead.
	Replaces []string  `json:"replaces,omitempty"`
	Services []Service `json:"services,omitempty"`
	// Labels gives the SELinux type of files the extension installs, by
	// path in the rootfs ("usr/local/sbin/node_exporter":
	// "node_exporter_exec_t") - applied when the rootfs is built.
	Labels map[string]string `json:"selinux_labels,omitempty"`
}

// Service is a long-running process janusd supervises for an extension.
type Service struct {
	// ID names the service in the API (ServiceList, Logs...).
	ID          string   `json:"id"`
	Description string   `json:"description,omitempty"`
	Path        string   `json:"path"`
	Args        []string `json:"args,omitempty"`
	// WaitFor lists paths that must exist before the service starts -
	// a device the hypervisor may not provide, like the QEMU guest
	// agent's virtio port. Until they do, the service is "waiting",
	// not failing.
	WaitFor []string `json:"wait_for,omitempty"`
}

var (
	idPattern    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	typePattern  = regexp.MustCompile(`^[a-z0-9_]+_t$`)
	reservedIDs  = []string{"janusd", "haproxy"}
	errNoService = errors.New("no such service")
)

// Validate checks a manifest.
func (m *Manifest) Validate() error {
	if !schematic.ValidName(m.Name) {
		return fmt.Errorf("extension name %q is invalid", m.Name)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("extension %s: no version", m.Name)
	}
	for _, old := range m.Replaces {
		if !schematic.ValidName(old) || old == m.Name {
			return fmt.Errorf("extension %s: former name %q is invalid", m.Name, old)
		}
	}
	seen := map[string]bool{}
	for _, s := range m.Services {
		if !idPattern.MatchString(s.ID) || slices.Contains(reservedIDs, s.ID) || seen[s.ID] {
			return fmt.Errorf("extension %s: service id %q is invalid, reserved or repeated", m.Name, s.ID)
		}
		seen[s.ID] = true
		if !path.IsAbs(s.Path) || path.Clean(s.Path) != s.Path {
			return fmt.Errorf("extension %s: service %s: path %q must be absolute and clean", m.Name, s.ID, s.Path)
		}
		for _, w := range s.WaitFor {
			if !path.IsAbs(w) || path.Clean(w) != w {
				return fmt.Errorf("extension %s: service %s: wait_for path %q must be absolute and clean", m.Name, s.ID, w)
			}
		}
	}
	for p, t := range m.Labels {
		if path.IsAbs(p) || path.Clean(p) != p || strings.HasPrefix(p, "..") {
			return fmt.Errorf("extension %s: label path %q must be relative to the rootfs and clean", m.Name, p)
		}
		if !typePattern.MatchString(t) {
			return fmt.Errorf("extension %s: %q isn't an SELinux type", m.Name, t)
		}
	}
	return nil
}

// Load reads every manifest in dir, sorted by name. A missing dir is an
// image without extensions.
func Load(dir string) ([]Manifest, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Manifest
	ids := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var m Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if err := m.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		for _, s := range m.Services {
			if other, dup := ids[s.ID]; dup {
				return nil, fmt.Errorf("service %s is provided by both %s and %s", s.ID, other, m.Name)
			}
			ids[s.ID] = m.Name
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Manifest) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}
