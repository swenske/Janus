# image/iso

`assemble.sh` builds a hybrid ISO/GPT installer/maintenance-mode
medium - bootable both via a real optical/El Torito UEFI path and as a
raw disk (dd'd to a USB stick, or attached directly as a virtio-blk
drive). `make iso-image` builds one; `make qemu-iso-boot-test` proves
it boots under real OVMF (see `hack/qemu-iso-boot-test.sh`).

Deliberately ephemeral (no A/B slots, no persistent STATE partition) -
this is a throwaway live environment, not a final installed system. It
boots straight to a running `janusd` (mTLS, prints its bootstrap CA/
admin creds to the console once, same as every other first boot in
this project), which an operator then drives remotely - e.g.
`janusctl lifecycle install <target-disk> <bundle-dir>` - to actually
install Janus onto a real, separate target disk.

`assemble.sh` takes an optional `[release-bundle-dir]` argument
(`image/release/assemble.sh`'s own output) that gets embedded directly
onto the medium's own ISO9660 partition, mounted read-only at
`/etc/janus/release` by `rootfs/init`'s `mountReleaseBundle` - so
`<bundle-dir>` above can just be `/etc/janus/release`, needing nothing
else reachable from the operator's own machine. `make
iso-image-with-bundle` builds this variant (the one actually meant for
distribution - `iso-image` itself stays bundle-free, for a faster
plain boot test); `make qemu-iso-install-test` proves a node booted
this way can genuinely install a real disk using only what's already
on the medium (see `hack/qemu-iso-install-test.sh`).

TODO(follow-up): PXE boot (serving this same UKI + cmdline over
TFTP/HTTP instead of via a physical medium) is still open - see
`../../docs/companion-site-builder-scope.md`.
