package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/internal/schematic"
)

// withImage points ImageInfo at an image.json and a command line written
// for the test.
func withImage(t *testing.T, info *schematic.ImageInfo, cmdline string) {
	t.Helper()
	dir := t.TempDir()
	oldInfo, oldCmdline := imageInfoPath, cmdlinePath
	imageInfoPath, cmdlinePath = filepath.Join(dir, "image.json"), filepath.Join(dir, "cmdline")
	if info != nil {
		data, _ := json.Marshal(info)
		if err := os.WriteFile(imageInfoPath, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(cmdlinePath, []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
	imageInfoOnce, imageInfo = sync.Once{}, nil
	t.Cleanup(func() {
		imageInfoPath, cmdlinePath = oldInfo, oldCmdline
		imageInfoOnce, imageInfo = sync.Once{}, nil
	})
}

func pinnedImage() (*schematic.Schematic, *schematic.ImageInfo) {
	sc, _ := schematic.Parse([]byte(`{"customization":{"haproxy":"3.2","kernel":"longterm"}}`))
	r := &schematic.Resolved{
		HAProxy: schematic.Variant{Name: "3.2", Version: "3.2.25"},
		Kernel:  schematic.Variant{Name: "longterm", Version: "6.18.55"},
	}
	return sc, schematic.NewImageInfo(sc, r, "v1", "amd64")
}

func TestVersionReportsTheImage(t *testing.T) {
	sc, info := pinnedImage()
	withImage(t, info, "console=ttyS0 "+schematic.CmdlineArg(sc.ID()))
	resp, err := (&System{BuildVersion: "v1"}).Version(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSchematicId() != sc.ID() || resp.GetSchematic() != string(sc.Canonical()) {
		t.Errorf("schematic: %s %s", resp.GetSchematicId(), resp.GetSchematic())
	}
	if h := resp.GetHaproxy(); h.GetVariant() != "3.2" || h.GetVersion() != "3.2.25" || !h.GetPinned() || h.GetReleaseDefault() {
		t.Errorf("haproxy: %v", h)
	}
	if k := resp.GetKernel(); k.GetVariant() != "longterm" || !k.GetPinned() {
		t.Errorf("kernel: %v", k)
	}
}

func TestVersionIgnoresAnotherSchematicsImageInfo(t *testing.T) {
	_, info := pinnedImage()
	withImage(t, info, "console=ttyS0") // booted the default schematic
	resp, _ := (&System{}).Version(context.Background(), &emptypb.Empty{})
	if resp.GetHaproxy() != nil || resp.GetSchematic() != `{"customization":{}}` {
		t.Errorf("reported an image.json that isn't the booted schematic's: %v", resp)
	}
}

func TestHAProxyVersion(t *testing.T) {
	if v := HAProxyVersion("3.4.6-56332c5"); v != "3.4.6" || haproxyBranch(v) != "3.4" {
		t.Errorf("%q %q", v, haproxyBranch(v))
	}
	if b := haproxyBranch("dev"); b != "" || !strings.HasPrefix(HAProxyVersion("3.0.29"), "3.0") {
		t.Errorf("%q", b)
	}
}
