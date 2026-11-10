package main

import (
	"os"
	"strings"
	"testing"
)

// Each platform's guide is a page of the docs site (site/docs/
// structure.yaml): the builder links its permalink.
func TestPlatformDocsArePages(t *testing.T) {
	structure, err := os.ReadFile("../docs/structure.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range platforms {
		if p.Docs != "" && !strings.Contains(string(structure), "file: "+p.Docs+",") {
			t.Errorf("%s's guide %s isn't a page of the docs site", p.ID, p.Docs)
		}
	}
}

// What the builder calls supported is what the docs say Janus supports:
// the two amd64 virtual machine images, never a target variants.mk can
// suspend.
func TestSupportedPlatforms(t *testing.T) {
	var supported []string
	for _, p := range platforms {
		if !p.Supported {
			continue
		}
		supported = append(supported, p.ID)
		if p.Arch != "amd64" || p.Target != "" || p.Experimental {
			t.Errorf("%s is supported but %s, target %q, experimental %v", p.ID, p.Arch, p.Target, p.Experimental)
		}
	}
	if strings.Join(supported, ",") != "proxmox,kvm" {
		t.Errorf("supported platforms = %v, the docs say Proxmox VE and KVM/libvirt", supported)
	}
}
