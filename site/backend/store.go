package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/swenske/Janus/internal/schematic"
)

// store is the site's data on disk:
//
//	<dir>/schematics/<id>.json                    a schematic, canonical form
//	<dir>/images/<id>/<version>/<arch>/<file>     built images
//	<dir>/images/<id>/<version>/<arch>/manifest.json   written last by the
//	                                              build: the set is complete
//
// The schematic build workflow uploads straight into images/ (rsync, as a
// restricted user); the manifest is its completion marker.
type store struct {
	dir string
}

var (
	versionPattern = regexp.MustCompile(`^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}(-[0-9]+)?$`)
	arches         = []string{"amd64", "arm64"}
)

func validVersion(v string) bool { return versionPattern.MatchString(v) }
func validArch(a string) bool    { return slices.Contains(arches, a) }

// platform is a download choice in the builder: where the image runs,
// and the file it downloads.
type platform struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Group        string `json:"group"`
	Arch         string `json:"arch"`
	File         string `json:"file"`
	Description  string `json:"description"`
	Docs         string `json:"docs,omitempty"`
	Experimental bool   `json:"experimental,omitempty"`
}

// platforms are the images a release publishes, with the same file
// names for every schematic.
var platforms = []platform{
	{ID: "proxmox", Name: "Proxmox VE", Group: "Virtualization", Arch: "amd64", File: "janus.qcow2",
		Description: "qcow2 disk to import with qm importdisk; UEFI (OVMF), serial console.", Docs: "image/kvm-proxmox/README.md"},
	{ID: "kvm", Name: "KVM / libvirt", Group: "Virtualization", Arch: "amd64", File: "janus-kvm.qcow2",
		Description: "qcow2 disk for virt-install --boot uefi, OpenStack or any KVM host.", Docs: "image/kvm/README.md"},
	{ID: "vmware", Name: "VMware ESXi", Group: "Virtualization", Arch: "amd64", File: "janus.vmdk",
		Description: "streamOptimized VMDK; EFI firmware.", Docs: "image/vmware/README.md"},
	{ID: "iso", Name: "Bare metal (ISO / USB)", Group: "Bare metal", Arch: "amd64", File: "janus.iso",
		Description: "Installer: write it to a USB stick (not a CD), boot it, install the machine's disk with janusctl lifecycle install - registered with your Controller if you give it one.", Docs: "docs/provisioning-a-node.md"},
	{ID: "rpi4", Name: "Raspberry Pi 4 / CM4", Group: "Single-board computer", Arch: "arm64", File: "pi4-disk.img",
		Description: "SD card image with UEFI firmware (pftf/RPi4).", Docs: "docs/raspberry-pi-testing.md"},
	{ID: "rpi5", Name: "Raspberry Pi 5", Group: "Single-board computer", Arch: "arm64", File: "pi5-disk.img", Experimental: true,
		Description: "SD card image with experimental UEFI firmware - no SD or Ethernet driver in mainline Linux yet.", Docs: "docs/raspberry-pi-testing.md"},
}

// bundleFiles are an update bundle's files (LifecycleService.Upgrade).
var bundleFiles = []string{"rootfs.squashfs", "rootfs.squashfs.sha256", "rootfs.verity", "uki-a.efi", "uki-b.efi"}

// servedFile reports whether name is a file a build publishes for arch.
func servedFile(arch, name string) bool {
	if name == "manifest.json" {
		return true
	}
	if arch == "amd64" && slices.Contains(bundleFiles, name) {
		return true
	}
	return slices.ContainsFunc(platforms, func(p platform) bool { return p.Arch == arch && p.File == name })
}

// manifest is a completed build.
type manifest struct {
	Schematic string         `json:"schematic"`
	Version   string         `json:"version"`
	Arch      string         `json:"arch"`
	BuiltAt   time.Time      `json:"built_at"`
	RunURL    string         `json:"run_url,omitempty"`
	Files     []manifestFile `json:"files"`
}

type manifestFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (m *manifest) file(name string) (manifestFile, bool) {
	i := slices.IndexFunc(m.Files, func(f manifestFile) bool { return f.Name == name })
	if i < 0 {
		return manifestFile{}, false
	}
	return m.Files[i], true
}

func newStore(dir string) (*store, error) {
	for _, d := range []string{"schematics", "images"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &store{dir: dir}, nil
}

// PutSchematic stores s and returns its ID.
func (s *store) PutSchematic(sc *schematic.Schematic) (string, error) {
	id := sc.ID()
	path := filepath.Join(s.dir, "schematics", id+".json")
	if _, err := os.Stat(path); err == nil {
		return id, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(sc.Canonical(), '\n'), 0o644); err != nil {
		return "", err
	}
	return id, os.Rename(tmp, path)
}

// Schematic loads a stored schematic; the default one always exists.
func (s *store) Schematic(id string) (*schematic.Schematic, error) {
	if id == schematic.DefaultID() {
		return schematic.Default(), nil
	}
	if !schematic.ValidID(id) {
		return nil, errNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "schematics", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	sc, err := schematic.Parse(data)
	if err != nil {
		return nil, err
	}
	if sc.ID() != id {
		return nil, fmt.Errorf("stored schematic %s doesn't match its ID", id)
	}
	return sc, nil
}

// SchematicIDs lists the stored schematics.
func (s *store) SchematicIDs() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "schematics"))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if id, ok := cutSuffix(e.Name(), ".json"); ok && schematic.ValidID(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func cutSuffix(s, suffix string) (string, bool) {
	if len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)], true
	}
	return "", false
}

// ImageDir is where a build's files live.
func (s *store) ImageDir(id, version, arch string) string {
	return filepath.Join(s.dir, "images", id, version, arch)
}

// Manifest returns a completed build's manifest, or nil if there's none.
func (s *store) Manifest(id, version, arch string) (*manifest, error) {
	data, err := os.ReadFile(filepath.Join(s.ImageDir(id, version, arch), "manifest.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Prune keeps the keep newest versions of each schematic's builds.
func (s *store) Prune(keep int, newer func(a, b string) int) error {
	ids, err := os.ReadDir(filepath.Join(s.dir, "images"))
	if err != nil {
		return err
	}
	for _, id := range ids {
		base := filepath.Join(s.dir, "images", id.Name())
		versions, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		var names []string
		for _, v := range versions {
			if validVersion(v.Name()) {
				names = append(names, v.Name())
			}
		}
		slices.SortFunc(names, func(a, b string) int { return newer(b, a) })
		for _, old := range names[min(keep, len(names)):] {
			if err := os.RemoveAll(filepath.Join(base, old)); err != nil {
				return err
			}
		}
	}
	return nil
}
