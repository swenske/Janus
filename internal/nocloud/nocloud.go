// Package nocloud reads node self-registration config
// (controller_address/controller_ca_cert) from a locally-attached
// volume, the same "NoCloud" discovery convention cloud-init itself
// uses: a filesystem (ISO9660 or vfat) labeled "cidata"/"CIDATA",
// containing a user-data file. Talos Linux uses this exact same
// mechanism for its own machine config (confirmed directly against
// Sidero's own docs, see docs/provisioning-a-node.md and CLAUDE.md's
// own research notes) - and, like Talos, this package does NOT
// implement real cloud-init semantics (#cloud-config, write_files,
// runcmd, ...) at all: user-data here is Janus's own minimal JSON
// schema (controller_address/controller_ca_cert), not a real
// cloud-init document. Reusing the discovery convention (label +
// filenames) is what matters - it's what every Terraform provider that
// already generates a cloud-init volume (libvirt, Proxmox, OpenStack,
// ...) already knows how to produce, with arbitrary content.
//
// This is the *external*, delivered-separately-at-boot complement to
// internal/diskseed's *embedded*, baked-into-the-image-at-generation-
// time approach - see internal/diskseed's own package doc for that
// side, and CLAUDE.md for why both exist (mirroring Talos's own two
// supported config-delivery models: NoCloud/PXE vs Image Factory's
// "Embedded machine configuration").
//
// meta-data's own seedfrom field (a real cloud-init/NoCloud feature -
// fetch user-data from a URL instead of reading it locally) is
// supported too, with one Janus-specific extension beyond what real
// cloud-init offers: an optional seedfrom_ca_cert to pin a specific CA
// for that fetch - see fetchUserData's own doc comment for the full
// trust model (three modes, chosen entirely by what's present, no
// explicit flag needed).
package nocloud

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem"
)

// ErrNotFound is returned by FindVolume when no candidate device
// carries the "cidata"/"CIDATA" label - the overwhelming majority of
// boots (no NoCloud volume attached at all), not a real error.
var ErrNotFound = errors.New("no cidata volume found")

// Config is what a NoCloud volume's user-data resolves to - the exact
// two fields internal/api/install.go's InstallRequest and
// internal/diskseed.SeedController already write directly onto STATE;
// rootfs/init writes these same two files itself once this package
// finds them here instead.
type Config struct {
	ControllerAddress string
	ControllerCACert  []byte
}

type userData struct {
	ControllerAddress string `json:"controller_address"`
	ControllerCACert  string `json:"controller_ca_cert"`
}

type metaData struct {
	SeedFrom       string `json:"seedfrom"`
	SeedFromCACert string `json:"seedfrom_ca_cert"`
}

// vdWholeDisk matches a whole virtio-blk disk device name (vda, vdb,
// ...), never a partition (vda1) - a NoCloud volume is always used as
// the entire block device's own content, no partition table, the same
// convention a real mkisofs/genisoimage- or mtools-built cloud-init
// volume already has.
var vdWholeDisk = regexp.MustCompile(`^vd[a-z]$`)

// ScanBlockDevices lists /dev/vd[a-z] whole-disk devices, excluding
// excludeDisk (this node's own boot disk - see internal/bootslot.Disk,
// which callers already have on hand from resolving STATE) - the real
// candidate list rootfs/init passes to FindVolume. virtio-blk only,
// matching every other assumption already baked into this project
// (image/disk/assemble.sh, internal/bootslot, ...) - no SCSI/SATA
// support, there being no real use case for it here yet.
func ScanBlockDevices(excludeDisk string) ([]string, error) {
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return nil, fmt.Errorf("read /dev: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !vdWholeDisk.MatchString(e.Name()) {
			continue
		}
		path := "/dev/" + e.Name()
		if path == excludeDisk {
			continue
		}
		out = append(out, path)
	}
	return out, nil
}

// FindVolume returns the first path in candidates whose filesystem
// carries the "cidata"/"CIDATA" label (case-insensitive, and trimmed -
// FAT/ISO9660 labels are commonly padded with trailing spaces).
// Deliberately takes an explicit candidate list rather than scanning
// /dev itself, so it's unit-testable against plain files (see
// ScanBlockDevices for the real /dev/vd* enumeration). A candidate that
// fails to open, or has no recognizable filesystem at all (e.g. a data
// disk with an existing Janus GPT layout - internal/api/install.go's
// own kind of disk), is silently skipped, not an error: the whole point
// is scanning devices with no prior knowledge of what's actually
// attached.
func FindVolume(candidates []string) (string, error) {
	for _, path := range candidates {
		d, err := diskfs.Open(path, diskfs.WithOpenMode(diskfs.ReadOnly))
		if err != nil {
			continue
		}
		fs, err := d.GetFilesystem(0)
		if err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(fs.Label()), "cidata") {
			return path, nil
		}
	}
	return "", ErrNotFound
}

// Read opens devicePath's filesystem (already confirmed by FindVolume
// to carry the cidata label) and resolves its Config - either straight
// from a local user-data file, or fetched from meta-data's own seedfrom
// URL if one is given.
func Read(devicePath string) (*Config, error) {
	d, err := diskfs.Open(devicePath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", devicePath, err)
	}
	fs, err := d.GetFilesystem(0)
	if err != nil {
		return nil, fmt.Errorf("open filesystem on %s: %w", devicePath, err)
	}

	var raw []byte
	md, mdErr := readMetaData(fs)
	if mdErr == nil && md.SeedFrom != "" {
		raw, err = fetchUserData(md.SeedFrom, md.SeedFromCACert)
		if err != nil {
			return nil, fmt.Errorf("fetch seedfrom %s: %w", md.SeedFrom, err)
		}
	} else {
		raw, err = readFSFile(fs, "user-data")
		if err != nil {
			return nil, fmt.Errorf("read user-data: %w", err)
		}
	}

	var ud userData
	if err := json.Unmarshal(raw, &ud); err != nil {
		return nil, fmt.Errorf("parse user-data: %w", err)
	}
	if ud.ControllerAddress == "" || ud.ControllerCACert == "" {
		return nil, fmt.Errorf("user-data is missing controller_address/controller_ca_cert")
	}
	return &Config{ControllerAddress: ud.ControllerAddress, ControllerCACert: []byte(ud.ControllerCACert)}, nil
}

func readMetaData(fs filesystem.FileSystem) (*metaData, error) {
	raw, err := readFSFile(fs, "meta-data")
	if err != nil {
		return nil, err
	}
	var md metaData
	if err := json.Unmarshal(raw, &md); err != nil {
		return nil, fmt.Errorf("parse meta-data: %w", err)
	}
	return &md, nil
}

// readFSFile trims trailing NUL bytes off whatever io.ReadAll returns -
// a real go-diskfs FAT quirk found writing this package's own tests,
// not by inspection: reading a file spanning more than one cluster back
// returns the whole last cluster's raw bytes, zero-padding past the
// file's real size, rather than stopping exactly at it (a small enough
// file, fitting in one cluster, happened to read back clean, which is
// what let this slip past the package's own first, smaller test case).
// Safe here specifically because valid JSON never legitimately contains
// an embedded NUL byte.
func readFSFile(fs filesystem.FileSystem, path string) ([]byte, error) {
	f, err := fs.OpenFile(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return bytes.TrimRight(data, "\x00"), nil
}

// fetchUserData retrieves user-data from a seedfrom URL - a real
// cloud-init/NoCloud feature (fetch remotely instead of reading
// locally), with one Janus-specific extension beyond what real
// cloud-init offers: pinning a specific CA for the fetch via
// caCertPEM, rather than only ever trusting the system store. Which of
// three modes applies is decided entirely by what's given, no explicit
// flag:
//
//   - caCertPEM set: seedfromURL must be https://, verified *only*
//     against this pinned CA - the system trust store is bypassed
//     entirely, no TOFU, the same "verify against a specific known CA,
//     never ambient trust" model every other CA-verified connection in
//     this project already uses (controller_ca_cert, etc.).
//   - caCertPEM empty, https://: verified against the system trust
//     store (Go's default TLS behavior, backed by the CA bundle
//     rootfs/assemble.sh installs at /etc/ssl/certs/ca-certificates.crt
//     specifically so this has something real to check against) -
//     ordinary HTTPS-client behavior, nothing special coded here.
//   - caCertPEM empty, http://: no verification at all, in the clear -
//     the PXE-style "trust the network" mode Talos's own
//     talos.config=http://... uses, accepted the same way here.
//
// http:// combined with a non-empty caCertPEM is refused outright: a
// plaintext connection has nothing for a pinned certificate to verify,
// and offering that combination would be a real footgun (an operator
// who thinks they've secured the fetch by supplying a CA, when they
// haven't) rather than a genuine third mode.
func fetchUserData(seedfromURL, caCertPEM string) ([]byte, error) {
	u, err := url.Parse(seedfromURL)
	if err != nil {
		return nil, fmt.Errorf("parse seedfrom URL: %w", err)
	}

	client := http.DefaultClient
	switch {
	case caCertPEM != "" && u.Scheme != "https":
		return nil, fmt.Errorf("seedfrom_ca_cert requires an https:// seedfrom URL, got %q", u.Scheme)
	case caCertPEM != "":
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caCertPEM)) {
			return nil, fmt.Errorf("seedfrom_ca_cert is not a valid PEM certificate")
		}
		client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("seedfrom URL must be http:// or https://, got %q", u.Scheme)
	}

	resp, err := client.Get(seedfromURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("seedfrom %s: HTTP %d", seedfromURL, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
