package haproxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

// HAProxy's own files: what a configuration references besides its
// certificates - error pages, maps, ACL lists, Lua, and certificates not
// obtained by the letsencrypt extension. They live in FilesDir (on a
// node, /etc/haproxy/files: STATE's haproxy/files/), one level of
// subdirectories allowed (certs/x.pem, for a crt directory).
//
// A file is only replaced or removed if haproxy.cfg still loads with the
// change: the next reload, or the next boot, can't break on it. A file
// holding a private key is never read back.

const (
	maxHAProxyFileBytes = 1 << 20
	maxHAProxyFiles     = 256
)

var fileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}(/[A-Za-z0-9][A-Za-z0-9._-]{0,99})?$`)

// ErrSecretFile is FileRead on a file holding a private key.
var ErrSecretFile = errors.New("the file holds a private key: it is never read back")

// ErrNoFile is a file that isn't there.
var ErrNoFile = errors.New("no such file")

// FileInfo describes one of HAProxy's files.
type FileInfo struct {
	Name     string // relative to FilesDir
	Path     string // what haproxy.cfg references
	Size     int64
	SHA256   string
	Modified time.Time
	Secret   bool // holds a private key
}

func isSecret(data []byte) bool { return bytes.Contains(data, []byte("PRIVATE KEY-----")) }

func (m *Manager) filePath(name string) (string, error) {
	if m.FilesDir == "" {
		return "", errors.New("HAProxy's files have no directory on this node")
	}
	if !fileNamePattern.MatchString(name) {
		return "", fmt.Errorf("%w: file name %q: letters, digits, '.', '-', '_', and at most one subdirectory", ErrInvalidArgument, name)
	}
	return filepath.Join(m.FilesDir, filepath.FromSlash(name)), nil
}

// Files lists HAProxy's files.
func (m *Manager) Files() ([]FileInfo, error) {
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	return m.files()
}

func (m *Manager) files() ([]FileInfo, error) {
	var out []FileInfo
	if m.FilesDir == "" {
		return nil, nil
	}
	err := filepath.WalkDir(m.FilesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		rel, _ := filepath.Rel(m.FilesDir, path)
		if d.IsDir() {
			if rel != "." && filepath.Dir(rel) != "." {
				return fs.SkipDir // deeper than one level: not ours
			}
			return nil
		}
		name := filepath.ToSlash(rel)
		if !d.Type().IsRegular() || !fileNamePattern.MatchString(name) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out = append(out, FileInfo{Name: name, Path: path, Size: fi.Size(), SHA256: hex.EncodeToString(sum[:]),
			Modified: fi.ModTime(), Secret: isSecret(data)})
		return nil
	})
	return out, err
}

// FileRead returns a file's content - never a private key's.
func (m *Manager) FileRead(name string) ([]byte, error) {
	path, err := m.filePath(name)
	if err != nil {
		return nil, err
	}
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoFile, name)
	}
	if err != nil {
		return nil, err
	}
	if isSecret(data) {
		return nil, ErrSecretFile
	}
	return data, nil
}

// FilePut writes a file - if haproxy.cfg still loads with it. It returns
// HAProxy's complaints when it doesn't, and leaves the previous file.
func (m *Manager) FilePut(name string, data []byte) ([]string, error) {
	path, err := m.filePath(name)
	if err != nil {
		return nil, err
	}
	if len(data) > maxHAProxyFileBytes {
		return nil, fmt.Errorf("%w: %s is over %d bytes", ErrInvalidArgument, name, maxHAProxyFileBytes)
	}
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		existing, err := m.files()
		if err != nil {
			return nil, err
		}
		if len(existing) >= maxHAProxyFiles {
			return nil, fmt.Errorf("%w: already %d files", ErrInvalidArgument, maxHAProxyFiles)
		}
	}
	return m.swapChecked(path, data)
}

// FileDelete removes a file - if haproxy.cfg still loads without it.
func (m *Manager) FileDelete(name string) ([]string, error) {
	path, err := m.filePath(name)
	if err != nil {
		return nil, err
	}
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoFile, name)
	}
	errs, err := m.swapChecked(path, nil)
	if err == nil {
		// An emptied subdirectory goes too.
		if dir := filepath.Dir(path); dir != m.FilesDir {
			_ = os.Remove(dir)
		}
	}
	return errs, err
}

// swapChecked puts data at path (nil: removes it), checks haproxy.cfg
// with the change, and puts the previous state back if it doesn't load.
// Called with fileMu held.
func (m *Manager) swapChecked(path string, data []byte) ([]string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(m.FilesDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Temporary and backup copies are outside FilesDir: a crt directory
	// there would load whatever it holds.
	scratch := filepath.Dir(m.FilesDir)
	backup := ""
	if _, err := os.Stat(path); err == nil {
		b, err := os.CreateTemp(scratch, ".haproxy-file-*.bak")
		if err != nil {
			return nil, err
		}
		backup = b.Name()
		b.Close()
		if err := os.Rename(path, backup); err != nil {
			os.Remove(backup)
			return nil, err
		}
	}
	restore := func() {
		os.Remove(path)
		if backup != "" {
			_ = os.Rename(backup, path)
		}
	}
	if data != nil {
		tmp, err := os.CreateTemp(scratch, ".haproxy-file-*.tmp")
		if err != nil {
			restore()
			return nil, err
		}
		_, werr := tmp.Write(data)
		if werr == nil {
			werr = tmp.Chmod(0o600)
		}
		if werr == nil {
			werr = tmp.Sync()
		}
		if cerr := tmp.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Rename(tmp.Name(), path)
		}
		if werr != nil {
			os.Remove(tmp.Name())
			restore()
			return nil, werr
		}
	}
	if cfg, err := os.ReadFile(m.ConfigPath); err == nil {
		if ok, errs := m.Validate(cfg); !ok {
			restore()
			return errs, errors.New("haproxy.cfg doesn't load with this change")
		}
	}
	if backup != "" {
		os.Remove(backup)
	}
	for _, d := range slices.Compact([]string{dir, m.FilesDir}) {
		if f, err := os.Open(d); err == nil {
			_ = f.Sync()
			f.Close()
		}
	}
	return nil, nil
}
