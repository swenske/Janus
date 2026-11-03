package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/swenske/Janus/internal/verity"
)

// testBundle is a root filesystem of n 4 KiB blocks, its hash tree with
// a superblock's room, and a UKI whose command line pins them.
func testBundle(t *testing.T, n int) (data, hashTree, uki []byte) {
	t.Helper()
	data = make([]byte, n*4096)
	for i := range data {
		data[i] = byte(i*7 + i/4096)
	}
	table := &verity.Table{Version: 1, DataBlockSize: 4096, HashBlockSize: 4096, DataBlocks: uint64(n),
		HashStartBlock: 1, Algorithm: "sha256", Salt: []byte{1, 2, 3}}
	tree, root, err := table.Build(data)
	if err != nil {
		t.Fatal(err)
	}
	hashTree = append(make([]byte, 4096), tree...)
	uki = minimalPE(t, fmt.Sprintf(`console=ttyS0 dm-mod.create="vroot,,,ro,0 %d verity 1 PARTLABEL=BOOT-B-DATA PARTLABEL=BOOT-B-HASH 4096 4096 %d 1 sha256 %x 010203" root=/dev/dm-0 ro`, n*8, n, root))
	return data, hashTree, uki
}

// TestHashTreeAgainstTheUKI: rootfs.verity must end in the root digest
// the UKI's command line pins - checked before the root filesystem is
// read; a refusal names the file and says nothing was written.
func TestHashTreeAgainstTheUKI(t *testing.T) {
	_, hashTree, uki := testBundle(t, 40)
	table, err := ukiVerityTable("uki-b.efi", uki)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkHashTree(table, "uki-b.efi", hashTree); err != nil {
		t.Fatalf("its own tree: %v", err)
	}
	_, otherTree, _ := testBundle(t, 41)
	for name, tree := range map[string][]byte{"another build's tree": otherTree, "a short tree": hashTree[:4096]} {
		_, err := checkHashTree(table, "uki-b.efi", tree)
		if status.Code(err) != codes.FailedPrecondition || !strings.HasPrefix(status.Convert(err).Message(), "rootfs.verity: ") || !strings.Contains(err.Error(), "nothing was written") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, u := range map[string][]byte{"no verity table": minimalPE(t, "console=ttyS0 ro"), "not a UKI": []byte("MZ")} {
		if _, err := ukiVerityTable("uki-b.efi", u); status.Code(err) != codes.FailedPrecondition || !strings.HasPrefix(status.Convert(err).Message(), "uki-b.efi: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestWriteSlotData: the root filesystem is streamed into the slot, each
// block checked first. A block refused within the first MiB leaves the
// slot as it was; refused later, the slot holds what was written before
// it - and an upload already in the slot is checked in place.
func TestWriteSlotData(t *testing.T) {
	data, hashTree, uki := testBundle(t, 400) // 1.6 MiB: past the 1 MiB buffer
	table, err := ukiVerityTable("uki-b.efi", uki)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := checkHashTree(table, "uki-b.efi", hashTree)
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Repeat([]byte{0xaa}, 4<<20)
	slot := filepath.Join(t.TempDir(), "BOOT-B-DATA")
	reset := func() {
		if err := os.WriteFile(slot, old, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []byte {
		b, err := os.ReadFile(slot)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	src := func(b []byte) *bundleData {
		return &bundleData{ReadCloser: io.NopCloser(bytes.NewReader(b)), size: int64(len(b))}
	}

	reset()
	written, sum, err := writeSlotData(slot, table, tree, src(data))
	want := sha256.Sum256(data)
	if err != nil || written != int64(len(data)) || sum != hex.EncodeToString(want[:]) {
		t.Fatalf("its own data: %d bytes, %s, %v", written, sum, err)
	}
	if got := read(); !bytes.Equal(got[:len(data)], data) || !bytes.Equal(got[len(data):], old[len(data):]) || len(got) != len(old) {
		t.Error("the slot doesn't hold the data, or was truncated")
	}

	reset()
	early := bytes.Clone(data)
	early[100*4096] ^= 1
	written, _, err = writeSlotData(slot, table, tree, src(early))
	if !errors.Is(err, verity.ErrData) || written != 0 || !bytes.Equal(read(), old) {
		t.Errorf("block 100 refused: %d bytes reached the slot, %v", written, err)
	}

	reset()
	late := bytes.Clone(data)
	late[350*4096] ^= 1
	written, _, err = writeSlotData(slot, table, tree, src(late))
	if !errors.Is(err, verity.ErrData) || written != 1<<20 {
		t.Errorf("block 350 refused: %d bytes reached the slot, %v", written, err)
	}
	if got := read(); !bytes.Equal(got[:1<<20], data[:1<<20]) || !bytes.Equal(got[1<<20:], old[1<<20:]) {
		t.Error("the slot doesn't hold exactly the first MiB")
	}

	reset()
	staged := &bundleData{ReadCloser: io.NopCloser(bytes.NewReader(data)), size: int64(len(data)), staged: true}
	if written, _, err := writeSlotData(slot, table, tree, staged); err != nil || written != 0 || !bytes.Equal(read(), old) {
		t.Errorf("staged: %d bytes written, %v", written, err)
	}
}

// TestOpenBundleData: a local bundle's file, a download (its size from
// Content-Length), and an upload staged in a slot - read in place, from
// the slot it was uploaded into only.
func TestOpenBundleData(t *testing.T) {
	dir := t.TempDir()
	data := []byte(strings.Repeat("squashfs", 1000))
	if err := os.WriteFile(filepath.Join(dir, "rootfs.squashfs"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	readAll := func(b *bundleData, err error) ([]byte, *bundleData) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		got, err := io.ReadAll(b)
		if err != nil {
			t.Fatal(err)
		}
		return got, b
	}

	got, b := readAll(openBundleData(context.Background(), dir, "B", ""))
	if !bytes.Equal(got, data) || b.size != int64(len(data)) || b.staged {
		t.Errorf("local file: %d bytes, size %d, staged %v", len(got), b.size, b.staged)
	}

	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	got, b = readAll(openBundleData(context.Background(), srv.URL+"/", "B", ""))
	if !bytes.Equal(got, data) || b.size != int64(len(data)) {
		t.Errorf("download: %d bytes, size %d", len(got), b.size)
	}
	if _, err := openBundleData(context.Background(), srv.URL+"/missing", "B", ""); !errors.Is(err, errBundleFileMissing) {
		t.Errorf("a 404: %v", err)
	}

	staging := t.TempDir()
	saved := releaseStagingDir
	releaseStagingDir = staging
	t.Cleanup(func() { releaseStagingDir = saved })
	slot := filepath.Join(t.TempDir(), "BOOT-B-DATA")
	if err := os.WriteFile(slot, append(bytes.Clone(data), bytes.Repeat([]byte{0}, 4096)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileDurably(filepath.Join(staging, stagedMarker), []byte(fmt.Sprintf(`{"slot":"B","bytes":%d,"sha256":"x"}`, len(data)))); err != nil {
		t.Fatal(err)
	}
	got, b = readAll(openBundleData(context.Background(), staging, "B", slot))
	if !bytes.Equal(got, data) || !b.staged || b.size != int64(len(data)) {
		t.Errorf("staged in the slot: %d bytes, staged %v, size %d", len(got), b.staged, b.size)
	}
	if _, err := openBundleData(context.Background(), staging, "A", slot); err == nil || !strings.Contains(err.Error(), "upload it again") {
		t.Errorf("uploaded into another slot: %v", err)
	}
}

// TestSlotTooSmall says what doesn't fit and what to do instead.
func TestSlotTooSmall(t *testing.T) {
	err := slotTooSmall("this release's root filesystem", 90<<20, 64<<20)
	msg := status.Convert(err).Message()
	for _, want := range []string{"is 90 MiB", "slots are 64 MiB", "recycle it", "state export/import"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q lacks %q", msg, want)
		}
	}
	if msg := status.Convert(slotTooSmall("rootfs.squashfs", 0, 128<<20)).Message(); !strings.Contains(msg, "doesn't fit: this node's slots are 128 MiB") {
		t.Errorf("unknown size: %q", msg)
	}
}
