package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var controllerIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// loadOrCreateControllerID returns this Controller's stable ID, created
// once in the data directory: the ownership tag of every virtual machine
// it creates carries it, so two Controllers sharing a hypervisor (a lab
// one and a production one) never act on each other's machines.
func loadOrCreateControllerID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "controller-id")
	if raw, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(raw))
		if !controllerIDRe.MatchString(id) {
			return "", fmt.Errorf("%s holds %q, not an ID", path, id)
		}
		return id, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return id, nil
}
