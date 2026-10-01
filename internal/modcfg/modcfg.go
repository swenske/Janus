// Package modcfg keeps an optional module daemon's configuration -
// keepalived's, BIRD's: saved on STATE (Dir), copied where the daemon
// reads it (a directory of its own under /run, which the daemon's
// SELinux domain can read - never STATE, which holds the node's keys),
// and checked by the daemon itself before anything is replaced.
package modcfg

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Dir holds the saved configurations: STATE's config/. A var so tests
// can move it.
var Dir = "/etc/janus/config"

// MaxBytes bounds a configuration.
const MaxBytes = 1 << 20

// Module is one daemon's configuration.
type Module struct {
	// Name is the daemon's, for messages and file names ("keepalived").
	Name string
	// Binary checks configurations (and is the daemon).
	Binary string
	// File is the configuration's name under Dir and RunDir.
	File string
	// RunDir is where the daemon reads its configuration.
	RunDir string
	// CheckArgs are the arguments that make Binary check the file at
	// path; a nonzero exit means it's refused, its output says why.
	CheckArgs func(path string) []string
}

// Available reports whether the daemon is in the image.
func (m *Module) Available() bool {
	_, err := os.Stat(m.Binary)
	return err == nil
}

// RunPath is where the daemon reads its configuration.
func (m *Module) RunPath() string { return filepath.Join(m.RunDir, m.File) }

// Saved returns the saved configuration; isDefault means there's none.
func (m *Module) Saved() (config string, isDefault bool, err error) {
	data, err := os.ReadFile(filepath.Join(Dir, m.File))
	if errors.Is(err, os.ErrNotExist) {
		return "", true, nil
	}
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", true, nil
	}
	return string(data), false, nil
}

// Check has the daemon check config; it returns the daemon's complaints,
// naming the file after the module's own (not the temporary copy).
func (m *Module) Check(config string) ([]string, error) {
	if len(config) > MaxBytes {
		return []string{fmt.Sprintf("the configuration is over %d bytes", MaxBytes)}, errors.New("invalid configuration")
	}
	if err := os.MkdirAll(m.RunDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(m.RunDir, ".check-*-"+m.File)
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(config); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return nil, err
	}
	cmd := exec.Command(m.Binary, m.CheckArgs(f.Name())...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, fmt.Errorf("run %s: %w", m.Name, err)
		}
		var lines []string
		for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
			l = strings.ReplaceAll(l, f.Name(), m.File)
			if strings.TrimSpace(l) != "" {
				lines = append(lines, l)
			}
		}
		if len(lines) == 0 {
			lines = []string{fmt.Sprintf("%s refused the configuration (%v)", m.Name, err)}
		}
		return lines, errors.New("invalid configuration")
	}
	return nil, nil
}

// Save makes config the saved one and the one the daemon reads; an
// empty config removes both.
func (m *Module) Save(config string) error {
	saved := filepath.Join(Dir, m.File)
	if strings.TrimSpace(config) == "" {
		if err := os.Remove(saved); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(m.RunPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := writeAtomic(Dir, m.File, []byte(config), true); err != nil {
		return err
	}
	return writeAtomic(m.RunDir, m.File, []byte(config), false)
}

// Boot copies the saved configuration where the daemon reads it, if
// there's one.
func (m *Module) Boot() error {
	if !m.Available() {
		return nil
	}
	config, isDefault, err := m.Saved()
	if err != nil || isDefault {
		return err
	}
	return writeAtomic(m.RunDir, m.File, []byte(config), false)
}

func writeAtomic(dir, name string, data []byte, sync bool) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if sync {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}
