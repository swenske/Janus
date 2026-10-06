package schematic

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ImageInfoPath is where an image says what it is built with: written
// into the rootfs at build time (hack/extpack image-info), so it is under
// dm-verity like everything else there, and the node reports it.
const ImageInfoPath = "/usr/lib/janus/image.json"

// ImageInfo is image.json.
type ImageInfo struct {
	// Schematic is the image's schematic in canonical form, SchematicID
	// its ID - the one in the image's signed kernel command line.
	Schematic   json.RawMessage `json:"schematic"`
	SchematicID string          `json:"schematic_id"`
	// Version is the Janus release the image belongs to ("" for a local
	// build without one).
	Version string `json:"version,omitempty"`
	Arch    string `json:"arch"`
	// HAProxy and Kernel are the variants the image is built with.
	HAProxy ImageComponent `json:"haproxy"`
	Kernel  ImageComponent `json:"kernel"`
}

// ImageComponent is a HAProxy branch or kernel track an image is built
// with.
type ImageComponent struct {
	Variant string `json:"variant"` // "3.2", "stable"
	Version string `json:"version"` // "3.2.25", "7.2.9"
	// Pinned: the schematic names this variant - else the image got the
	// release's default, and follows it from release to release.
	Pinned bool `json:"pinned,omitempty"`
	// Default: the variant is its release's default.
	Default bool `json:"default,omitempty"`
}

// NewImageInfo describes an image of sc built with r's variants.
func NewImageInfo(sc *Schematic, r *Resolved, version, arch string) *ImageInfo {
	return &ImageInfo{
		Schematic: sc.Canonical(), SchematicID: sc.ID(), Version: version, Arch: arch,
		HAProxy: ImageComponent{Variant: r.HAProxy.Name, Version: r.HAProxy.Version, Pinned: sc.HAProxyBranch() != "", Default: r.HAProxy.Default},
		Kernel:  ImageComponent{Variant: r.Kernel.Name, Version: r.Kernel.Version, Pinned: sc.KernelTrack() != "", Default: r.Kernel.Default},
	}
}

// ParseImageInfo reads image.json, checking its schematic is the one its
// ID names; Schematic comes back in canonical form (the file may be
// indented).
func ParseImageInfo(data []byte) (*ImageInfo, *Schematic, error) {
	var info ImageInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, nil, fmt.Errorf("image info: %w", err)
	}
	if len(info.Schematic) == 0 {
		return nil, nil, errors.New("image info: no schematic")
	}
	sc, err := Parse(info.Schematic)
	if err != nil {
		return nil, nil, fmt.Errorf("image info: %w", err)
	}
	if sc.ID() != info.SchematicID {
		return nil, nil, fmt.Errorf("image info: schematic %s isn't the one it names (%s)", sc.ID(), info.SchematicID)
	}
	info.Schematic = sc.Canonical()
	return &info, sc, nil
}
