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
// therefore rebuild the hash tree here first and refuse a bundle whose
// files aren't the ones its UKI was built with.
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

// Verify checks that data is the table's data and that hashTree - the
// hash device's content, superblock included when there is one - holds
// the tree it makes from HashStartBlock on. Nothing outside the tree is
// read (the kernel doesn't read a veritysetup superblock either).
func (t *Table) Verify(data, hashTree []byte) error {
	if want := t.DataBlocks * uint64(t.DataBlockSize); uint64(len(data)) != want {
		return fmt.Errorf("%w: %d bytes, the table covers %d (%d blocks of %d)", ErrData, len(data), want, t.DataBlocks, t.DataBlockSize)
	}
	tree, root, err := t.Build(data)
	if err != nil {
		return err
	}
	if !bytes.Equal(root, t.RootDigest) {
		return fmt.Errorf("%w: its root hash is %x, the table's %x", ErrData, root, t.RootDigest)
	}
	start := t.HashStartBlock * uint64(t.HashBlockSize)
	end := start + uint64(len(tree))
	if end < start || uint64(len(hashTree)) < end {
		return fmt.Errorf("%w: %d bytes, the tree ends at byte %d", ErrHashTree, len(hashTree), end)
	}
	if !bytes.Equal(hashTree[start:end], tree) {
		return fmt.Errorf("%w: bytes %d-%d differ from the tree the data makes", ErrHashTree, start, end-1)
	}
	return nil
}

// Build makes data's hash tree as the hash device holds it from
// HashStartBlock on (top level first), and its root digest. data must
// hold the table's DataBlocks blocks; anything after them is left out.
func (t *Table) Build(data []byte) (tree, root []byte, err error) {
	newHash, size, err := hasher(t.Algorithm)
	if err != nil {
		return nil, nil, err
	}
	if uint64(len(data)) < t.DataBlocks*uint64(t.DataBlockSize) {
		return nil, nil, fmt.Errorf("%w: %d bytes, fewer than the table's %d blocks of %d", ErrData, len(data), t.DataBlocks, t.DataBlockSize)
	}
	h := newHash()
	digest := func(block []byte) []byte {
		h.Reset()
		if t.Version >= 1 {
			h.Write(t.Salt)
		}
		h.Write(block)
		if t.Version == 0 {
			h.Write(t.Salt)
		}
		return h.Sum(nil)
	}

	// The kernel's hash_per_block_bits: as many hashes per block as the
	// largest power of two that fits; format 1 pads each to the block's
	// share, format 0 packs them.
	perBlockBits := bits.Len(uint(t.HashBlockSize/size)) - 1
	perBlock := 1 << perBlockBits
	entry := size
	if t.Version >= 1 {
		entry = t.HashBlockSize >> perBlockBits
	}

	// Level 0's input: one digest per data block.
	digests := make([][]byte, t.DataBlocks)
	for i := range digests {
		off := uint64(i) * uint64(t.DataBlockSize)
		digests[i] = digest(data[off : off+uint64(t.DataBlockSize)])
	}

	// The levels from the data up, as many as the kernel counts: until
	// one block holds every digest of the level below.
	var levels [][]byte
	for len(digests) > 1 {
		n := (len(digests) + perBlock - 1) / perBlock
		level := make([]byte, n*t.HashBlockSize)
		for i, d := range digests {
			copy(level[(i/perBlock)*t.HashBlockSize+(i%perBlock)*entry:], d)
		}
		levels = append(levels, level)
		next := make([][]byte, n)
		for i := range next {
			next[i] = digest(level[i*t.HashBlockSize : (i+1)*t.HashBlockSize])
		}
		digests = next
	}
	// The device holds the top level first.
	for i := len(levels) - 1; i >= 0; i-- {
		tree = append(tree, levels[i]...)
	}
	return tree, digests[0], nil
}
