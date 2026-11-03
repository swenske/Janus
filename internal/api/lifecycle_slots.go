package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/bootslot"
	"github.com/swenske/Janus/internal/espswitch"
	"github.com/swenske/Janus/internal/events"
	"github.com/swenske/Janus/internal/verity"
)

// A slot's root filesystem is never held in memory: Upgrade streams it
// into the inactive slot, each block checked against the UKI's hash tree
// before it's written (internal/verity), and UploadReleaseFile writes an
// uploaded rootfs.squashfs straight into that slot too. A slot is as big
// as its partition: 64 MiB on disks made before 128 MiB slots, which keep
// them for good.

// updateMu: one operation on the slots at a time - Upgrade, Rollback, an
// upload into the inactive slot.
var updateMu sync.Mutex

// lockSlots takes updateMu, or says what holds it.
func lockSlots() (func(), error) {
	if !updateMu.TryLock() {
		return nil, status.Error(codes.FailedPrecondition, "another update, upload or rollback is running on this node - try again once it's done")
	}
	return updateMu.Unlock, nil
}

// partitionSize is a block device's size in bytes - a variable for tests.
var partitionSize = func(dev string) (int64, error) {
	f, err := os.Open(dev)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Seek(0, io.SeekEnd)
}

// SlotBytes is the size of this node's A/B data partitions - the largest
// root filesystem an update can write; 0 when this boot isn't from a slot.
var SlotBytes = sync.OnceValue(func() uint64 {
	bc, err := resolveBootContext()
	if err != nil {
		return 0
	}
	dev, ok := bootslot.SlotDataDevice(bc.disk, bc.targetSlot)
	if !ok {
		return 0
	}
	n, err := partitionSize(dev)
	if err != nil {
		log.Printf("lifecycle: size of %s: %v", dev, err)
		return 0
	}
	return uint64(n)
})

// mib formats a byte count in MiB, as the slots are spoken of.
func mib(n uint64) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// slotTooSmall refuses a root filesystem bigger than the node's slots
// (need 0: by how much isn't known yet).
func slotTooSmall(what string, need, slot uint64) error {
	size := "is " + mib(need) + ", and"
	if need == 0 {
		size = "doesn't fit:"
	}
	return status.Errorf(codes.FailedPrecondition,
		"%s %s this node's slots are %s - a disk keeps the slots it was installed with. Replace the node with one installed from a newer image instead: recycle it if it's one of a cluster's machines, or move its state to a new node (janusctl system state export/import)",
		what, size, mib(slot))
}

// ukiVerityTable is the dm-verity table a UKI's command line boots.
func ukiVerityTable(ukiName string, uki []byte) (*verity.Table, error) {
	cmdline, err := ukiCmdline(uki)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%s: %v", ukiName, err)
	}
	table, err := verity.FromCmdline(cmdline)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%s: %v", ukiName, err)
	}
	return table, nil
}

// notTheUKIs says a bundle file isn't the one a UKI was built with.
func notTheUKIs(file string, err error, ukiName, written string) error {
	return status.Errorf(codes.FailedPrecondition,
		"%s: %v - it isn't the one %s was built with (damaged on its way, or the bundle mixes two builds)%s", file, err, ukiName, written)
}

const nothingWritten = ", and the node couldn't mount its root from it: nothing was written"

// checkHashTree checks a bundle's rootfs.verity against the root digest
// its UKI's table pins, before anything of the root filesystem is read.
func checkHashTree(table *verity.Table, ukiName string, hashTree []byte) (*verity.Tree, error) {
	tree, err := table.CheckTree(hashTree)
	if err != nil {
		return nil, notTheUKIs("rootfs.verity", err, ukiName, nothingWritten)
	}
	return tree, nil
}

// stagedMarker, in releaseStagingDir, says an uploaded rootfs.squashfs
// was written straight into a slot's data partition (stagedData).
const stagedMarker = "rootfs.squashfs.slot"

// stagedData is what stagedMarker records.
type stagedData struct {
	Slot   string `json:"slot"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// bundleData is where Upgrade reads a bundle's root filesystem from.
type bundleData struct {
	io.ReadCloser
	size   int64 // -1 when the source doesn't say
	staged bool  // already in the target slot (an upload): read in place
}

// openBundleData opens a bundle's rootfs.squashfs as a stream: an http(s)
// download, a file of a local bundle, or - for UploadReleaseFile's
// staging directory - the target slot's data partition it was uploaded
// into.
func openBundleData(ctx context.Context, bundleRef, targetSlot, targetDataDev string) (*bundleData, error) {
	if strings.HasPrefix(bundleRef, "http://") || strings.HasPrefix(bundleRef, "https://") {
		url := strings.TrimSuffix(bundleRef, "/") + "/rootfs.squashfs"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("build request for %s: %w", url, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", url, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				return nil, fmt.Errorf("GET %s: %s: %w", url, resp.Status, errBundleFileMissing)
			}
			return nil, fmt.Errorf("GET %s: unexpected status %s", url, resp.Status)
		}
		return &bundleData{ReadCloser: resp.Body, size: resp.ContentLength}, nil
	}
	if filepath.Clean(bundleRef) == filepath.Clean(releaseStagingDir) {
		if b, err := os.ReadFile(filepath.Join(releaseStagingDir, stagedMarker)); err == nil {
			var st stagedData
			if err := json.Unmarshal(b, &st); err != nil {
				return nil, fmt.Errorf("%s: %w", stagedMarker, err)
			}
			if st.Slot != targetSlot {
				return nil, fmt.Errorf("it was uploaded into slot %s, and an update now writes slot %s: upload it again", st.Slot, targetSlot)
			}
			f, err := os.Open(targetDataDev)
			if err != nil {
				return nil, err
			}
			return &bundleData{ReadCloser: readCloser{io.NewSectionReader(f, 0, st.Bytes), f}, size: st.Bytes, staged: true}, nil
		}
	}
	f, err := os.Open(filepath.Join(bundleRef, "rootfs.squashfs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %w", err, errBundleFileMissing)
	}
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &bundleData{ReadCloser: f, size: fi.Size()}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// writeSlotData streams a root filesystem into a slot's data partition,
// each block checked against tree before it's written; when data is
// staged (already in the partition), it's checked in place. Returns the
// bytes written and the SHA-256 of what was read.
func writeSlotData(dev string, table *verity.Table, tree *verity.Tree, data *bundleData) (int64, string, error) {
	sum := sha256.New()
	src := io.TeeReader(data, sum)
	if data.staged {
		_, err := table.Copy(io.Discard, src, tree)
		return 0, hex.EncodeToString(sum.Sum(nil)), err
	}
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		return 0, "", err
	}
	// Buffered: a block refused within the first MiB leaves the slot as
	// it was.
	w := bufio.NewWriterSize(f, 1<<20)
	written, err := table.Copy(w, src, tree)
	if err != nil {
		f.Close()
		return written - int64(w.Buffered()), "", err
	}
	if err := errors.Join(w.Flush(), f.Sync()); err != nil {
		f.Close()
		return written, "", err
	}
	return written, hex.EncodeToString(sum.Sum(nil)), f.Close()
}

// verifySlotHook is verifySlot - a variable for tests.
var verifySlotHook = verifySlot

// verifySlot checks that a slot's data and hash partitions hold what the
// UKI staged for it boots: Rollback won't switch to a slot an update
// wrote part of and gave up on.
func verifySlot(bc *bootContext, slot string) error {
	if err := espswitch.Mount(bc.espDevice); err != nil {
		return err
	}
	ukiPath := filepath.Join(espswitch.Mountpoint, "JANUS", fmt.Sprintf("UKI-%s.EFI", slot))
	uki, readErr := os.ReadFile(ukiPath)
	if err := espswitch.Unmount(); err != nil {
		log.Printf("lifecycle: unmount %s: %v", espswitch.Mountpoint, err)
	}
	if errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", espswitch.ErrUKINotStaged, ukiPath)
	}
	if readErr != nil {
		return readErr
	}
	ukiName := fmt.Sprintf("its kernel image (UKI-%s.EFI)", slot)
	table, err := ukiVerityTable(ukiName, uki)
	if err != nil {
		return err
	}
	dataDev, _ := bootslot.SlotDataDevice(bc.disk, slot)
	hashDev, _ := bootslot.SlotHashDevice(bc.disk, slot)
	hf, err := os.Open(hashDev)
	if err != nil {
		return err
	}
	hashTree := make([]byte, table.HashBytes())
	_, err = io.ReadFull(hf, hashTree)
	hf.Close()
	if err != nil {
		return fmt.Errorf("read %s: %w", hashDev, err)
	}
	tree, err := table.CheckTree(hashTree)
	if err != nil {
		return fmt.Errorf("its hash partition: %w", err)
	}
	df, err := os.Open(dataDev)
	if err != nil {
		return err
	}
	defer df.Close()
	if _, err := table.Copy(io.Discard, io.NewSectionReader(df, 0, int64(table.DataBytes())), tree); err != nil {
		return fmt.Errorf("its data partition: %w", err)
	}
	return nil
}

// uploadIntoSlot is UploadReleaseFile for rootfs.squashfs: written
// straight into the inactive slot's data partition - which the Upgrade
// that follows replaces anyway - rather than staged on STATE, which may
// be smaller than a slot. The slot no longer holds its previous release:
// Rollback refuses it (verifySlot) until an update completes.
// The caller holds updateMu.
func (l *Lifecycle) uploadIntoSlot(first *janusv1alpha1.UploadReleaseFileRequest, stream janusv1alpha1.LifecycleService_UploadReleaseFileServer) error {
	bc, err := resolveBootContext()
	if err != nil {
		return err
	}
	dev, _ := bootslot.SlotDataDevice(bc.disk, bc.targetSlot)
	size, err := partitionSize(dev)
	if err != nil {
		return status.Errorf(codes.Internal, "size of %s: %v", dev, err)
	}
	if n := first.GetSize(); n > uint64(size) {
		return slotTooSmall("rootfs.squashfs", n, uint64(size))
	}
	if err := os.MkdirAll(releaseStagingDir, 0o700); err != nil {
		return status.Errorf(codes.Internal, "mkdir %s: %v", releaseStagingDir, err)
	}
	markerPath := filepath.Join(releaseStagingDir, stagedMarker)
	// An older upload's marker, or file, no longer stands.
	for _, p := range []string{markerPath, filepath.Join(releaseStagingDir, "rootfs.squashfs")} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return status.Errorf(codes.Internal, "remove %s: %v", p, err)
		}
	}

	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		return status.Errorf(codes.Internal, "open %s: %v", dev, err)
	}
	slotWriting(bc.targetSlot)
	sum := sha256.New()
	w := bufio.NewWriterSize(io.MultiWriter(f, sum), 1<<20)
	var written int64
	fail := func(err error) error {
		f.Close()
		slotWritten(bc.targetSlot, nil, errors.New("holds part of an upload that didn't complete"), false)
		return err
	}
	for msg := first; ; {
		chunk := msg.GetChunk()
		if written+int64(len(chunk)) > size {
			return fail(slotTooSmall("rootfs.squashfs", 0, uint64(size)))
		}
		if _, err := w.Write(chunk); err != nil {
			return fail(status.Errorf(codes.Internal, "write %s: %v", dev, err))
		}
		written += int64(len(chunk))
		var err error
		if msg, err = stream.Recv(); err == io.EOF {
			break
		} else if err != nil {
			return fail(status.Errorf(codes.Internal, "receive rootfs.squashfs: %v", err))
		}
	}
	if err := errors.Join(w.Flush(), f.Sync()); err != nil {
		return fail(status.Errorf(codes.Internal, "write %s: %v", dev, err))
	}
	if err := f.Close(); err != nil {
		return fail(status.Errorf(codes.Internal, "close %s: %v", dev, err))
	}
	marker, _ := json.Marshal(stagedData{Slot: bc.targetSlot, Bytes: written, SHA256: hex.EncodeToString(sum.Sum(nil))})
	if err := writeFileDurably(markerPath, marker); err != nil {
		return fail(status.Errorf(codes.Internal, "write %s: %v", markerPath, err))
	}
	slotWritten(bc.targetSlot, nil, errors.New("holds an uploaded root filesystem, waiting for its update"), false)

	events.Publish("lifecycle.release_file.uploaded", map[string]any{"filename": "rootfs.squashfs", "bytes": written, "slot": bc.targetSlot})
	return stream.SendAndClose(&janusv1alpha1.UploadReleaseFileResponse{
		StagingDir:   releaseStagingDir,
		BytesWritten: uint64(written),
	})
}

// writeFileDurably writes a small file so that a power cut leaves either
// the old one or the new one: tmp, fsync, rename, directory fsync.
func writeFileDurably(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
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
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
