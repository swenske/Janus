package acme

import (
	"os"
	"path/filepath"
)

// writeDurable replaces dir/name with data so that it survives a power
// cut: a temporary file, synced, renamed, the directory synced.
func writeDurable(dir, name string, data []byte, perm os.FileMode) error {
	return writeDurableVia(dir, dir, name, data, perm)
}

// writeDurableVia is writeDurable with the temporary file in tmpDir
// (same filesystem as dir).
func writeDurableVia(tmpDir, dir, name string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(tmpDir, ".acme-"+name+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
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
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
