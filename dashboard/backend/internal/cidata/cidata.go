// Package cidata builds the NoCloud volume a machine the Controller
// creates boots with: an ISO9660 filesystem labeled "cidata" holding
// Janus's own JSON user-data (internal/nocloud reads it on the node's
// first boot) and an empty JSON meta-data. Built in Go with go-diskfs
// because the Controller's image has no xorriso/genisoimage.
package cidata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/netconfig"
)

// UserData is the node's NoCloud user-data (internal/nocloud's userData,
// which is the reading side of the same contract).
type UserData struct {
	ControllerAddress string
	ControllerCACert  string
	RegistrationToken string
	// ControllerFleetRoot: the Controller's fleet root, once it has one -
	// the node checks the Controller through it.
	ControllerFleetRoot string
	Network             *janusv1alpha1.NetworkConfig
}

// MarshalJSON writes the network the way netconfig stores it, so the
// node parses exactly what netconfig.Validate accepted here.
func (u UserData) MarshalJSON() ([]byte, error) {
	doc := struct {
		ControllerAddress   string          `json:"controller_address"`
		ControllerCACert    string          `json:"controller_ca_cert"`
		RegistrationToken   string          `json:"registration_token,omitempty"`
		ControllerFleetRoot string          `json:"controller_fleet_root_cert,omitempty"`
		Network             json.RawMessage `json:"network,omitempty"`
	}{u.ControllerAddress, u.ControllerCACert, u.RegistrationToken, u.ControllerFleetRoot, nil}
	if u.Network != nil {
		if err := netconfig.Validate(u.Network); err != nil {
			return nil, fmt.Errorf("network: %w", err)
		}
		raw, err := netconfig.Marshal(u.Network)
		if err != nil {
			return nil, err
		}
		doc.Network = raw
	}
	return json.Marshal(doc)
}

// size is far more than a user-data ever needs (a CA certificate and a
// network configuration are a few KiB) - the image is written whole.
const size = 1 << 20

// Build returns the ISO image's bytes. workDir is a writable directory
// the image is assembled in (the Controller's data directory: its
// container image has no /tmp).
func Build(workDir string, ud UserData) ([]byte, error) {
	userData, err := json.Marshal(ud)
	if err != nil {
		return nil, fmt.Errorf("user-data: %w", err)
	}
	tmp, err := os.MkdirTemp(workDir, "cidata-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	path := filepath.Join(tmp, "cidata.iso")
	d, err := diskfs.Create(path, size, diskfs.SectorSize(2048))
	if err != nil {
		return nil, fmt.Errorf("create image: %w", err)
	}
	defer d.Close()
	ws := filepath.Join(tmp, "workspace")
	if err := os.Mkdir(ws, 0o700); err != nil {
		return nil, err
	}
	fs, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeISO9660, VolumeLabel: "cidata", WorkDir: ws})
	if err != nil {
		return nil, fmt.Errorf("create filesystem: %w", err)
	}
	for name, content := range map[string][]byte{"user-data": userData, "meta-data": []byte("{}\n")} {
		f, err := fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return nil, fmt.Errorf("create %s: %w", name, err)
		}
		if _, err := f.Write(content); err != nil {
			return nil, fmt.Errorf("write %s: %w", name, err)
		}
	}
	iso, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return nil, fmt.Errorf("unexpected filesystem type %T", fs)
	}
	// Rock Ridge keeps the names as written ("user-data", lowercase,
	// with its hyphen) - plain ISO9660 level 1 can't hold them. The
	// label is padded with spaces to its 32-byte field, as ISO9660 has
	// it: go-diskfs pads with NULs, which internal/nocloud (on nodes
	// already released) doesn't trim, so it wouldn't find "cidata".
	if err := iso.Finalize(iso9660.FinalizeOptions{RockRidge: true, VolumeIdentifier: fmt.Sprintf("%-32s", "cidata")}); err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	return os.ReadFile(path)
}
