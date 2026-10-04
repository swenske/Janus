package api

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// fileChunk is the Data message size for Read and Copy.
const fileChunk = 64 * 1024

// virtualRoots are kernel pseudo-filesystems a recursive walk started
// above them doesn't descend into: endless, and nothing a file listing,
// size total or archive can represent faithfully.
var virtualRoots = map[string]bool{"/proc": true, "/sys": true, "/dev": true}

func cleanAbs(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", status.Errorf(codes.InvalidArgument, "path %q must be absolute", p)
	}
	return filepath.Clean(p), nil
}

// skipVirtual reports whether a walk rooted at root should skip path.
func skipVirtual(root, path string) bool {
	return path != root && virtualRoots[path]
}

// List lists root's entries (every descendant with recursive), without
// following symlinks. Per-entry errors are reported in FileInfo.error
// instead of aborting the listing.
func (s *System) List(req *janusv1alpha1.ListRequest, stream janusv1alpha1.SystemService_ListServer) error {
	root, err := cleanAbs(req.GetRoot())
	if err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return status.Errorf(codes.NotFound, "%v", err)
	}
	if !info.IsDir() {
		return stream.Send(fileInfo(root, filepath.Base(root), info))
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if path == root {
			return walkErr
		}
		rel, _ := filepath.Rel(root, path)
		if walkErr != nil {
			return stream.Send(&janusv1alpha1.FileInfo{Name: path, RelativeName: rel, Error: walkErr.Error()})
		}
		info, err := d.Info()
		if err != nil {
			return stream.Send(&janusv1alpha1.FileInfo{Name: path, RelativeName: rel, Error: err.Error()})
		}
		if err := stream.Send(fileInfo(path, rel, info)); err != nil {
			return err
		}
		if d.IsDir() && (!req.GetRecursive() || skipVirtual(root, path)) {
			return filepath.SkipDir
		}
		return nil
	})
}

func fileInfo(path, rel string, info fs.FileInfo) *janusv1alpha1.FileInfo {
	return &janusv1alpha1.FileInfo{
		Name:         path,
		RelativeName: rel,
		Size:         info.Size(),
		Mode:         uint32(info.Mode()),
		IsDir:        info.IsDir(),
	}
}

// Read streams one file's content. Only regular files are read: block
// and character devices (reading a disk raw isn't what this is for),
// FIFOs (a read with no writer would block the call forever) and sockets
// are refused. The file is opened non-blocking and its type checked on
// the open descriptor, so it can't be swapped for something else between
// the check and the read. The node's secrets are refused (secretfiles.go).
func (s *System) Read(req *janusv1alpha1.ReadRequest, stream janusv1alpha1.SystemService_ReadServer) error {
	path, err := cleanAbs(req.GetPath())
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return status.Errorf(codes.NotFound, "%v", err)
		}
		return status.Errorf(codes.PermissionDenied, "%v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}
	switch mode := info.Mode(); {
	case mode.IsDir():
		return status.Errorf(codes.InvalidArgument, "%s is a directory - use List or Copy", path)
	case !mode.IsRegular():
		return status.Errorf(codes.PermissionDenied, "%s is not a regular file (%s) - not readable through this API", path, mode.Type())
	}
	if err := checkServable(f); err != nil {
		return secretError(path, err)
	}
	r := &keyGuard{r: f}
	buf := make([]byte, fileChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&janusv1alpha1.Data{Bytes: append([]byte(nil), buf[:n]...)}); sendErr != nil {
				return nil
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return secretError(path, err)
		}
	}
}

// secretError is the status for a file Read or Copy won't serve, or
// couldn't read.
func secretError(path string, err error) error {
	if errors.Is(err, errSecretPath) || errors.Is(err, errPrivateKey) {
		return status.Errorf(codes.PermissionDenied, "%s %v - never served by the file API", path, err)
	}
	return status.Errorf(codes.Internal, "read %s: %v", path, err)
}

// Copy streams root_path (a file or a whole directory tree) as a tar
// archive: regular files, directories and symlinks; devices, sockets and
// FIFOs are left out, and so are the node's secrets (secretfiles.go).
// /proc and /sys are refused - their files report a size of 0, which a
// tar header can't carry correctly (use Read).
func (s *System) Copy(req *janusv1alpha1.CopyRequest, stream janusv1alpha1.SystemService_CopyServer) error {
	root, err := cleanAbs(req.GetRootPath())
	if err != nil {
		return err
	}
	if root == "/proc" || strings.HasPrefix(root, "/proc/") || root == "/sys" || strings.HasPrefix(root, "/sys/") {
		return status.Errorf(codes.InvalidArgument, "%s is a kernel pseudo-filesystem - use Read for individual files", root)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return status.Errorf(codes.NotFound, "%v", err)
	}
	// Where root really is, to leave a secret directory out whole (each
	// file is checked on its own anyway, wherever it's reached from).
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}
	// A secret asked for by name is refused, not an empty archive.
	if isSecretPath(realRoot) {
		return secretError(root, errSecretPath)
	}
	if info.Mode().IsRegular() {
		f, err := os.Open(root)
		if err != nil {
			return status.Errorf(codes.PermissionDenied, "%v", err)
		}
		err = checkServable(f)
		f.Close()
		if err != nil {
			return secretError(root, err)
		}
	}

	w := &dataWriter{send: func(b []byte) error { return stream.Send(&janusv1alpha1.Data{Bytes: b}) }}
	tw := tar.NewWriter(w)
	base := filepath.Dir(root)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // unreadable entry: left out, the rest still copied
		}
		if skipVirtual(root, path) {
			return filepath.SkipDir
		}
		if rel, _ := filepath.Rel(root, path); d.IsDir() && isSecretPath(filepath.Join(realRoot, rel)) {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		mode := info.Mode()
		if !mode.IsRegular() && !mode.IsDir() && mode&fs.ModeSymlink == 0 {
			return nil
		}
		link := ""
		if mode&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return nil
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil
		}
		hdr.Name, _ = filepath.Rel(base, path)
		if mode.IsDir() {
			hdr.Name += "/"
		}
		if !mode.IsRegular() {
			return tw.WriteHeader(hdr)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		if checkServable(f) != nil {
			return nil // a secret (or a file that can't be checked): left out
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		// A file that changed size meanwhile would corrupt the archive;
		// copy exactly the size the header announced. One that turned
		// into a secret since it was checked aborts the archive.
		if _, err := io.CopyN(tw, &keyGuard{r: f}, hdr.Size); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		return status.Errorf(codes.Internal, "archive %s: %v", root, err)
	}
	if err := tw.Close(); err != nil {
		return status.Errorf(codes.Internal, "archive %s: %v", root, err)
	}
	return w.flush()
}

// dataWriter batches writes into fileChunk-sized Data messages.
type dataWriter struct {
	send func([]byte) error
	buf  []byte
}

func (w *dataWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for len(w.buf) >= fileChunk {
		if err := w.send(append([]byte(nil), w.buf[:fileChunk]...)); err != nil {
			return 0, err
		}
		w.buf = w.buf[fileChunk:]
	}
	return len(p), nil
}

func (w *dataWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	err := w.send(w.buf)
	w.buf = nil
	return err
}

// DiskUsage reports each requested path's total size (apparent size of
// every regular file under it, not crossing into /proc, /sys or /dev);
// with recursive, also one entry per directory beneath it.
func (s *System) DiskUsage(req *janusv1alpha1.DiskUsageRequest, stream janusv1alpha1.SystemService_DiskUsageServer) error {
	if len(req.GetPaths()) == 0 {
		return status.Error(codes.InvalidArgument, "at least one path is required")
	}
	for _, p := range req.GetPaths() {
		root, err := cleanAbs(p)
		if err != nil {
			return err
		}
		entries, err := diskUsage(root, req.GetRecursive())
		if err != nil {
			return status.Errorf(codes.NotFound, "%v", err)
		}
		for _, e := range entries {
			if err := stream.Send(e); err != nil {
				return nil
			}
		}
	}
	return nil
}

// diskUsage walks root once, totalling each directory's size, and
// returns root's total (or, with perDir, every directory's total,
// deepest first, root last).
func diskUsage(root string, perDir bool) ([]*janusv1alpha1.DiskUsageInfo, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []*janusv1alpha1.DiskUsageInfo{{Path: root, SizeBytes: info.Size()}}, nil
	}
	totals := map[string]int64{}
	var order []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipVirtual(root, path) {
				return filepath.SkipDir
			}
			order = append(order, path)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
			totals[dir] += fi.Size()
			if dir == root || dir == "/" {
				break
			}
		}
		return nil
	})
	var out []*janusv1alpha1.DiskUsageInfo
	if perDir {
		for i := len(order) - 1; i >= 0; i-- {
			out = append(out, &janusv1alpha1.DiskUsageInfo{Path: order[i], SizeBytes: totals[order[i]], IsDir: true})
		}
		return out, nil
	}
	return []*janusv1alpha1.DiskUsageInfo{{Path: root, SizeBytes: totals[root], IsDir: true}}, nil
}
