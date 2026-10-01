# image/iso

`assemble.sh` builds a hybrid ISO/GPT installer/maintenance-mode
medium, booted as a disk: dd'd to a USB stick, presented as a removable
disk by a BMC, or attached to a VM as a disk. Its UKI names its root
partitions by GPT label (`PARTLABEL=JANUS-ISO-DATA`/`-HASH`, set with
`sgdisk` after xorriso, which can't name the partitions it appends) - so
it boots whatever the disk is called (`/dev/sdb` as a USB stick), and
never confuses its own partitions with the `BOOT-A-*` ones of a disk it
has just installed. Not from an optical drive: the root is a partition
of the image, which a CD/DVD drive doesn't expose. `make iso-image`
builds one; `make qemu-iso-boot-test` proves it boots under real OVMF
(see `hack/qemu-iso-boot-test.sh`), and `make qemu-baremetal-test` boots
it from an emulated USB stick and installs a virtio-scsi disk.

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

The operator's procedure - from booting the ISO to approving the
installed node on the Controller - is `docs/provisioning-a-node.md`,
method 4. PXE: see `../pxe/README.md`.
