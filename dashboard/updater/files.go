package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

// setEnvVar returns a .env file's content with name set to value: every
// line assigning it (commented ones aside) replaced, or the assignment
// appended. Everything else - comments, other variables, blank lines - is
// kept as is.
func setEnvVar(content []byte, name, value string) []byte {
	re := regexp.MustCompile(`^\s*(?:export\s+)?` + regexp.QuoteMeta(name) + `\s*=`)
	lines := bytes.SplitAfter(content, []byte("\n"))
	set := false
	var out bytes.Buffer
	for _, line := range lines {
		if re.Match(line) {
			out.WriteString(name + "=" + value + "\n")
			set = true
			continue
		}
		out.Write(line)
	}
	if !set {
		if out.Len() > 0 && !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			out.WriteByte('\n')
		}
		out.WriteString(name + "=" + value + "\n")
	}
	return out.Bytes()
}

// writeFileAtomic replaces path with data: a synced temporary file
// renamed over it, keeping the mode and owner of the file it replaces.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	uid, gid := -1, -1
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			uid, gid = int(st.Uid), int(st.Gid)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if uid >= 0 {
		_ = os.Lchown(tmp.Name(), uid, gid)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// copyTree copies the directory src to dst, which mustn't exist yet:
// files, directories and symlinks, with their modes and owners.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, to); err != nil {
				return err
			}
		case fi.IsDir():
			if err := os.Mkdir(to, fi.Mode().Perm()); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			if err := copyFile(path, to, fi.Mode().Perm()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: not a regular file, directory or symlink", path)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			_ = os.Lchown(to, int(st.Uid), int(st.Gid))
		}
		if fi.IsDir() {
			// Mkdir applied the umask.
			return os.Chmod(to, fi.Mode().Perm())
		}
		return nil
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// replaceContents makes dir hold exactly what from holds - dir itself
// stays (it's a volume's mount point), its entries are replaced.
func replaceContents(dir, from string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	entries, err = os.ReadDir(from)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTree(filepath.Join(from, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

// readOptional reads a file that may not exist (exists false then).
func readOptional(path string) (data []byte, exists bool, err error) {
	data, err = os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}
