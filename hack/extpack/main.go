// Command extpack packs optional extensions for the image build (see
// docs/image-factory.md):
//
//	extpack pack -name NAME -arch ARCH -version VERSION -tree DIR -out FILE.tar
//	extpack catalog -release VERSION -out FILE.json NAME=VERSION...
//
// pack turns an extension's exported file tree (extensions/<name>/
// Dockerfile's export stage) into the tar rootfs/assemble.sh layers onto
// the base rootfs: the tree, the extension's manifest at
// usr/lib/janus/extensions/<name>.json (with its version filled in), and
// a .janus-labels file listing the SELinux types to set on its files.
//
// catalog writes schematic-catalog.json: the extensions a release can
// build a schematic with.
//
//	extpack id [-schematic FILE]
//	extpack layers -schematic FILE -arch ARCH -dir DIR
//
// id prints a schematic's ID (the default schematic's without -schematic);
// layers prints the extension tars (in DIR) a schematic needs for ARCH,
// checking each exists and is built for ARCH.
//
//	extpack check -schematic FILE -catalog FILE -arch ARCH [-id ID]
//
// check verifies a schematic against a release's catalog - and, with
// -id, that it is the schematic with that ID.
//
//	extpack variant [-schematic FILE] [-arch ARCH] haproxy|kernel
//
// variant prints the HAProxy branch or kernel track a schematic gets
// (variants.mk, in the current directory): the Makefile builds with it.
//
//	extpack image-info [-schematic FILE] [-catalog FILE] -arch ARCH [-version V] -out FILE
//
// image-info writes the image.json of an image of the schematic
// (schematic.ImageInfoPath): the variants it gets from the release's
// catalog, or from variants.mk without -catalog.
package main

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/schematic"
	"github.com/swenske/Janus/internal/variants"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: extpack pack|catalog ...")
	}
	switch os.Args[1] {
	case "pack":
		pack(os.Args[2:])
	case "catalog":
		catalog(os.Args[2:])
	case "id":
		schematicID(os.Args[2:])
	case "layers":
		layers(os.Args[2:])
	case "check":
		check(os.Args[2:])
	case "variant":
		variant(os.Args[2:])
	case "image-info":
		imageInfo(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func readManifest(name string) *extensions.Manifest {
	data, err := os.ReadFile(filepath.Join("extensions", name, "manifest.json"))
	if err != nil {
		log.Fatal(err)
	}
	var m extensions.Manifest
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		log.Fatalf("extensions/%s/manifest.json: %v", name, err)
	}
	if m.Name != name {
		log.Fatalf("extensions/%s/manifest.json names itself %q", name, m.Name)
	}
	return &m
}

func pack(args []string) {
	fl := flag.NewFlagSet("pack", flag.ExitOnError)
	name := fl.String("name", "", "extension name")
	arch := fl.String("arch", "", "architecture")
	version := fl.String("version", "", "extension version")
	tree := fl.String("tree", "", "exported file tree")
	out := fl.String("out", "", "output tar")
	_ = fl.Parse(args)
	if *name == "" || *arch == "" || *version == "" || *tree == "" || *out == "" {
		fl.Usage()
		os.Exit(2)
	}
	m := readManifest(*name)
	m.Version = *version
	if err := m.Validate(); err != nil {
		log.Fatal(err)
	}
	if !slices.Contains(m.Arches, *arch) {
		log.Fatalf("%s isn't built for %s (arches: %v)", *name, *arch, m.Arches)
	}
	for p := range m.Labels {
		if _, err := os.Stat(filepath.Join(*tree, p)); err != nil {
			log.Fatalf("%s labels %s, which the tree doesn't have: %v", *name, p, err)
		}
	}
	for _, s := range m.Services {
		if _, err := os.Stat(filepath.Join(*tree, s.Path)); err != nil {
			log.Fatalf("service %s runs %s, which the tree doesn't have: %v", s.ID, s.Path, err)
		}
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := addTree(tw, *tree); err != nil {
		log.Fatal(err)
	}
	manifest, _ := json.MarshalIndent(m, "", "  ")
	if err := addFile(tw, "usr/lib/janus/extensions/"+m.Name+".json", append(manifest, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	var labels strings.Builder
	paths := make([]string, 0, len(m.Labels))
	for p := range m.Labels {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		fmt.Fprintf(&labels, "%s %s\n", p, m.Labels[p])
	}
	if err := addFile(tw, ".janus-labels", []byte(labels.String()), 0o644); err != nil {
		log.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("packed %s %s (%s) into %s\n", m.Name, m.Version, *arch, *out)
}

var epoch = time.Unix(0, 0)

// addTree adds dir's content, sorted, owned by root, with a fixed mtime,
// so the same tree always gives the same tar.
func addTree(tw *tar.Writer, dir string) error {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.Sort(paths)
	for _, p := range paths {
		rel, _ := filepath.Rel(dir, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if rel == ".janus-labels" || strings.HasPrefix(rel, "usr/lib/janus/extensions/") {
			return fmt.Errorf("%s is reserved for extpack", rel)
		}
		switch {
		case info.IsDir():
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: rel + "/", Mode: int64(info.Mode().Perm()), ModTime: epoch}); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := addFile(tw, rel, data, info.Mode().Perm()); err != nil {
				return err
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: rel, Linkname: target, Mode: 0o777, ModTime: epoch}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: unsupported file type %s", rel, info.Mode().Type())
		}
	}
	return nil
}

func addFile(tw *tar.Writer, name string, data []byte, mode fs.FileMode) error {
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(mode), Size: int64(len(data)), ModTime: epoch}); err != nil {
		return err
	}
	_, err := io.Copy(tw, strings.NewReader(string(data)))
	return err
}

func catalog(args []string) {
	fl := flag.NewFlagSet("catalog", flag.ExitOnError)
	release := fl.String("release", "", "Janus release")
	out := fl.String("out", "", "output file")
	_ = fl.Parse(args)
	if *release == "" || *out == "" {
		fl.Usage()
		os.Exit(2)
	}
	c := schematic.Catalog{Version: *release, Extensions: []schematic.CatalogEntry{}}
	for _, arg := range fl.Args() {
		name, version, ok := strings.Cut(arg, "=")
		if !ok || version == "" {
			log.Fatalf("%q: want NAME=VERSION", arg)
		}
		m := readManifest(name)
		m.Version = version
		if err := m.Validate(); err != nil {
			log.Fatal(err)
		}
		c.Extensions = append(c.Extensions, schematic.CatalogEntry{Name: m.Name, Version: m.Version, Description: m.Description, Arches: m.Arches, Homepage: m.Homepage, Replaces: m.Replaces})
	}
	if len(c.Extensions) == 0 {
		log.Fatal(errors.New("no extension given"))
	}
	slices.SortFunc(c.Extensions, func(a, b schematic.CatalogEntry) int { return strings.Compare(a.Name, b.Name) })
	set, err := variants.Load(".")
	if err != nil {
		log.Fatal(err)
	}
	vc := set.CatalogVariants()
	c.HAProxy, c.Kernel, c.Retired = vc.HAProxy, vc.Kernel, vc.Retired
	data, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s (%d extensions, %d HAProxy branches, %d kernel tracks)\n", *out, len(c.Extensions), len(c.HAProxy), len(c.Kernel))
}

func loadSchematic(file string) *schematic.Schematic {
	if file == "" {
		return schematic.Default()
	}
	data, err := os.ReadFile(file)
	if err != nil {
		log.Fatal(err)
	}
	sc, err := schematic.Parse(data)
	if err != nil {
		log.Fatalf("%s: %v", file, err)
	}
	return sc
}

func schematicID(args []string) {
	fl := flag.NewFlagSet("id", flag.ExitOnError)
	file := fl.String("schematic", "", "schematic JSON (default: the default schematic)")
	_ = fl.Parse(args)
	fmt.Println(loadSchematic(*file).ID())
}

func layers(args []string) {
	fl := flag.NewFlagSet("layers", flag.ExitOnError)
	file := fl.String("schematic", "", "schematic JSON")
	arch := fl.String("arch", "", "architecture")
	dir := fl.String("dir", "", "directory holding extension-<name>-<arch>.tar")
	_ = fl.Parse(args)
	if *arch == "" || *dir == "" {
		fl.Usage()
		os.Exit(2)
	}
	var out []string
	for _, name := range loadSchematic(*file).Extensions() {
		m := readManifest(name)
		if !slices.Contains(m.Arches, *arch) {
			log.Fatalf("extension %s isn't available for %s (only %v)", name, *arch, m.Arches)
		}
		tarPath := filepath.Join(*dir, "extension-"+name+"-"+*arch+".tar")
		if _, err := os.Stat(tarPath); err != nil {
			log.Fatalf("extension %s: %v - build it first", name, err)
		}
		out = append(out, tarPath)
	}
	fmt.Println(strings.Join(out, " "))
}

func check(args []string) {
	fl := flag.NewFlagSet("check", flag.ExitOnError)
	file := fl.String("schematic", "", "schematic JSON")
	catalogFile := fl.String("catalog", "", "schematic-catalog.json")
	arch := fl.String("arch", "", "architecture")
	id := fl.String("id", "", "expected schematic ID")
	_ = fl.Parse(args)
	if *catalogFile == "" || *arch == "" {
		fl.Usage()
		os.Exit(2)
	}
	sc := loadSchematic(*file)
	if *id != "" && sc.ID() != *id {
		log.Fatalf("the schematic's ID is %s, not %s", sc.ID(), *id)
	}
	data, err := os.ReadFile(*catalogFile)
	if err != nil {
		log.Fatal(err)
	}
	c, err := schematic.ParseCatalog(data)
	if err != nil {
		log.Fatal(err)
	}
	if err := c.Check(sc, *arch); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("schematic %s: ok for %s %s\n", sc.ID(), c.Version, *arch)
}

func variant(args []string) {
	fl := flag.NewFlagSet("variant", flag.ExitOnError)
	file := fl.String("schematic", "", "schematic JSON (default: the default schematic)")
	arch := fl.String("arch", "amd64", "architecture")
	_ = fl.Parse(args)
	if fl.NArg() != 1 || (fl.Arg(0) != schematic.ComponentHAProxy && fl.Arg(0) != schematic.ComponentKernel) {
		log.Fatal("usage: extpack variant [-schematic FILE] [-arch ARCH] haproxy|kernel")
	}
	set, err := variants.Load(".")
	if err != nil {
		log.Fatal(err)
	}
	haproxy, kernel, err := set.Resolve(loadSchematic(*file), *arch)
	if err != nil {
		log.Fatal(err)
	}
	if fl.Arg(0) == schematic.ComponentHAProxy {
		fmt.Println(haproxy.Name)
	} else {
		fmt.Println(kernel.Name)
	}
}

func imageInfo(args []string) {
	fl := flag.NewFlagSet("image-info", flag.ExitOnError)
	file := fl.String("schematic", "", "schematic JSON (default: the default schematic)")
	catalogFile := fl.String("catalog", "", "the release's schematic-catalog.json (default: variants.mk)")
	arch := fl.String("arch", "", "architecture")
	version := fl.String("version", "", "Janus release")
	out := fl.String("out", "", "output file")
	_ = fl.Parse(args)
	if *arch == "" || *out == "" {
		fl.Usage()
		os.Exit(2)
	}
	sc := loadSchematic(*file)
	var c *schematic.Catalog
	if *catalogFile != "" {
		data, err := os.ReadFile(*catalogFile)
		if err != nil {
			log.Fatal(err)
		}
		if c, err = schematic.ParseCatalog(data); err != nil {
			log.Fatal(err)
		}
	} else {
		set, err := variants.Load(".")
		if err != nil {
			log.Fatal(err)
		}
		c = set.CatalogVariants()
	}
	// Only the variants: the extensions are checked where they're layered.
	r, err := c.Resolve(&schematic.Schematic{Customization: schematic.Customization{
		HAProxy: sc.HAProxyBranch(), Kernel: sc.KernelTrack()}}, *arch)
	if err != nil {
		log.Fatal(err)
	}
	data, _ := json.MarshalIndent(schematic.NewImageInfo(sc, r, *version, *arch), "", "  ")
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
}
