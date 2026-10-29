// Package statexfer carries a node's state to another node
// (SystemService.StateExport/StateImport, docs/guide/replacing-a-node.md):
// what a replacement needs to serve as the node it replaces - HAProxy's
// configuration, its files and certificates, the modules' configurations,
// the network configuration, the Controller registration, the fleet
// trust and, when asked, the node's identity (its CA and keys).
//
// The state is carried logically, as files under their STATE paths,
// never as a disk image: the two nodes' disks may be laid out apart (a
// LOGS partition on one only), and what isn't the node's configuration -
// its event log, the logs, a boot confirmation pending - stays behind.
//
// An import is staged on STATE (StagingDir) and put in place by janusd at
// its next start (ApplyStaged), before anything reads STATE: nothing a
// running janusd keeps in memory can write over it, and a power cut while
// it is put in place leaves the rest staged for the next start.
package statexfer

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Format names the stream's layout, in its manifest.
const Format = "janus-state-1"

// ManifestName is the stream's first entry.
const ManifestName = "MANIFEST.json"

// StagingDir holds an import, under the STATE root, until janusd puts it
// in place: files/ (the files under their paths), MANIFEST.json, and the
// ready marker written last.
const StagingDir = "import"

const (
	stagedFiles = "files"
	readyName   = "ready"

	// maxFile and maxTotal bound an import: STATE is 128 MiB on an image,
	// and a node's configuration is a few megabytes.
	maxFile  = 16 << 20
	maxTotal = 96 << 20
)

// NetworkConfig is the network configuration's path: imported only when
// asked, held back otherwise.
const NetworkConfig = "network/config.json"

// Manifest describes an export.
type Manifest struct {
	Format    string    `json:"format"`
	Hostname  string    `json:"hostname"`
	Version   string    `json:"version"`
	Schematic string    `json:"schematic"`
	Created   time.Time `json:"created"`
	// Identity: the node's CA and keys are in it.
	Identity bool   `json:"identity"`
	Files    []File `json:"files"`
}

// File is one exported file.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

// What is carried, by STATE path: whole trees (ending in "/") or files.
// Identity's only with the node's identity.
var (
	carried = []string{
		"haproxy/haproxy.cfg",
		"haproxy/history/",
		"haproxy/files/",
		"haproxy/runtime-certs/",
		"haproxy/acme/",
		"haproxy/cluster/tls-ticket-keys",
		"config/",
		NetworkConfig,
		"controller/address",
		"controller/ca.crt",
		"controller/fleet-root.crt",
		"pki/fleet/",
	}
	identity = []string{
		"pki/ca.crt", "pki/ca.key",
		"pki/server.crt", "pki/server.key",
		"pki/admin.crt", "pki/admin.key",
		"pki/ca-cross.crt",
		"controller/registered",
		"controller/enrollment.json",
		"config/consul-node-id",
		// The Consul agent's ACL token is this node's own (its node
		// identity): a replacement gets its own from the Controller.
		"config/consul-managed.json", // consul.ManagedFile
	}
	// left describes the hardware or this run, not the configuration.
	left = []string{
		"config/sysctl-observations.json", // the old node's hardware, by hour
		"config/logforward/cursors.json",  // positions in the old node's LOGS
	}
)

// Class says whether rel - a clean STATE path - is carried, and whether
// as part of the node's identity.
func Class(rel string) (isCarried, isIdentity bool) {
	if slices.Contains(left, rel) {
		return false, false
	}
	if slices.Contains(identity, rel) {
		return true, true
	}
	for _, c := range carried {
		if rel == c || (strings.HasSuffix(c, "/") && strings.HasPrefix(rel, c)) {
			return true, false
		}
	}
	return false, false
}

// cleanRel is rel if it's a clean relative path under the root, "" if
// not.
func cleanRel(rel string) string {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
		return ""
	}
	c := path.Clean(rel)
	if c != rel || c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return ""
	}
	return c
}

// hidden: a temporary file (".name.tmp") some writer left behind.
func hidden(rel string) bool { return strings.HasPrefix(path.Base(rel), ".") }

// ExportOptions: what to say of the node, and whether its identity goes.
type ExportOptions struct {
	Identity                     bool
	Hostname, Version, Schematic string
}

// ErrRotating: a CA rotation is half done (pki/rotation/): its identity
// can't be exported until janusd finishes it.
var ErrRotating = errors.New("a CA rotation is under way: restart janusd to finish it, then export")

// Export writes root's state to w as a tar: the manifest, then the files.
func Export(root string, opts ExportOptions, w io.Writer) (*Manifest, error) {
	if opts.Identity {
		if _, err := os.Stat(filepath.Join(root, "pki", "rotation")); err == nil {
			return nil, ErrRotating
		}
	}
	m := &Manifest{Format: Format, Hostname: opts.Hostname, Version: opts.Version, Schematic: opts.Schematic, Created: time.Now().UTC(), Identity: opts.Identity}
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// Neither what's staged nor the filesystem's own.
			if rel == StagingDir || rel == "lost+found" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || hidden(rel) {
			return nil
		}
		ok, isID := Class(rel)
		if !ok || (isID && !opts.Identity) {
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	// The manifest first, so an import knows what's coming: read each file
	// once for its hash, again to send it - a file changed in between is
	// caught by the import's check.
	for _, rel := range paths {
		f, err := describe(root, rel)
		if err != nil {
			return nil, err
		}
		m.Files = append(m.Files, f)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o600, Size: int64(len(data)), ModTime: m.Created, Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(data); err != nil {
		return nil, err
	}
	for _, f := range m.Files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.Path, Mode: int64(f.Mode), Size: int64(len(data)), ModTime: m.Created, Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	return m, tw.Close()
}

func describe(root, rel string) (File, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	st, err := os.Stat(p)
	if err != nil {
		return File{}, err
	}
	if st.Size() > maxFile {
		return File{}, fmt.Errorf("%s: %d bytes, more than a state file holds (%d)", rel, st.Size(), maxFile)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return File{}, err
	}
	sum := sha256.Sum256(data)
	return File{Path: rel, Size: int64(len(data)), Mode: uint32(st.Mode().Perm()), SHA256: hex.EncodeToString(sum[:])}, nil
}

// InUse reports why root's node is in use - an HAProxy configuration
// applied, a certificate or a file uploaded -, "" when it isn't: only a
// node in use refuses an import, which would mix two nodes' states.
func InUse(root string) string {
	for _, c := range []struct{ dir, what string }{
		{"haproxy/history", "an HAProxy configuration was applied"},
		{"haproxy/runtime-certs", "a certificate was uploaded"},
		{"haproxy/files", "a file was uploaded"},
	} {
		entries, err := os.ReadDir(filepath.Join(root, c.dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !hidden(e.Name()) {
				return c.what
			}
		}
	}
	return ""
}

// ImportOptions: whether the identity and the network configuration go
// in place.
type ImportOptions struct {
	Identity     bool
	ApplyNetwork bool
}

// Staged is what Stage put aside.
type Staged struct {
	Manifest *Manifest
	Files    []string
	// Skipped: the export's files left out, each with why.
	Skipped []string
	// HeldNetwork: the network configuration, not applied.
	HeldNetwork []byte
}

// Stage reads an export from r and stages it under root for ApplyStaged:
// every file checked against the manifest, nothing outside what an
// export carries. A stage already there is replaced.
func Stage(root string, opts ImportOptions, r io.Reader) (*Staged, error) {
	dir := filepath.Join(root, StagingDir)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	st, err := stage(dir, opts, r)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return st, nil
}

func stage(dir string, opts ImportOptions, r io.Reader) (*Staged, error) {
	tr := tar.NewReader(r)
	h, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("not a node's state: %w", err)
	}
	if h.Name != ManifestName {
		return nil, fmt.Errorf("not a node's state: %s first, not %s", h.Name, ManifestName)
	}
	data, err := io.ReadAll(io.LimitReader(tr, 4<<20))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("the manifest: %w", err)
	}
	if m.Format != Format {
		return nil, fmt.Errorf("the manifest's format is %q, this node reads %q", m.Format, Format)
	}
	if opts.Identity && !m.Identity {
		return nil, errors.New("the export doesn't carry its node's identity: export it with the identity, or import without")
	}
	want := map[string]File{}
	var total int64
	for _, f := range m.Files {
		rel := cleanRel(f.Path)
		if rel == "" {
			return nil, fmt.Errorf("the manifest names %q: not a path under STATE", f.Path)
		}
		if ok, _ := Class(rel); !ok {
			return nil, fmt.Errorf("the manifest names %s: not something a node's state carries", rel)
		}
		if f.Size < 0 || f.Size > maxFile {
			return nil, fmt.Errorf("%s: %d bytes", rel, f.Size)
		}
		total += f.Size
		want[rel] = f
	}
	if total > maxTotal {
		return nil, fmt.Errorf("%d bytes in all, more than a node's state holds (%d)", total, maxTotal)
	}
	if err := os.MkdirAll(filepath.Join(dir, stagedFiles), 0o700); err != nil {
		return nil, err
	}
	st := &Staged{Manifest: &m}
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the stream: %w", err)
		}
		rel := cleanRel(h.Name)
		f, listed := want[rel]
		switch {
		case h.Typeflag != tar.TypeReg:
			return nil, fmt.Errorf("%s: not a regular file", h.Name)
		case !listed:
			return nil, fmt.Errorf("%s isn't in the manifest", h.Name)
		case seen[rel]:
			return nil, fmt.Errorf("%s twice", rel)
		case h.Size != f.Size:
			return nil, fmt.Errorf("%s: %d bytes, the manifest says %d", rel, h.Size, f.Size)
		}
		seen[rel] = true
		_, isID := Class(rel)
		switch {
		case isID && !opts.Identity:
			st.Skipped = append(st.Skipped, rel+": the node's identity, not reused")
			if err := discard(tr, f); err != nil {
				return nil, err
			}
			continue
		case rel == NetworkConfig && !opts.ApplyNetwork:
			body, err := readChecked(tr, f)
			if err != nil {
				return nil, err
			}
			st.HeldNetwork = body
			st.Skipped = append(st.Skipped, rel+": held back, not applied")
			continue
		}
		if err := stageFile(filepath.Join(dir, stagedFiles), rel, f, tr); err != nil {
			return nil, err
		}
		st.Files = append(st.Files, rel)
	}
	for rel := range want {
		if !seen[rel] {
			return nil, fmt.Errorf("%s is in the manifest but not in the stream", rel)
		}
	}
	mdata, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeDurable(filepath.Join(dir, ManifestName), mdata, 0o600); err != nil {
		return nil, err
	}
	// Last: only a whole stage is ever put in place.
	if err := writeDurable(filepath.Join(dir, readyName), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return st, nil
}

// readChecked reads f's content from r, checked against its hash.
func readChecked(r io.Reader, f File) ([]byte, error) {
	h := sha256.New()
	data, err := io.ReadAll(io.TeeReader(io.LimitReader(r, f.Size), h))
	if err != nil {
		return nil, err
	}
	if err := checkSum(h, f); err != nil {
		return nil, err
	}
	return data, nil
}

func discard(r io.Reader, f File) error {
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(r, f.Size)); err != nil {
		return err
	}
	return checkSum(h, f)
}

func checkSum(h hash.Hash, f File) error {
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%s: its content isn't what the manifest says (sha256 %s, not %s)", f.Path, got, f.SHA256)
	}
	return nil
}

// stageFile writes f under files, durably, checked against its hash.
func stageFile(files, rel string, f File, r io.Reader) error {
	p := filepath.Join(files, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode(f.Mode))
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), io.LimitReader(r, f.Size))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return checkSum(h, f)
}

// fileMode keeps a carried file's permission bits, never more than
// read-write.
func fileMode(m uint32) os.FileMode {
	mode := os.FileMode(m) & 0o666
	if mode == 0 {
		mode = 0o600
	}
	return mode
}

// Applied is what ApplyStaged put in place.
type Applied struct {
	Manifest *Manifest
	Files    []string
}

// ApplyStaged puts a staged import in place under root: each file
// renamed over its STATE path, then the stage removed. Nothing staged:
// nil. A stage without its ready marker - an import that didn't finish -
// is removed, nothing applied. A power cut midway leaves what wasn't
// moved yet staged: the next start moves it.
func ApplyStaged(root string) (*Applied, error) {
	dir := filepath.Join(root, StagingDir)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(dir, readyName)); err != nil {
		return nil, errors.Join(errors.New("an import that didn't finish was left: removed"), os.RemoveAll(dir))
	}
	a := &Applied{}
	if data, err := os.ReadFile(filepath.Join(dir, ManifestName)); err == nil {
		var m Manifest
		if json.Unmarshal(data, &m) == nil {
			a.Manifest = &m
		}
	}
	files := filepath.Join(dir, stagedFiles)
	err := filepath.WalkDir(files, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(files, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if ok, _ := Class(rel); !ok || cleanRel(rel) == "" {
			return fmt.Errorf("%s: not something a node's state carries", rel)
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.Rename(p, target); err != nil {
			return err
		}
		a.Files = append(a.Files, rel)
		return syncDir(filepath.Dir(target))
	})
	if err != nil {
		return a, err
	}
	return a, os.RemoveAll(dir)
}

func writeDurable(p string, data []byte, perm os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(p), "."+filepath.Base(p)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
