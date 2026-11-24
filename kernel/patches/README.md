# Kernel patches

Janus's own changes to the kernel source, as `git format-patch` files
named `NNNN-short-description.patch`. `kernel/Dockerfile` applies them in
order to every track and architecture (`patch -p1 --forward --fuzz=0`),
right after checking the tarball: a patch that no longer applies exactly
- or that a release already carries - fails the build. That is the cue to
drop it (or rebase it), not to loosen the check.

Each patch is one sent upstream, in the form it was sent - ours, or a
maintainer's commit taken from their tree before a release carries it:
its message says what it fixes and how. Drop it once every pinned track
(`versions.mk`) carries the fix.

| Patch | Fixes | Upstream |
|---|---|---|
| `0001-x86-efi-Disregard-the-INITRD-and-MEMATTR-config-tables-after-kexec.patch` | A kexec'd kernel read the INITRD config table the EFI stub installed at the first boot and cleared 16 bytes there, in memory the first kernel had reused - its own text under Debian's Secure Boot capable OVMF (7.2.10: `qemu-kexec-secboot-test` oopsed in `tcp_parse_options`). On x86, a kernel entered by kexec (a `SETUP_EFI` setup_data entry) now disregards the INITRD and Memory Attributes tables. The clearing came with 6e62d1c6f2c7 (6.19); longterm 6.18 only had the older stale read, which this covers too. | Ard Biesheuvel's [6863183e8b65](https://git.kernel.org/pub/scm/linux/kernel/git/efi/efi.git/commit/?h=urgent&id=6863183e8b65e833f8140e20d526282a725e040e) (efi.git `urgent`), the fix he asked us to test in place of [ours](https://lore.kernel.org/all/20261012133712.618913-1-sebastien@wenske.fr/) (ACPI reclaim memory moves across hibernation), 2026-11-24 - #374 |
