# Testing Janus on real Raspberry Pi 4 / Pi 5 hardware

This is the first real-hardware Janus image for the Single Board
Computer tranche. Everything up to this point (`make
qemu-raspi4-boot-test`, `qemu-raspi4-daemon-test`, `qemu-arm64-network-test`,
`qemu-arm64-uefi-boot-test`) has been verified under QEMU, which cannot
exercise the real Pi firmware boot chain at all (no VideoCore GPU
emulation) - **this document exists because the only way to close that
gap is a real boot, on real hardware, which this project's own
development and CI environments genuinely don't have.**

## What you're testing, and why the two boards differ

Both images use the same underlying approach: the Pi's own GPU-side
boot ROM/EEPROM reads a plain FAT32 partition looking for
`config.txt`, which points it at a real UEFI firmware (`RPI_EFI.fd`)
to load as the ARM CPU's boot stub. That UEFI firmware then does an
ordinary UEFI boot-device search and finds Janus's own Unified Kernel
Image (kernel + cmdline, one signed-or-not PE file) at the standard
fallback path.

- **Raspberry Pi 4**: uses [pftf/RPi4](https://github.com/pftf/RPi4)'s
  firmware - a mature, actively-maintained project (the official EDK2
  Raspberry Pi 4 UEFI platform). This project's own kernel source was
  checked directly (not assumed) to confirm real ACPI driver support
  exists for the SD card controller and the on-board Ethernet MAC.
  **Expected result: a real, working, networked appliance** - the
  same HAProxy-fronted node every QEMU test already proves, just on
  real hardware.
- **Raspberry Pi 5**: uses
  [NumberOneGit/rpi5-uefi](https://github.com/NumberOneGit/rpi5-uefi)'s
  `v0.1` "D0" release - the best available option, but genuinely less
  mature (a single early prerelease, not a long-established project).
  Worse, **mainline Linux has no SD card or Ethernet driver for the Pi
  5's "RP1" I/O chip at all yet** (confirmed by grepping this
  project's own pinned kernel's `MAINTAINERS` file - only clock/misc/
  pinctrl support exists). **Expected result: at best, a UEFI/kernel
  boot proof on the serial console - not a working appliance.** The
  kernel may start but will have no way to reach its own root
  filesystem or the network. This is still worth testing: confirming
  (or disconnecting) genuine UEFI+kernel boot on real Pi 5 hardware is
  real information this project has no other way to get.

## Getting the image

Either build it locally:

```sh
make pi4-sdcard-image   # -> build/rpi-uefi/pi4-disk.img
make pi5-sdcard-image   # -> build/rpi-uefi/pi5-disk.img
```

or download `janus-alpha-rpi4-<sha>` / `janus-alpha-rpi5-<sha>` from a
recent `Image Build` GitHub Actions run's artifacts.

Both are already `394MiB`, un-compressed, GPT-partitioned raw disk
images - no conversion needed, just written directly to an SD card
(8GB or larger, any speed class).

## Flashing

**This will erase the target SD card entirely.** Double-check the
device name before running any of these.

Using `dd` (Linux/macOS):

```sh
sudo dd if=build/rpi-uefi/pi4-disk.img of=/dev/sdX bs=4M status=progress conv=fsync
```

Or use [Raspberry Pi Imager](https://www.raspberrypi.com/software/) ->
"Use custom" -> select the `.img` file directly - works for both
boards, Imager doesn't need to know which one it is.

## Connecting a serial console (strongly recommended)

Neither board's own HDMI output will show anything useful here (no
display driver in this kernel at all, by design - this is a headless
appliance). A USB-to-TTL serial adapter on the 40-pin header's UART
pins is the only way to see what's actually happening:

- **GND** -> pin 6
- **RXD** (adapter) -> **GPIO14 / TXD** -> pin 8
- **TXD** (adapter) -> **GPIO15 / RXD** -> pin 10
- **115200 8n1**, no flow control (`screen /dev/ttyUSB0 115200`,
  `minicom`, PuTTY, etc.)

## What a successful Pi 4 boot looks like

1. A brief multicoloured screen on HDMI if connected (the GPU
   bootloader reading the SD card) - not required, just a sign the
   firmware partition is being read at all.
2. On the serial console: UEFI firmware messages, then the Linux
   kernel's own boot log, then `JANUS_INIT_BOOT_OK`.
3. `janusd` prints its bootstrapped PKI **once** - the CA certificate
   and an initial admin client certificate/key. **Copy these out of
   the console immediately** - there's no shell to retrieve them
   later (same as every other first Janus boot, see the project's own
   `CLAUDE.md`).
4. The board should pick up a DHCP address on its wired Ethernet port.
   From another machine on the same network:

   ```sh
   mkdir /tmp/janus-pi4-pki
   # paste the CA cert / admin cert / admin key printed on the console
   # into ca.crt / admin.crt / admin.key under that directory
   bin/janusctl -ca /tmp/janus-pi4-pki/ca.crt -cert /tmp/janus-pi4-pki/admin.crt \
     -key /tmp/janus-pi4-pki/admin.key -addr <pi4-ip>:9505 version
   ```

   A real response here (kernel version, active slot, etc.) is the
   final proof: full gRPC/mTLS control-plane access over a real
   network link, from real hardware.

## What to report back either way

This is genuinely unverified until your test - **please report exactly
what the serial console shows**, even (especially) if something fails
partway through:

- Nothing at all on the serial console -> likely a wiring/baud
  mismatch, or the EEPROM is too old to boot from an SD card at all
  (see below).
- Firmware messages but no `JANUS_INIT_BOOT_OK` -> the kernel itself
  is failing to boot under ACPI - the exact console output is what's
  needed to diagnose this.
- `JANUS_INIT_BOOT_OK` but no DHCP lease / no `janusd` PKI print ->
  Ethernet or the kernel's own boot sequence stalling after init - also
  very useful to know, especially for Pi 5.
- Everything above works but `janusctl version` fails -> a real,
  specific gap to chase (firewall, wrong IP, etc.) - not expected, but
  possible on a first real-network attempt.

## Known prerequisites / likely gotchas

- **Pi 4 EEPROM must be reasonably recent** for SD-card UEFI boot to
  work at all (pftf's own `Readme.md`, bundled on the firmware
  partition, documents this) - if the board never gets past the
  multicoloured screen, updating the EEPROM via a normal Raspberry Pi
  OS boot first (`sudo rpi-eeprom-update -a`) is the documented fix.
- **Power supply matters** - both boards are picky about
  under-powering, especially the Pi 5 (5V/5A officially recommended).
  A flaky boot before even reaching the firmware stage is more likely
  a power/cable problem than anything in this image.
- **Pi 5 EEPROM revision**: the firmware used here is explicitly the
  "D0"-stepping build - very recent Pi 5 boards may still not work
  even so (see this project's own `versions.mk` comment on
  `RPI5_UEFI_VERSION` for the full context); this is a known, accepted
  risk of using an early community firmware, not a Janus-specific bug.
