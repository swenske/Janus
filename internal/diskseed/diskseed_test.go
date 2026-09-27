package diskseed

import (
	"os"
	"path/filepath"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// buildTestDisk creates a minimal disk with a single GPT partition
// named "STATE", ext4-formatted - just enough shape for SeedController
// to find and write to, without needing a real Install/native-janusd
// round trip (that's covered end to end by hack/lifecycle-install-test.sh
// and this package's own real-world use from cmd/janusctl instead).
func buildTestDisk(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.img")

	const sectorSize = 512
	const alignSectors = 2048                          // 1MiB, matching internal/diskimage's own alignment
	const stateSectors = 32 * 1024 * 1024 / sectorSize // 32MiB - comfortably above ext4's practical minimum

	d, err := diskfs.Create(path, int64(alignSectors+stateSectors)*sectorSize, diskfs.SectorSize(sectorSize))
	if err != nil {
		t.Fatalf("diskfs.Create: %v", err)
	}
	table := &gpt.Table{
		ProtectiveMBR: true,
		Partitions: []*gpt.Partition{
			{Index: 1, Start: alignSectors, End: alignSectors + stateSectors - 1, Type: gpt.LinuxFilesystem, Name: "STATE"},
		},
	}
	if err := d.Partition(table); err != nil {
		t.Fatalf("Partition: %v", err)
	}
	if _, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 1, FSType: filesystem.TypeExt4, VolumeLabel: "janus-state"}); err != nil {
		t.Fatalf("CreateFilesystem: %v", err)
	}
	return path
}

func readBack(t *testing.T, diskPath, name string) []byte {
	t.Helper()
	d, err := diskfs.Open(diskPath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		t.Fatalf("re-open disk: %v", err)
	}
	fs, err := d.GetFilesystem(1)
	if err != nil {
		t.Fatalf("GetFilesystem: %v", err)
	}
	f, err := fs.OpenFile("controller/"+name, os.O_RDONLY)
	if err != nil {
		t.Fatalf("open controller/%s: %v", name, err)
	}
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	return buf[:n]
}

func TestSeedControllerWritesFiles(t *testing.T) {
	path := buildTestDisk(t)

	if err := SeedController(path, "controller.example.com:8443", []byte("fake-ca-cert-1")); err != nil {
		t.Fatalf("SeedController: %v", err)
	}

	if got := string(readBack(t, path, "address")); got != "controller.example.com:8443" {
		t.Errorf("controller/address = %q, want %q", got, "controller.example.com:8443")
	}
	if got := string(readBack(t, path, "ca.crt")); got != "fake-ca-cert-1" {
		t.Errorf("controller/ca.crt = %q, want %q", got, "fake-ca-cert-1")
	}
}

func TestSeedControllerRefusesAnAlreadySeededDisk(t *testing.T) {
	path := buildTestDisk(t)

	if err := SeedController(path, "old.example.com:8443", []byte("old-ca-cert")); err != nil {
		t.Fatalf("first SeedController: %v", err)
	}
	err := SeedController(path, "new.example.com:9999", []byte("new-ca-cert"))
	if err == nil {
		t.Fatal("expected the second SeedController call to be refused, got nil error")
	}

	// The refused second call must not have touched the first call's
	// own values.
	if got := string(readBack(t, path, "address")); got != "old.example.com:8443" {
		t.Errorf("controller/address after refused re-seed = %q, want the original %q", got, "old.example.com:8443")
	}
	if got := string(readBack(t, path, "ca.crt")); got != "old-ca-cert" {
		t.Errorf("controller/ca.crt after refused re-seed = %q, want the original %q", got, "old-ca-cert")
	}
}

func TestSeedControllerRequiresAddressAndCA(t *testing.T) {
	path := buildTestDisk(t)

	if err := SeedController(path, "", []byte("ca")); err == nil {
		t.Error("expected an error with an empty address")
	}
	if err := SeedController(path, "host:8443", nil); err == nil {
		t.Error("expected an error with an empty CA cert")
	}
}

func TestSeedControllerRejectsDiskWithNoStatePartition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blank.img")
	if err := os.WriteFile(path, make([]byte, 8*1024*1024), 0o644); err != nil {
		t.Fatalf("write blank disk: %v", err)
	}

	if err := SeedController(path, "host:8443", []byte("ca")); err == nil {
		t.Error("expected an error against a disk with no partition table at all")
	}
}
