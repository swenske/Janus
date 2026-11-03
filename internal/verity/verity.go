// Package verity checks a root filesystem and its dm-verity hash tree
// against the mapping a kernel command line describes, before they are
// written to a slot.
//
// A release bundle's UKI carries, in its signed command line, the
// dm-verity table of the root it boots: the root hash, the salt, the
// block sizes. The kernel checks every block of rootfs.squashfs against
// that table at boot - but by then the ESP already points at the new
// slot, and a squashfs or a rootfs.verity the table doesn't describe
// (damaged on the way, mixed from two builds) leaves a kernel that can't
// mount its root: no init, no janusd, nothing left to revert to the
// other slot (security audit 2026-09-30, #16). Upgrade and Install
// therefore check the bundle against that table first: the hash tree
// against the root digest (CheckTree), then each block of the root
// filesystem against the tree before it's written (Copy) - streamed, so
// a slot's worth of data is never held in memory. Rollback checks a
// slot against its staged UKI the same way before switching to it.
//
// The tree is built the way the kernel's dm-verity target reads it
// (drivers/md/dm-verity-target.c, Documentation/admin-guide/
// device-mapper/verity.rst) and veritysetup writes it: one level per
// power of the hashes a block holds, stored top level first from
// hash_start_block on, each block's hashes zero-padded; format 1
// salts before the block, format 0 after. The tests compare it with
// veritysetup's own output.
package verity

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/bits"
	"strconv"
	"strings"
)

// Table is a dm-verity mapping's parameters, as a verity target's table
// line carries them.
type Table struct {
	// Version is the hash format: 1 (what veritysetup makes) or 0
	// (Chrome OS's original).
	Version        int
	DataBlockSize  int
	HashBlockSize  int
	DataBlocks     uint64
	HashStartBlock uint64
	Algorithm      string
	RootDigest     []byte
	// Salt is empty for a table whose salt is "-".
	Salt []byte
}

var (
	// ErrNoTable is a command line without a dm-mod.create= verity
	// mapping.
	ErrNoTable = errors.New("no dm-mod.create= verity mapping in the command line")
	// ErrData is data whose hash tree doesn't end in the table's root
	// hash, or isn't the size the table says.
	ErrData = errors.New("the data isn't what the table describes")
	// ErrHashTree is a hash tree other than the one the data makes.
	ErrHashTree = errors.New("the hash tree isn't the data's")
)

// FromCmdline returns the table of the verity mapping in a kernel
// command line's dm-mod.create= parameter (the kernel's
// Documentation/admin-guide/device-mapper/dm-init.rst):
//
//	dm-mod.create="<name>,<uuid>,<minor>,<flags>,<table>[,<table>...][;<device>...]"
//
// The first verity table found wins: Janus's command lines carry one.
func FromCmdline(cmdline string) (*Table, error) {
	const key = `dm-mod.create=`
	i := strings.Index(cmdline, key)
	if i < 0 {
		return nil, ErrNoTable
	}
	value := cmdline[i+len(key):]
	if rest, ok := strings.CutPrefix(value, `"`); ok {
		j := strings.IndexByte(rest, '"')
		if j < 0 {
			return nil, errors.New("dm-mod.create=: no closing quote")
		}
		value = rest[:j]
	} else if j := strings.IndexAny(value, " \t\n"); j >= 0 {
		value = value[:j]
	}
	for _, device := range strings.Split(value, ";") {
		// name, uuid, minor, flags, then the device's tables.
		parts := strings.Split(device, ",")
		if len(parts) < 5 {
			continue
		}
		for _, line := range parts[4:] {
			fields := strings.Fields(line)
			if len(fields) < 3 || fields[2] != "verity" {
				continue
			}
			t, err := ParseTable(fields[3:])
			if err != nil {
				return nil, fmt.Errorf("dm-mod.create=: %w", err)
			}
			return t, nil
		}
	}
	return nil, ErrNoTable
}

// ParseTable parses a verity target's arguments - what follows
// "<start> <sectors> verity" in a table line:
//
//	<version> <data_dev> <hash_dev> <data_block_size> <hash_block_size>
//	<num_data_blocks> <hash_start_block> <algorithm> <digest> <salt>
//	[<#opt_params> <opt_params>...]
//
// The optional parameters (error handling, FEC, a signature's key...)
// change nothing in the tree and are ignored.
func ParseTable(args []string) (*Table, error) {
	if len(args) < 10 {
		return nil, fmt.Errorf("verity table: %d arguments, want at least 10", len(args))
	}
	t := &Table{Algorithm: args[7]}
	var err error
	if t.Version, err = strconv.Atoi(args[0]); err != nil || (t.Version != 0 && t.Version != 1) {
		return nil, fmt.Errorf("verity table: unknown hash format %q", args[0])
	}
	if t.DataBlockSize, err = blockSize(args[3]); err != nil {
		return nil, fmt.Errorf("verity table: data block size: %w", err)
	}
	if t.HashBlockSize, err = blockSize(args[4]); err != nil {
		return nil, fmt.Errorf("verity table: hash block size: %w", err)
	}
	if t.DataBlocks, err = strconv.ParseUint(args[5], 10, 64); err != nil || t.DataBlocks == 0 {
		return nil, fmt.Errorf("verity table: number of data blocks %q", args[5])
	}
	if t.HashStartBlock, err = strconv.ParseUint(args[6], 10, 64); err != nil {
		return nil, fmt.Errorf("verity table: hash start block %q", args[6])
	}
	_, size, err := hasher(t.Algorithm)
	if err != nil {
		return nil, err
	}
	if t.RootDigest, err = hex.DecodeString(args[8]); err != nil || len(t.RootDigest) != size {
		return nil, fmt.Errorf("verity table: root digest %q isn't a %s digest", args[8], t.Algorithm)
	}
	if args[9] != "-" {
		if t.Salt, err = hex.DecodeString(args[9]); err != nil {
			return nil, fmt.Errorf("verity table: salt %q isn't hex", args[9])
		}
	}
	if t.HashBlockSize < 2*size {
		return nil, fmt.Errorf("verity table: a %d-byte hash block can't hold two %s digests", t.HashBlockSize, t.Algorithm)
	}
	return t, nil
}

// blockSize parses a block size: a power of two from 512 bytes to the
// largest the kernel takes (its page size; 64 KiB is the most any
// architecture has).
func blockSize(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 512 || n > 64<<10 || n&(n-1) != 0 {
		return 0, fmt.Errorf("%q isn't a power of two from 512 to 65536", s)
	}
	return n, nil
}

// hasher is the table's algorithm: the ones veritysetup offers that
// Janus would use.
func hasher(algorithm string) (func() hash.Hash, int, error) {
	switch algorithm {
	case "sha256":
		return sha256.New, sha256.Size, nil
	case "sha512":
		return sha512.New, sha512.Size, nil
	}
	return nil, 0, fmt.Errorf("verity table: unsupported hash algorithm %q", algorithm)
}

// DataBytes is the size of the data the table covers.
func (t *Table) DataBytes() uint64 {
	return t.DataBlocks * uint64(t.DataBlockSize)
}

// HashBytes is how much of the hash device the kernel reads: up to the
// end of the tree, superblock included when there is one.
func (t *Table) HashBytes() uint64 {
	g, err := t.geometry()
	if err != nil {
		return 0
	}
	return t.HashStartBlock*uint64(t.HashBlockSize) + g.treeBlocks()*uint64(t.HashBlockSize)
}

// geometry is how the table's tree is laid out.
type geometry struct {
	digest func([]byte) []byte
	// perBlock hashes per hash block, each entry bytes apart (format 1
	// pads each to the block's share, format 0 packs them).
	perBlock, entry int
	// counts: each level's blocks, from the data up (level 0 holds the
	// data blocks' digests); empty when the data is one block.
	counts []uint64
}

func (t *Table) geometry() (*geometry, error) {
	newHash, size, err := hasher(t.Algorithm)
	if err != nil {
		return nil, err
	}
	h := newHash()
	g := &geometry{digest: func(block []byte) []byte {
		h.Reset()
		if t.Version >= 1 {
			h.Write(t.Salt)
		}
		h.Write(block)
		if t.Version == 0 {
			h.Write(t.Salt)
		}
		return h.Sum(nil)
	}}
	// The kernel's hash_per_block_bits: as many hashes per block as the
	// largest power of two that fits.
	perBlockBits := bits.Len(uint(t.HashBlockSize/size)) - 1
	g.perBlock, g.entry = 1<<perBlockBits, size
	if t.Version >= 1 {
		g.entry = t.HashBlockSize >> perBlockBits
	}
	// As many levels as the kernel counts: until one block holds every
	// digest of the level below.
	for n := t.DataBlocks; n > 1; {
		n = (n + uint64(g.perBlock) - 1) / uint64(g.perBlock)
		g.counts = append(g.counts, n)
	}
	return g, nil
}

func (g *geometry) treeBlocks() uint64 {
	var n uint64
	for _, c := range g.counts {
		n += c
	}
	return n
}

// Tree is a hash device's tree, checked against its table's root digest.
type Tree struct {
	t *Table
	g *geometry
	// level0 holds the data blocks' digests; nil when the data is a
	// single block, whose digest is the root.
	level0 []byte
}

// CheckTree checks a hash device's tree against the table's root digest,
// the way the kernel does on its way to each data block: the top level's
// block must hash into the root digest, every other block into its entry
// in the level above. It needs no data: a bundle's hash tree is known
// good - or refused - before its root filesystem is read, and then each
// data block can be checked on its own (Copy). Nothing outside the tree
// is read (the kernel doesn't read a veritysetup superblock either).
func (t *Table) CheckTree(hashDev []byte) (*Tree, error) {
	g, err := t.geometry()
	if err != nil {
		return nil, err
	}
	hbs := uint64(t.HashBlockSize)
	start := t.HashStartBlock * hbs
	end := start + g.treeBlocks()*hbs
	if end < start || uint64(len(hashDev)) < end {
		return nil, fmt.Errorf("%w: %d bytes, the tree ends at byte %d", ErrHashTree, len(hashDev), end)
	}
	// Each level's offset: the device holds the top level first.
	offsets := make([]uint64, len(g.counts))
	off := start
	for i := len(g.counts) - 1; i >= 0; i-- {
		offsets[i] = off
		off += g.counts[i] * hbs
	}
	for i := len(g.counts) - 1; i >= 0; i-- {
		for b := uint64(0); b < g.counts[i]; b++ {
			block := hashDev[offsets[i]+b*hbs : offsets[i]+(b+1)*hbs]
			want := t.RootDigest
			if i < len(g.counts)-1 {
				want = entryOf(hashDev[offsets[i+1]:], g, t.HashBlockSize, b, len(t.RootDigest))
			}
			if !bytes.Equal(g.digest(block), want) {
				return nil, fmt.Errorf("%w: block %d of level %d (byte %d) doesn't hash to what %s", ErrHashTree, b, i, offsets[i]+b*hbs, above(i, len(g.counts)))
			}
		}
	}
	tree := &Tree{t: t, g: g}
	if len(g.counts) > 0 {
		tree.level0 = hashDev[offsets[0] : offsets[0]+g.counts[0]*hbs]
	}
	return tree, nil
}

// above names what a level's blocks hash into.
func above(level, levels int) string {
	if level == levels-1 {
		return "the table's root digest says"
	}
	return fmt.Sprintf("level %d holds for it", level+1)
}

// entryOf is the digest a level holds for block i of the level below.
func entryOf(level []byte, g *geometry, hashBlockSize int, i uint64, size int) []byte {
	off := (i/uint64(g.perBlock))*uint64(hashBlockSize) + (i%uint64(g.perBlock))*uint64(g.entry)
	return level[off : off+uint64(size)]
}

// leaf is the digest the tree holds for data block i.
func (tr *Tree) leaf(i uint64) []byte {
	if tr.level0 == nil {
		return tr.t.RootDigest
	}
	return entryOf(tr.level0, tr.g, tr.t.HashBlockSize, i, len(tr.t.RootDigest))
}

// Copy reads the table's data from src, checks each block against the
// tree before writing it to dst, and wants src to end there. It returns
// the bytes written: a block refused at byte n leaves n written. Errors
// about the data wrap ErrData; dst's are returned as they are.
func (t *Table) Copy(dst io.Writer, src io.Reader, tree *Tree) (int64, error) {
	if tree == nil || tree.t != t {
		return 0, errors.New("verity: Copy needs this table's checked tree (CheckTree)")
	}
	var written int64
	buf := make([]byte, t.DataBlockSize)
	for i := uint64(0); i < t.DataBlocks; i++ {
		if n, err := io.ReadFull(src, buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return written, fmt.Errorf("%w: it ends after %d bytes, the table covers %d", ErrData, i*uint64(t.DataBlockSize)+uint64(n), t.DataBytes())
			}
			return written, err
		}
		if !bytes.Equal(tree.g.digest(buf), tree.leaf(i)) {
			return written, fmt.Errorf("%w: block %d (byte %d) isn't the one the hash tree holds", ErrData, i, i*uint64(t.DataBlockSize))
		}
		n, err := dst.Write(buf)
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
	var one [1]byte
	if n, _ := io.ReadFull(src, one[:]); n > 0 {
		return written, fmt.Errorf("%w: it goes on after the %d bytes the table covers", ErrData, t.DataBytes())
	}
	return written, nil
}

// Verify checks that data is the table's data and that hashDev - the hash
// device's content, superblock included when there is one - holds its
// tree from HashStartBlock on.
func (t *Table) Verify(data, hashDev []byte) error {
	if uint64(len(data)) != t.DataBytes() {
		return fmt.Errorf("%w: %d bytes, the table covers %d (%d blocks of %d)", ErrData, len(data), t.DataBytes(), t.DataBlocks, t.DataBlockSize)
	}
	tree, err := t.CheckTree(hashDev)
	if err != nil {
		return err
	}
	_, err = t.Copy(io.Discard, bytes.NewReader(data), tree)
	return err
}

// Build makes data's hash tree as the hash device holds it from
// HashStartBlock on (top level first), and its root digest - what
// veritysetup format writes. data must hold the table's DataBlocks
// blocks; anything after them is left out.
func (t *Table) Build(data []byte) (tree, root []byte, err error) {
	g, err := t.geometry()
	if err != nil {
		return nil, nil, err
	}
	if uint64(len(data)) < t.DataBytes() {
		return nil, nil, fmt.Errorf("%w: %d bytes, fewer than the table's %d blocks of %d", ErrData, len(data), t.DataBlocks, t.DataBlockSize)
	}
	// Level 0's input: one digest per data block.
	digests := make([][]byte, t.DataBlocks)
	for i := range digests {
		off := uint64(i) * uint64(t.DataBlockSize)
		digests[i] = g.digest(data[off : off+uint64(t.DataBlockSize)])
	}
	var levels [][]byte
	for _, n := range g.counts {
		level := make([]byte, n*uint64(t.HashBlockSize))
		for i, d := range digests {
			copy(level[(i/g.perBlock)*t.HashBlockSize+(i%g.perBlock)*g.entry:], d)
		}
		levels = append(levels, level)
		next := make([][]byte, n)
		for i := range next {
			next[i] = g.digest(level[i*t.HashBlockSize : (i+1)*t.HashBlockSize])
		}
		digests = next
	}
	// The device holds the top level first.
	for i := len(levels) - 1; i >= 0; i-- {
		tree = append(tree, levels[i]...)
	}
	return tree, digests[0], nil
}
