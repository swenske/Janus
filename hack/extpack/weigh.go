package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/swenske/Janus/internal/diskimage"
	"github.com/swenske/Janus/internal/schematic"
)

// weighArch is the architecture weighed: the one releases publish inputs
// for while the Raspberry Pi images are suspended - and the bigger one.
const weighArch = "amd64"

// squashFlags squash a part the way rootfs/layer-and-squash.sh squashes
// the image (TestSquashFlagsAreTheImages keeps them the same).
var squashFlags = []string{"-comp", "zstd", "-Xcompression-level", "19", "-b", "1M", "-all-root"}

// weigh writes into a release's catalog what each part of an image takes
// of a slot - the base tree, each HAProxy branch's layer, each extension
// - and the slots' size: the image builder adds them up for a schematic,
// before anyone builds it. Each part is squashed alone, as the image is;
// added up, they come within a few KiB of the image itself (each
// squashfs has its own superblock and tables).
func weigh(args []string) {
	fl := flag.NewFlagSet("weigh", flag.ExitOnError)
	catalogFile := fl.String("catalog", "", "the release's schematic-catalog.json, rewritten")
	inputs := fl.String("inputs", "", "the release's inputs: rootfs-base-"+weighArch+".tar, haproxy-<branch>-"+weighArch+".tar, extension-<name>-"+weighArch+".tar")
	_ = fl.Parse(args)
	if *catalogFile == "" || *inputs == "" {
		fl.Usage()
		os.Exit(2)
	}
	data, err := os.ReadFile(*catalogFile)
	if err != nil {
		log.Fatal(err)
	}
	c, err := schematic.ParseCatalog(data)
	if err != nil {
		log.Fatal(err)
	}
	if err := weighCatalog(c, *inputs); err != nil {
		log.Fatal(err)
	}
	out, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(*catalogFile, append(out, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("weighed %s: base %s, %d HAProxy branches, %d extensions, slots %s\n",
		*catalogFile, mib(c.BaseBytes), len(c.HAProxy), len(c.Extensions), mib(c.SlotBytes))
}

// weighCatalog fills c's weights from the inputs in dir.
func weighCatalog(c *schematic.Catalog, dir string) error {
	c.SlotBytes = diskimage.DataMB << 20
	var err error
	if c.BaseBytes, err = squashedSize(filepath.Join(dir, "rootfs-base-"+weighArch+".tar")); err != nil {
		return err
	}
	for i, v := range c.HAProxy {
		if !slices.Contains(v.Arches, weighArch) {
			continue
		}
		if c.HAProxy[i].Bytes, err = squashedSize(filepath.Join(dir, "haproxy-"+v.Name+"-"+weighArch+".tar")); err != nil {
			return err
		}
	}
	for i, e := range c.Extensions {
		if !slices.Contains(e.Arches, weighArch) {
			continue
		}
		if c.Extensions[i].Bytes, err = squashedSize(filepath.Join(dir, "extension-"+e.Name+"-"+weighArch+".tar")); err != nil {
			return err
		}
	}
	return nil
}

// squashedSize is the size of a tar's tree squashed like the image.
func squashedSize(tarPath string) (uint64, error) {
	tool, err := lookTool("mksquashfs")
	if err != nil {
		return 0, err
	}
	dir, err := os.MkdirTemp("", "extpack-weigh-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	tree := filepath.Join(dir, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		return 0, err
	}
	if out, err := exec.Command("tar", "-xf", tarPath, "-C", tree, "--no-same-owner").CombinedOutput(); err != nil {
		return 0, fmt.Errorf("unpack %s: %v: %s", tarPath, err, out)
	}
	sqfs := filepath.Join(dir, "part.squashfs")
	args := append([]string{tree, sqfs, "-noappend", "-quiet", "-no-progress"}, squashFlags...)
	if out, err := exec.Command(tool, args...).CombinedOutput(); err != nil {
		return 0, fmt.Errorf("squash %s: %v: %s", tarPath, err, out)
	}
	fi, err := os.Stat(sqfs)
	if err != nil {
		return 0, err
	}
	return uint64(fi.Size()), nil
}

// lookTool finds a tool where Debian puts it, outside a user's PATH too.
func lookTool(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, d := range []string{"/usr/sbin", "/sbin"} {
		if p := filepath.Join(d, name); fileExists(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found (squashfs-tools)", name)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func mib(n uint64) string {
	return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
}
