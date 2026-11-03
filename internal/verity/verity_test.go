package verity

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// shape is one table veritysetup is compared with: every algorithm and
// format the package takes, block sizes that make 0 to 3 levels, and
// the level boundaries.
type shape struct {
	name               string
	version            int
	algorithm          string
	dataBlock, hashBlk int
	blocks             int
	salt               string // hex, "-" for none
	superblock         bool
	// What veritysetup made (TestAgainstVeritysetup prints them): the
	// root digest and the SHA-256 of the tree, from hash_start_block on.
	root, treeSHA256 string
}

const salt32 = "4a616e75732073616c7420666f722074686520766572697479207465737473ff"

var shapes = []shape{
	{"janus", 1, "sha256", 4096, 4096, 300, salt32, true,
		"17f3b526052145ddcdf6ebf115b1f87f294a848b0064134f9270da69e73d922c",
		"88b5ed3d3b35afe7ac857f711fe8367612e1d91f48c87181305802a5760deac4"},
	{"small-blocks-3-levels", 1, "sha256", 512, 512, 1000, salt32, true,
		"94313515d25380ebe771df68ae53eb4e5afecc59444ea46a3b0b02dc7a6604cf",
		"253a867a6665882ae11f3febf10be45f178660caa2a8e1c56873dfe9e17cc7f7"},
	{"format-0", 0, "sha256", 4096, 4096, 300, salt32, false,
		"7c7965c490dc26ac54b84fdc580a5ab454bdc370228b4c8f4b1743c314dd154e",
		"40b67ed7121ea5ad0631f7466d36cf96253fd6036127243fbd3da384f62b77ff"},
	{"sha512", 1, "sha512", 4096, 4096, 300, salt32, true,
		"1004f66c58890c5c07d3ca7b8446e4de0908c6520da26f020638f8b216188f52ddbeb7d4a8069b52a86c66362676ad8c71ed1cb2eb763f5108a8b9ecd0beedde",
		"e0ac9bf2d8fef686e7e7b7d991c9040f6a7fc7e043016ee055aa4309db0e1315"},
	{"no-salt", 1, "sha256", 4096, 4096, 129, "-", true,
		"5a5e63bfa915d37a224623dadd0b47462a3ae8eae656098584acc7424dd986eb",
		"b4e86a4ca0e11563f802391c819869677c6a4f6cbc2a3af0207b7a18ec8c88ac"},
	{"one-block", 1, "sha256", 4096, 4096, 1, salt32, true,
		"35f7dc0999afdcd7211ab46ac9cf26e7e8b907f1d002abb662d3e4db825e2244",
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	{"one-full-level", 1, "sha256", 4096, 4096, 128, salt32, false,
		"60fb69d2604f5cf7eb0eebe185263ba9eadb90274b56a84a1a45655ebc8570ea",
		"0535dd2f4606ede234b17b715505a807130bf199f875aeca711242bb4878d683"},
	{"mixed-sizes", 1, "sha256", 4096, 1024, 200, "00ff", true,
		"ddff5b63595bcd7f7373635eb59bca019a71001aa5639062250a6cd14fd635b3",
		"cec67f6ecf0afa62fbdd4628d4148b212bdb7118577edacb55fef89fe84f7a4b"},
}

// testData is n blocks of deterministic bytes: SHA-256 of a counter.
func testData(n, blockSize int) []byte {
	data := make([]byte, 0, n*blockSize)
	var ctr [8]byte
	for k := 0; len(data) < n*blockSize; k++ {
		binary.LittleEndian.PutUint64(ctr[:], uint64(k))
		sum := sha256.Sum256(ctr[:])
		data = append(data, sum[:]...)
	}
	return data[:n*blockSize]
}

func (s shape) table() *Table {
	t := &Table{
		Version: s.version, DataBlockSize: s.dataBlock, HashBlockSize: s.hashBlk,
		DataBlocks: uint64(s.blocks), Algorithm: s.algorithm,
	}
	if s.superblock {
		t.HashStartBlock = 1
	}
	if s.salt != "-" {
		t.Salt, _ = hex.DecodeString(s.salt)
	}
	return t
}

// TestKnownTrees: the trees and roots veritysetup made for each shape.
func TestKnownTrees(t *testing.T) {
	for _, s := range shapes {
		if s.root == "" {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			tree, root, err := s.table().Build(testData(s.blocks, s.dataBlock))
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(root); got != s.root {
				t.Errorf("root %s, veritysetup's %s", got, s.root)
			}
			if got := fmt.Sprintf("%x", sha256.Sum256(tree)); s.treeSHA256 != "" && got != s.treeSHA256 {
				t.Errorf("tree SHA-256 %s, veritysetup's %s", got, s.treeSHA256)
			}
		})
	}
}

// veritysetup finds the tool where Debian puts it, outside a user's PATH.
func veritysetup() string {
	if p, err := exec.LookPath("veritysetup"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/sbin/veritysetup", "/sbin/veritysetup"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// TestAgainstVeritysetup builds every shape with veritysetup itself,
// when it's installed, and checks Build and Verify agree with it.
func TestAgainstVeritysetup(t *testing.T) {
	tool := veritysetup()
	if tool == "" {
		t.Skip("no veritysetup (cryptsetup-bin)")
	}
	rootLine := regexp.MustCompile(`(?m)^Root hash:\s+([0-9a-f]+)`)
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			dir := t.TempDir()
			dataPath, hashPath := filepath.Join(dir, "data"), filepath.Join(dir, "hash")
			data := testData(s.blocks, s.dataBlock)
			if err := os.WriteFile(dataPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"format", "--format", fmt.Sprint(s.version), "--hash", s.algorithm,
				"--data-block-size", fmt.Sprint(s.dataBlock), "--hash-block-size", fmt.Sprint(s.hashBlk),
				"--salt", s.salt}
			if !s.superblock {
				args = append(args, "--no-superblock")
			}
			out, err := exec.Command(tool, append(args, dataPath, hashPath)...).CombinedOutput()
			if err != nil {
				t.Fatalf("veritysetup: %v\n%s", err, out)
			}
			m := rootLine.FindSubmatch(out)
			if m == nil {
				t.Fatalf("no root hash in\n%s", out)
			}
			hashDev, err := os.ReadFile(hashPath)
			if err != nil {
				t.Fatal(err)
			}

			table := s.table()
			table.RootDigest, _ = hex.DecodeString(string(m[1]))
			tree, root, err := table.Build(data)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(root, table.RootDigest) {
				t.Errorf("root %x, veritysetup's %s", root, m[1])
			}
			if err := table.Verify(data, hashDev); err != nil {
				t.Errorf("Verify of veritysetup's own files: %v", err)
			}
			start := int(table.HashStartBlock) * s.hashBlk
			if len(hashDev) != start+len(tree) {
				t.Errorf("veritysetup wrote %d bytes, the tree ends at %d", len(hashDev), start+len(tree))
			}
			t.Logf("root %s tree %x", m[1], sha256.Sum256(tree))
		})
	}
}

// TestVerifyRefuses: a byte changed in the data or in the tree, a short
// or long file, each refused with what's wrong; the bytes before the
// tree (a veritysetup superblock) aren't looked at.
func TestVerifyRefuses(t *testing.T) {
	s := shapes[0]
	table := s.table()
	data := testData(s.blocks, s.dataBlock)
	tree, root, err := table.Build(data)
	if err != nil {
		t.Fatal(err)
	}
	table.RootDigest = root
	hashDev := append(make([]byte, s.hashBlk), tree...)
	if err := table.Verify(data, hashDev); err != nil {
		t.Fatalf("its own files: %v", err)
	}

	flip := func(b []byte, i int) []byte {
		c := bytes.Clone(b)
		c[i] ^= 1
		return c
	}
	for _, c := range []struct {
		name       string
		data, hash []byte
		want       error
	}{
		{"a data byte", flip(data, len(data)/2), hashDev, ErrData},
		{"the last data byte", flip(data, len(data)-1), hashDev, ErrData},
		{"short data", data[:len(data)-s.dataBlock], hashDev, ErrData},
		{"long data", append(bytes.Clone(data), make([]byte, s.dataBlock)...), hashDev, ErrData},
		{"another image's data", testData(s.blocks+1, s.dataBlock)[s.dataBlock:], hashDev, ErrData},
		{"a byte of the top level", data, flip(hashDev, s.hashBlk+1), ErrHashTree},
		{"a byte of the last level", data, flip(hashDev, len(hashDev)-1), ErrHashTree},
		{"a short tree", data, hashDev[:len(hashDev)-1], ErrHashTree},
		{"no superblock where the table says one", data, tree, ErrHashTree},
	} {
		if err := table.Verify(c.data, c.hash); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if err := table.Verify(data, flip(hashDev, 0)); err != nil {
		t.Errorf("a byte of the superblock: %v", err)
	}
	if err := table.Verify(data, append(bytes.Clone(hashDev), 0xff)); err != nil {
		t.Errorf("bytes after the tree: %v", err)
	}
}

// TestFromCmdline reads the table of the command lines Janus builds
// (hack/dm-verity-cmdline.sh, image/uki/assemble.sh) and refuses what
// isn't one.
func TestFromCmdline(t *testing.T) {
	const root = "c5c1d1a3a8d8bcf1d3e0b3f6b5aa0b8bfa1c7d0e5b2f8a4c6d9e1f2a3b4c5d6e"
	janus := `console=tty0 console=ttyS0 dm-mod.create="vroot,,,ro,0 30720 verity 1 PARTLABEL=BOOT-B-DATA PARTLABEL=BOOT-B-HASH 4096 4096 3840 1 sha256 ` +
		root + ` 0011" dm-mod.waitfor=PARTLABEL=BOOT-B-DATA,PARTLABEL=BOOT-B-HASH root=/dev/dm-0 ro enforcing=1 janus.schematic=x`
	tab, err := FromCmdline(janus)
	if err != nil {
		t.Fatal(err)
	}
	if tab.Version != 1 || tab.DataBlockSize != 4096 || tab.HashBlockSize != 4096 || tab.DataBlocks != 3840 ||
		tab.HashStartBlock != 1 || tab.Algorithm != "sha256" || hex.EncodeToString(tab.RootDigest) != root ||
		!bytes.Equal(tab.Salt, []byte{0, 0x11}) {
		t.Errorf("%+v", tab)
	}

	// Another device first, the verity table second in its device, the
	// optional parameters, a salt of "-".
	other := `quiet dm-mod.create="lin,,,rw,0 8 linear /dev/vdb 0;vroot,,,ro,0 8 linear /dev/vdc 0,8 8 verity 1 /dev/vda /dev/vdd 4096 4096 2 0 sha256 ` +
		root + ` - 1 ignore_corruption" ro`
	if tab, err = FromCmdline(other); err != nil {
		t.Fatal(err)
	}
	if tab.HashStartBlock != 0 || tab.Salt != nil || tab.DataBlocks != 2 {
		t.Errorf("%+v", tab)
	}

	for _, bad := range []string{
		"root=/dev/vda ro",
		`dm-mod.create="vroot,,,ro,0 8 linear /dev/vda 0"`,
		`dm-mod.create="vroot,,,ro,0 8 verity 1 /dev/vda /dev/vdb 4096 4096 2 1 sha256 ` + root + ` 00`, // no closing quote
		`dm-mod.create="vroot,,,ro,0 8 verity 2 /dev/vda /dev/vdb 4096 4096 2 1 sha256 ` + root + ` 00"`,
		`dm-mod.create="vroot,,,ro,0 8 verity 1 /dev/vda /dev/vdb 4000 4096 2 1 sha256 ` + root + ` 00"`,
		`dm-mod.create="vroot,,,ro,0 8 verity 1 /dev/vda /dev/vdb 4096 4096 0 1 sha256 ` + root + ` 00"`,
		`dm-mod.create="vroot,,,ro,0 8 verity 1 /dev/vda /dev/vdb 4096 4096 2 1 md5 ` + root + ` 00"`,
		`dm-mod.create="vroot,,,ro,0 8 verity 1 /dev/vda /dev/vdb 4096 4096 2 1 sha256 ` + root[2:] + ` 00"`,
		`dm-mod.create="vroot,,,ro,0 8 verity 1 /dev/vda /dev/vdb 4096 4096 2 1 sha512 ` + root + ` 00"`,
		`dm-mod.create="vroot,,,ro,0 8 verity 1 /dev/vda /dev/vdb 4096 4096 2 1 sha256 ` + root + ` zz"`,
		`dm-mod.create="vroot,,,ro,0 8 verity 1 /dev/vda /dev/vdb 4096 4096 2 1 sha256 ` + root + `"`,
	} {
		if tab, err := FromCmdline(bad); err == nil {
			t.Errorf("%s: accepted as %+v", strings.TrimSpace(bad), tab)
		}
	}
}
