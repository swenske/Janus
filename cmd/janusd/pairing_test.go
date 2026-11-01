package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swenske/Janus/internal/pki"
	"github.com/swenske/Janus/internal/selfregister"
)

// TestPairingWanted: the window opens on a node that trusts nothing -
// no fleet, no Controller provisioned, not a node from before the code -,
// and on none other.
func TestPairingWanted(t *testing.T) {
	saved := selfregister.Dir
	t.Cleanup(func() { selfregister.Dir = saved })
	selfregister.Dir = t.TempDir()
	pkiDir := t.TempDir()
	fleet, err := pki.OpenFleet(pkiDir)
	if err != nil {
		t.Fatal(err)
	}
	if !pairingWanted(fleet, nil, pkiDir) {
		t.Error("a node that trusts nothing has no window")
	}
	if pairingWanted(fleet, errors.New("unreadable"), pkiDir) || pairingWanted(nil, nil, pkiDir) {
		t.Error("a fleet that couldn't be read: a window")
	}

	// A first boot's admin credential without the mark: a node from
	// before the code, no window; with it, a window.
	if err := os.WriteFile(filepath.Join(pkiDir, "admin.crt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if pairingWanted(fleet, nil, pkiDir) {
		t.Error("a node from before the code has a window")
	}
	markPairing(pkiDir)
	if !pairingWanted(fleet, nil, pkiDir) {
		t.Error("a node whose PKI is marked has no window")
	}

	// A Controller provisioned: it registers, no window.
	_ = os.WriteFile(filepath.Join(selfregister.Dir, "address"), []byte("ctl.example:8443"), 0o600)
	_ = os.WriteFile(filepath.Join(selfregister.Dir, "ca.crt"), []byte("x"), 0o600)
	if pairingWanted(fleet, nil, pkiDir) {
		t.Error("a node provisioned with a Controller has a window")
	}
	os.Remove(filepath.Join(selfregister.Dir, "address"))

	// A fleet: no window.
	root, _ := pki.NewCAFor("root", time.Hour)
	issuing, _ := root.IssueCA("issuing", time.Hour)
	signed, _ := pki.SignBundle(root, pki.Bundle{Version: 1, Issued: time.Now().UTC(), IssuingCAs: []string{string(issuing.CertPEM)}})
	if err := fleet.Set(root.CertPEM, signed); err != nil {
		t.Fatal(err)
	}
	if pairingWanted(fleet, nil, pkiDir) {
		t.Error("a node that trusts a fleet has a window")
	}
}
