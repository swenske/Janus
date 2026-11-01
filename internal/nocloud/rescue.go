package nocloud

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// RescueLabel is a rescue volume's label (janusctl image rescue-volume,
// docs/guide/recovery.md): attached to a node, it opens a rescue window
// at the next boot - whatever the node trusts. Its own label, not
// cidata's: a node keeps its NoCloud volume attached (the Controller's
// machines do), and FindVolume takes the first cidata.
const RescueLabel = "JANUSRESCUE"

// RescueFileName is the file a rescue volume carries.
const RescueFileName = "rescue.json"

// RescueRunFile is where init leaves what this boot's rescue volume
// says, for janusd: /run is a tmpfs - the next boot without the volume
// has none.
const RescueRunFile = "/run/janus/rescue.json"

// Rescue is what a rescue volume says.
type Rescue struct {
	// Firewall "skip": the node's saved nftables ruleset isn't applied
	// this boot - it may shut the rescue window's port.
	Firewall string `json:"firewall,omitempty"`
}

// ErrNoRescue is no rescue volume among the candidates.
var ErrNoRescue = errors.New("no rescue volume found")

// ParseRescue reads a rescue.json: unknown fields refused - a volume
// made for a newer janusd than this one says so rather than being half
// understood.
func ParseRescue(raw []byte) (*Rescue, error) {
	var r Rescue
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%s: %w", RescueFileName, err)
	}
	switch r.Firewall {
	case "", "skip":
	default:
		return nil, fmt.Errorf("%s: firewall %q: \"skip\" or nothing", RescueFileName, r.Firewall)
	}
	return &r, nil
}

// ReadRescue reads the rescue volume at devicePath (FindLabel's).
func ReadRescue(devicePath string) (*Rescue, error) {
	d, err := diskfs.Open(devicePath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", devicePath, err)
	}
	defer d.Close()
	fs, err := d.GetFilesystem(0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem on %s: %w", devicePath, err)
	}
	raw, err := readFSFile(fs, RescueFileName)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", RescueFileName, err)
	}
	return ParseRescue(raw)
}

// FindRescue is FindLabel for RescueLabel: ErrNoRescue when none.
func FindRescue(candidates []string) (string, error) {
	path, err := FindLabel(candidates, RescueLabel)
	if errors.Is(err, ErrNotFound) {
		return "", ErrNoRescue
	}
	return path, err
}

// A volume holds a few files of a few KiB: an ISO is written whole at
// isoSize, a FAT32 filesystem needs some room of its own.
const (
	isoSize = 1 << 20
	fatSize = 8 << 20
)

// WriteVolume writes, at path, a volume labelled label holding files:
// ISO9660 (Rock Ridge: names as written), to attach as a CD-ROM, or FAT
// (fat), to attach as a disk or write onto a USB stick - an ISO attached
// as a disk isn't read. workDir is a writable directory to assemble an
// ISO in.
func WriteVolume(path, workDir, label string, fat bool, files map[string][]byte) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s exists already", path)
	}
	if fat {
		if len(label) > 11 {
			return fmt.Errorf("label %q: a FAT label is 11 characters at most", label)
		}
		return writeFAT(path, label, files)
	}
	return writeISO(path, workDir, label, files)
}

func writeFAT(path, label string, files map[string][]byte) error {
	d, err := diskfs.Create(path, fatSize, diskfs.SectorSize(512))
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer d.Close()
	fs, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeFat32, VolumeLabel: label})
	if err != nil {
		return fmt.Errorf("create the filesystem: %w", err)
	}
	return writeFiles(fs, files)
}

func writeISO(path, workDir, label string, files map[string][]byte) error {
	ws, err := os.MkdirTemp(workDir, "volume-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(ws)
	d, err := diskfs.Create(path, isoSize, diskfs.SectorSize(2048))
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer d.Close()
	fs, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeISO9660, VolumeLabel: label, WorkDir: ws})
	if err != nil {
		return fmt.Errorf("create the filesystem: %w", err)
	}
	if err := writeFiles(fs, files); err != nil {
		return err
	}
	iso, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return fmt.Errorf("unexpected filesystem type %T", fs)
	}
	// Rock Ridge keeps the names as written ("user-data", lowercase,
	// with its hyphen) - plain ISO9660 level 1 can't hold them. The
	// label is padded with spaces to its 32-byte field, as ISO9660 has
	// it: go-diskfs pads with NULs, which nodes released before
	// FindLabel trimmed them don't trim.
	if err := iso.Finalize(iso9660.FinalizeOptions{RockRidge: true, VolumeIdentifier: fmt.Sprintf("%-32s", label)}); err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	return nil
}

func writeFiles(fs filesystem.FileSystem, files map[string][]byte) error {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names) // the same image for the same files
	for _, name := range names {
		if strings.ContainsRune(name, '/') || name != filepath.Base(name) {
			return fmt.Errorf("%q: a file at the volume's root only", name)
		}
		f, err := fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		if _, err := f.Write(files[name]); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	return nil
}
