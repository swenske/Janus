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

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/netconfig"
	"github.com/swenske/Janus/internal/nocloud"
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

// Build returns the ISO image's bytes (nocloud.WriteVolume). workDir is
// a writable directory the image is assembled in (the Controller's data
// directory: its container image has no /tmp).
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
	if err := nocloud.WriteVolume(path, tmp, "cidata", false, map[string][]byte{"user-data": userData, "meta-data": []byte("{}\n")}); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
