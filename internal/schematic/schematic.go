// Package schematic is the image schematic: what goes into a Janus image
// beyond the base system - the optional extensions it carries, and the
// HAProxy branch and kernel track it is built with. Modeled on Talos Image
// Factory's schematics: the schematic says what the image contains, while
// the version, platform, architecture and format are chosen separately,
// at download time.
//
// A schematic is identified by the sha256 of its canonical JSON form, so
// the same choices always give the same ID. The ID is written into the
// signed kernel command line of every image built from it
// (janus.schematic=<id>): a node knows its own schematic from
// /proc/cmdline - authenticated by the UKI signature - and an upgrade can
// require the same schematic, so a node built with an extension keeps it.
package schematic

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// CmdlineParam is the kernel command line parameter carrying the ID.
const CmdlineParam = "janus.schematic"

// MaxExtensions bounds a schematic, as a sanity limit.
const MaxExtensions = 32

// Schematic is the document. It holds choices, never content: an
// extension, a HAProxy branch or a kernel track is named, and only what
// the catalog of the version being built offers can be named.
type Schematic struct {
	Customization Customization `json:"customization"`
}

// Customization's fields are in their canonical order: never reorder
// them, and never add one that isn't omitted when unset - every ID ever
// issued would change.
type Customization struct {
	// Extensions to include, by name ("prometheus-node-exporter"). Order and
	// duplicates don't matter: Normalize sorts and deduplicates.
	Extensions []string `json:"extensions,omitempty"`
	// HAProxy pins the image to a HAProxy LTS branch ("3.2"). Unset: the
	// release's default, its newest LTS branch - such an image moves to
	// the next LTS branch with the release that makes it the default.
	HAProxy string `json:"haproxy,omitempty"`
	// Kernel picks the kernel track ("stable", "longterm"). Unset: the
	// release's default track.
	Kernel string `json:"kernel,omitempty"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// A HAProxy branch is "major.minor", exactly as HAProxy names its
// branches - no leading zero, no patch level: a schematic names a branch,
// the release decides the version.
var branchPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})$`)

var trackPattern = regexp.MustCompile(`^[a-z]{1,32}$`)

// ValidName reports whether name can name an extension.
func ValidName(name string) bool { return namePattern.MatchString(name) }

// ValidID reports whether id has the form of a schematic ID.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// ValidHAProxyBranch reports whether b can name a HAProxy branch.
func ValidHAProxyBranch(b string) bool { return branchPattern.MatchString(b) }

// ValidKernelTrack reports whether t can name a kernel track.
func ValidKernelTrack(t string) bool { return trackPattern.MatchString(t) }

// Parse reads a schematic from its JSON form, strictly (unknown fields
// refused), and normalizes it.
func Parse(data []byte) (*Schematic, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Schematic
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("schematic: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("schematic: trailing data after the document")
	}
	if err := s.Normalize(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Normalize validates the schematic, then sorts and deduplicates the
// extensions. A HAProxy branch or kernel track equal to the default is
// kept as written: the default changes from release to release, an ID
// must not.
func (s *Schematic) Normalize() error {
	if b := s.Customization.HAProxy; b != "" && !ValidHAProxyBranch(b) {
		return fmt.Errorf("schematic: %q isn't a HAProxy branch (\"3.2\", say)", b)
	}
	if t := s.Customization.Kernel; t != "" && !ValidKernelTrack(t) {
		return fmt.Errorf("schematic: %q isn't a kernel track (\"stable\", \"longterm\")", t)
	}
	exts := make([]string, 0, len(s.Customization.Extensions))
	for _, name := range s.Customization.Extensions {
		if !ValidName(name) {
			return fmt.Errorf("schematic: %q isn't a valid extension name", name)
		}
		exts = append(exts, name)
	}
	slices.Sort(exts)
	exts = slices.Compact(exts)
	if len(exts) > MaxExtensions {
		return fmt.Errorf("schematic: at most %d extensions", MaxExtensions)
	}
	if len(exts) == 0 {
		exts = nil
	}
	s.Customization.Extensions = exts
	return nil
}

// Canonical is the normalized JSON form the ID is computed from.
func (s *Schematic) Canonical() []byte {
	n := s.clone()
	if err := n.Normalize(); err != nil {
		// Only reachable with invalid names set directly on the struct;
		// they still get a stable form.
		slices.Sort(n.Customization.Extensions)
	}
	data, _ := json.Marshal(n)
	return data
}

// ID is the schematic's identifier: the hex sha256 of Canonical.
func (s *Schematic) ID() string {
	sum := sha256.Sum256(s.Canonical())
	return hex.EncodeToString(sum[:])
}

// Extensions returns the normalized extension list.
func (s *Schematic) Extensions() []string {
	n := s.clone()
	_ = n.Normalize()
	return n.Customization.Extensions
}

// HAProxyBranch is the HAProxy branch the schematic pins, "" for the
// release's default.
func (s *Schematic) HAProxyBranch() string { return s.Customization.HAProxy }

// KernelTrack is the kernel track the schematic picks, "" for the
// release's default.
func (s *Schematic) KernelTrack() string { return s.Customization.Kernel }

// YAML renders the schematic for display, in the shape Talos users know.
func (s *Schematic) YAML() string {
	exts := s.Extensions()
	c := s.Customization
	if len(exts) == 0 && c.HAProxy == "" && c.Kernel == "" {
		return "customization: {}\n"
	}
	var b strings.Builder
	b.WriteString("customization:\n")
	if len(exts) > 0 {
		b.WriteString("  extensions:\n")
		for _, e := range exts {
			b.WriteString("    - " + e + "\n")
		}
	}
	if c.HAProxy != "" {
		// Quoted: YAML would read 3.2 as a number.
		b.WriteString("  haproxy: \"" + c.HAProxy + "\"\n")
	}
	if c.Kernel != "" {
		b.WriteString("  kernel: " + c.Kernel + "\n")
	}
	return b.String()
}

// Clone returns a copy of s that can be changed on its own.
func (s *Schematic) Clone() *Schematic { return s.clone() }

func (s *Schematic) clone() *Schematic {
	c := s.Customization
	c.Extensions = slices.Clone(c.Extensions)
	return &Schematic{Customization: c}
}

// Default is the schematic of the official releases: no extension.
func Default() *Schematic { return &Schematic{} }

// DefaultID is Default's ID.
func DefaultID() string { return Default().ID() }

// FromCmdline returns the schematic ID a kernel command line carries.
// A command line without the parameter - an image built before
// schematics existed - is the default schematic; ok is false only when
// the parameter is present but malformed.
func FromCmdline(cmdline string) (id string, ok bool) {
	for _, field := range strings.Fields(cmdline) {
		if v, found := strings.CutPrefix(field, CmdlineParam+"="); found {
			if !ValidID(v) {
				return "", false
			}
			return v, true
		}
	}
	return DefaultID(), true
}

// CmdlineArg is the command line argument for id.
func CmdlineArg(id string) string { return CmdlineParam + "=" + id }
