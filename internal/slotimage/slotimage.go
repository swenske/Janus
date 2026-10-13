// Package slotimage reads what an image is from its root filesystem,
// without mounting it: the /usr/lib/janus/image.json of the squashfs a
// release bundle carries, or of an A/B slot's data partition - which
// release, schematic, HAProxy and kernel it holds. Nothing here is
// checked against dm-verity: it says what a slot holds, the boot is
// what trusts it.
package slotimage

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/squashfs"

	"github.com/swenske/Janus/internal/schematic"
)

// ErrNoImageInfo is an image that doesn't say what it is: built before
// image.json existed (v2026.10.06-3).
var ErrNoImageInfo = errors.New("the image doesn't say what it is (no " + schematic.ImageInfoPath + ": built before v2026.10.06-3)")

// maxImageInfo bounds what's read of image.json.
const maxImageInfo = 64 << 10

// Read reads the image.json of the squashfs root filesystem in r, size
// bytes long.
func Read(r io.ReaderAt, size int64) (*schematic.ImageInfo, error) {
	fsys, err := squashfs.Read(file.New(&readerFile{r: r, size: size}, true), size, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("read the squashfs: %w", err)
	}
	// Paths as fs.FS has them: no leading slash.
	name := strings.TrimPrefix(schematic.ImageInfoPath, "/")
	f, err := fsys.OpenFile(name, os.O_RDONLY)
	if err != nil {
		// go-diskfs says a file isn't there without an error to match.
		if !listed(fsys, name) {
			return nil, ErrNoImageInfo
		}
		return nil, fmt.Errorf("open %s: %w", schematic.ImageInfoPath, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxImageInfo))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", schematic.ImageInfoPath, err)
	}
	info, _, err := schematic.ParseImageInfo(data)
	return info, err
}

// listed reports whether name's directory lists it.
func listed(fsys *squashfs.FileSystem, name string) bool {
	entries, err := fsys.ReadDir(path.Dir(name))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == path.Base(name) {
			return true
		}
	}
	return false
}

// ReadPath reads the image.json of the squashfs at path: an A/B slot's
// data partition, or a root filesystem image.
func ReadPath(path string) (*schematic.ImageInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	return Read(f, size)
}

// readerFile is an io.ReaderAt as the fs.File go-diskfs reads from.
type readerFile struct {
	r    io.ReaderAt
	size int64
	off  int64
}

func (f *readerFile) ReadAt(p []byte, off int64) (int, error) { return f.r.ReadAt(p, off) }

func (f *readerFile) Read(p []byte) (int, error) {
	n, err := f.r.ReadAt(p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *readerFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.off
	case io.SeekEnd:
		offset += f.size
	default:
		return 0, errors.New("seek: bad whence")
	}
	if offset < 0 {
		return 0, errors.New("seek: negative position")
	}
	f.off = offset
	return offset, nil
}

func (f *readerFile) Stat() (fs.FileInfo, error) { return sizeInfo(f.size), nil }
func (f *readerFile) Close() error               { return nil }

type sizeInfo int64

func (s sizeInfo) Name() string       { return "squashfs" }
func (s sizeInfo) Size() int64        { return int64(s) }
func (s sizeInfo) Mode() fs.FileMode  { return 0o400 }
func (s sizeInfo) ModTime() time.Time { return time.Time{} }
func (s sizeInfo) IsDir() bool        { return false }
func (s sizeInfo) Sys() any           { return nil }
