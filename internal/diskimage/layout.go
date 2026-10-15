// Package diskimage computes the partition byte layout
// LifecycleService.Install lays a fresh disk out with - the same
// convention image/disk/assemble.sh's own sgdisk invocation produces at
// build time (see internal/bootslot's own doc comment: 1:ESP,
// 2/3:BOOT-A-DATA/HASH, 4/5:BOOT-B-DATA/HASH, 6:STATE, 7:LOGS after
// STATE when the disk has room - internal/logvol), computed here in
// pure Go instead of shelling out, since sgdisk doesn't exist on the
// target OS any more than it does at runtime for Rollback/Upgrade.
//
// Deliberately just the arithmetic - actually writing the GPT table and
// filesystems (github.com/diskfs/go-diskfs) lives in
// internal/api/lifecycle.go's Install, its only real consumer; this
// package exists on its own because the layout math is worth testing
// without needing a real disk or root to do it, the same reasoning
// internal/bootslot's pure cmdline parsing was kept separate from the
// mount/reboot syscalls that act on what it parses.
package diskimage

import "fmt"

// Fixed partition sizes, matching image/disk/assemble.sh's own
// DATA_MB/HASH_MB/ESP_MB - kept identical so a disk Install produces is
// indistinguishable in shape from one assembled at build time, and so
// internal/bootslot's fixed partition-number convention keeps holding
// regardless of which one produced a given disk.
const (
	ESPMB  = 64
	DataMB = 64
	HashMB = 4

	// MinStateMB is the smallest STATE partition Install will accept -
	// below this, the target disk is rejected outright (Compute
	// returns an error) rather than producing a technically-bootable
	// but barely-usable node.
	MinStateMB = 16

	// MinLogsMB and MaxLogsMB bound the LOGS partition Install makes by
	// default: DefaultLogsPercent of the disk, on a disk of at least
	// MinLogsDiskMB - below that, none unless asked for.
	MinLogsMB          = 512
	MaxLogsMB          = 8 << 10
	DefaultLogsPercent = 5
	MinLogsDiskMB      = 4 << 10
	// SmallestLogsMB is the least a LOGS partition may be asked for
	// (internal/logvol needs its own MiB and two segments).
	SmallestLogsMB = 16
	// SectorSize matches every other disk-building script in this
	// project (image/disk/assemble.sh, rootfs/state-image.sh, ...) -
	// 512-byte logical sectors, the same default image/disk/
	// assemble.sh's sgdisk invocation assumes.
	SectorSize = 512
	// AlignSectors is 1MiB, matching sgdisk's own default alignment -
	// keeping it identical isn't load-bearing (nothing here shells out
	// to sgdisk to cross-check), but there's no reason to diverge from
	// a well-established default either.
	AlignSectors = 2048

	// GPTBackupSectors is the space the GPT spec reserves at the very
	// end of the disk for the backup header (1 sector) and backup
	// partition array (32 sectors, for the standard 128-entry table
	// github.com/diskfs/go-diskfs's gpt.Table writes) - 33 sectors
	// total. STATE must leave this untouched, not claim it as part of
	// its own "whatever's left" sizing - found by a real `sgdisk -p`
	// warning ("Secondary partition table overlaps the last partition
	// by 33 blocks") against a genuinely Install-produced disk, not by
	// inspection: every partition Compute ever laid out filled the
	// disk right to its last byte, silently corrupting the backup GPT
	// copy underneath STATE's own filesystem data on every real
	// Install this project has ever run.
	GPTBackupSectors = 33
)

// Layout is the sector-aligned start/end (inclusive, matching GPT's own
// convention and gpt.Partition's Start/End fields) of each of the six
// fixed partitions on a disk of a given total size.
type Layout struct {
	ESPStart, ESPEnd     uint64
	ADataStart, ADataEnd uint64
	AHashStart, AHashEnd uint64
	BDataStart, BDataEnd uint64
	BHashStart, BHashEnd uint64
	StateStart, StateEnd uint64
	// LogsStart and LogsEnd are 0 when the disk gets no LOGS partition.
	LogsStart, LogsEnd uint64
}

// HasLogs says whether the layout has a LOGS partition.
func (l *Layout) HasLogs() bool { return l.LogsEnd > l.LogsStart }

// DefaultLogsMB is the LOGS partition Install makes on a disk of
// diskBytes when not told otherwise: DefaultLogsPercent of it, between
// MinLogsMB and MaxLogsMB - none on a disk under MinLogsDiskMB.
func DefaultLogsMB(diskBytes int64) int64 {
	if diskBytes < mb(MinLogsDiskMB) {
		return 0
	}
	return min(max(diskBytes/mb(1)*DefaultLogsPercent/100, MinLogsMB), MaxLogsMB)
}

func mb(n int64) int64 { return n * 1024 * 1024 }

func alignUp(sector uint64) uint64 {
	return ((sector + AlignSectors - 1) / AlignSectors) * AlignSectors
}

// Compute lays the six partitions out sequentially, each sector-aligned
// to AlignSectors, giving STATE whatever's left after the ESP and the
// four fixed-size boot partitions - unlike image/disk/assemble.sh's own
// build-time STATE_MB constant (fixed, because that script only ever
// targets a deliberately small QEMU test disk), a real target disk's
// remaining space is put to use instead of wasted.
//
// Returns an error, rather than a Layout, if diskBytes can't fit the
// ESP, both boot slots, and at least MinStateMB of STATE - a target
// disk too small for a real install, not something to silently degrade
// into a broken one.
func Compute(diskBytes int64) (*Layout, error) {
	return ComputeWithLogs(diskBytes, 0)
}

// ComputeWithLogs is Compute with a LOGS partition of logsMB (0: none)
// at the end of the disk, STATE taking what's left between the boot
// partitions and it.
func ComputeWithLogs(diskBytes, logsMB int64) (*Layout, error) {
	if logsMB != 0 && logsMB < SmallestLogsMB {
		return nil, fmt.Errorf("a LOGS partition of %d MiB: %d MiB at least", logsMB, SmallestLogsMB)
	}
	cur := uint64(AlignSectors)
	next := func(sizeMB int64) (start, end uint64) {
		sectors := uint64(mb(sizeMB) / SectorSize)
		start = cur
		end = start + sectors - 1
		cur = alignUp(end + 1)
		return start, end
	}

	l := &Layout{}
	l.ESPStart, l.ESPEnd = next(ESPMB)
	l.ADataStart, l.ADataEnd = next(DataMB)
	l.AHashStart, l.AHashEnd = next(HashMB)
	l.BDataStart, l.BDataEnd = next(DataMB)
	l.BHashStart, l.BHashEnd = next(HashMB)

	usedBytes := int64(cur) * SectorSize
	reservedBytes := int64(GPTBackupSectors) * SectorSize
	remaining := diskBytes - usedBytes - reservedBytes
	if remaining < mb(MinStateMB) {
		minTotalMB := float64(usedBytes+reservedBytes+mb(MinStateMB)) / 1024 / 1024
		return nil, fmt.Errorf("disk is too small: only %d bytes would be left for STATE (need at least %dMiB) - disk must be at least %.1fMiB total", remaining, MinStateMB, minTotalMB)
	}
	l.StateStart = cur
	l.StateEnd = l.StateStart + uint64(remaining/SectorSize) - 1
	if logsMB > 0 {
		l.LogsEnd = l.StateEnd
		l.LogsStart = (l.LogsEnd + 1 - uint64(mb(logsMB)/SectorSize)) / AlignSectors * AlignSectors
		if l.LogsStart <= l.StateStart || (l.LogsStart-l.StateStart)*SectorSize < uint64(mb(MinStateMB)) {
			return nil, fmt.Errorf("disk is too small for a %d MiB LOGS partition: STATE would get less than %d MiB", logsMB, MinStateMB)
		}
		l.StateEnd = l.LogsStart - 1
	}

	return l, nil
}
