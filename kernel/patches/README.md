# Kernel patches

Janus's own changes to the kernel source, as `git format-patch` files
named `NNNN-short-description.patch`. `kernel/Dockerfile` applies them in
order to every track and architecture (`patch -p1 --forward --fuzz=0`),
right after checking the tarball: a patch that no longer applies exactly
- or that a release already carries - fails the build. That is the cue to
drop it (or rebase it), not to loosen the check.

Each patch is one sent upstream, in the form it was sent: its message
says what it fixes, how it was found and how it was tested. Drop it once
every pinned track (`versions.mk`) carries the fix.

| Patch | Fixes | Upstream |
|---|---|---|
| `0001-efi-libstub-Allocate-the-INITRD-table-in-ACPI-reclaim-memory.patch` | A kexec'd kernel clears 16 bytes of whatever reused the memory of the EFI stub's INITRD table - its own text under Debian's Secure Boot capable OVMF (7.2.10: `qemu-kexec-secboot-test` oopses in `tcp_parse_options`). The clearing came with 6e62d1c6f2c7 (6.19): the stable track has it, longterm 6.18 doesn't - the patch is harmless there. | [Sent to linux-efi](https://lore.kernel.org/all/20261012133712.618913-1-sebastien@wenske.fr/) (Ard Biesheuvel), 2026-10-12 |
