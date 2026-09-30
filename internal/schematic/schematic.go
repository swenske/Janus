// Package schematic is the image schematic: what goes into a Janus image
// beyond the base system - today, the optional extensions it carries.
// Modeled on Talos Image Factory's schematics: the schematic says what the
// image contains, while the version, platform, architecture and format
// are chosen separately, at download time.
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
// extension is named, and only extensions from the catalog of the
// version being built can be named.
type Schematic struct {
	Customization Customization `json:"customization"`
}

type Customization struct {
	// Extensions to include, by name ("node-exporter"). Order and
	// duplicates don't matter: Normalize sorts and deduplicates.
	Extensions []string `json:"extensions,omitempty"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

var idPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidName reports whether name can name an extension.
func ValidName(name string) bool { return namePattern.MatchString(name) }

// ValidID reports whether id has the form of a schematic ID.
func ValidID(id string) bool { return idPattern.MatchString(id) }

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

// Normalize validates the extension names, then sorts and deduplicates
// them.
func (s *Schematic) Normalize() error {
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

// YAML renders the schematic for display, in the shape Talos users know.
func (s *Schematic) YAML() string {
	exts := s.Extensions()
	if len(exts) == 0 {
		return "customization: {}\n"
	}
	var b strings.Builder
	b.WriteString("customization:\n  extensions:\n")
	for _, e := range exts {
		b.WriteString("    - " + e + "\n")
	}
	return b.String()
}

func (s *Schematic) clone() *Schematic {
	return &Schematic{Customization: Customization{Extensions: slices.Clone(s.Customization.Extensions)}}
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
