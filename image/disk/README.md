# image/disk

`assemble.sh` builds a single, real GPT-partitioned disk image: two
independently bootable A/B slots (`BOOT-A-DATA`/`BOOT-A-HASH`,
`BOOT-B-DATA`/`BOOT-B-HASH` - each a squashfs data + dm-verity hash tree
pair) plus the persistent `STATE` partition, and `LOGS` after it (512
MiB, raw - the node formats it at its first boot, `internal/logvol`;
`LOGS_MB=0` leaves it out). Both slots get the same root filesystem;
`activate-slot.sh` stages both slots' kernel images on the ESP and makes
one of them the firmware's default.

| Partition | Size |
|---|---|
| ESP | 64 MiB |
| `BOOT-A-DATA`, `BOOT-B-DATA` | 128 MiB each |
| `BOOT-A-HASH`, `BOOT-B-HASH` | 4 MiB each |
| `STATE` | 128 MiB |
| `LOGS` | 512 MiB |

A disk keeps its partitions for good: disks made before 128 MiB slots
have 64 MiB ones, and `janusd` reads a slot's real size before an update
writes it. `JANUS_SLOT_MB=64` builds a disk of that older layout, for
tests. `LifecycleService.Install` lays out the same partitions in Go
(`internal/diskimage`), STATE taking the rest of the disk. The boot
chain and the A/B updates: [boot, disk layout and A/B
updates](../../docs/internals/boot.md).
