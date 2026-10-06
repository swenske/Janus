package sysctl

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// savedName is the file the confirmed values live in, under Dir: the
// syntax of sysctl.d(5), holding only what differs from Janus's defaults.
const savedName = "sysctl.d/90-haproxy-tuning.conf"

const savedHeader = `# Kernel parameters tuned on this node, written by janusd when a change
# is confirmed (docs/guide/kernel-tuning.md). Applied at every boot after
# Janus's baseline, line by line against the same whitelist and bounds as
# the API - any other line is refused - and before the CIS benchmark's
# values are written again.
`

// SavedLine is one assignment of the saved file.
type SavedLine struct {
	Line        int
	Name, Value string
}

// LoadSaved reads the saved file: its assignments, and the lines that
// aren't one. os.ErrNotExist means none was ever saved.
func LoadSaved() ([]SavedLine, []Refused, error) {
	data, err := os.ReadFile(filepath.Join(Dir, savedName))
	if err != nil {
		return nil, nil, err
	}
	var lines []SavedLine
	var malformed []Refused
	s := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; s.Scan(); n++ {
		line := strings.TrimSpace(s.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			malformed = append(malformed, Refused{Line: n, Name: name, Reason: "not a \"name = value\" line"})
			continue
		}
		lines = append(lines, SavedLine{Line: n, Name: name, Value: strings.TrimSpace(value)})
	}
	return lines, malformed, s.Err()
}

// Saved is the saved values by name - only the ones that apply (the
// boot refuses the others).
func Saved() map[string]string {
	lines, _, _ := LoadSaved()
	out := map[string]string{}
	for _, l := range lines {
		if p, _ := editable(l.Name); p != nil {
			if v, _, err := p.Parse(l.Value); err == nil {
				out[l.Name] = v
			}
		}
	}
	return out
}

// saveValues writes values - Editable parameters only, in Catalog's
// order - atomically: what survives a power cut is the old file or the
// new one. No value left removes the file.
func saveValues(values map[string]string) error {
	path := filepath.Join(Dir, savedName)
	if len(values) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return syncDir(filepath.Dir(path))
	}
	var b strings.Builder
	b.WriteString(savedHeader)
	for _, p := range EditableParams() {
		if v, ok := values[p.Name]; ok {
			fmt.Fprintf(&b, "%s = %s\n", p.Name, v)
		}
	}
	return writeAtomic(path, []byte(b.String()))
}

// writeAtomic writes path through a temporary file, fsynced, renamed
// over it, its directory fsynced.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
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
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer d.Close()
	return d.Sync()
}
