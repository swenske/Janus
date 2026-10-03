include versions.mk

MODULE  := github.com/swenske/Janus
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

BIN_DIR   := bin
BINARIES  := janusd janusctl
BUILD_DIR := build

GEN_DIR := gen

.PHONY: all build test vet lint proto clean kernel-menuconfig janusctl-deb janusctl-deb-test \
	shutdown-bin extensions-amd64 extensions-arm64 extension-qemu-guest-agent-amd64 extension-nftables-amd64 extension-nftables-arm64 extension-keepalived-amd64 extension-keepalived-arm64 extension-bird-amd64 extension-bird-arm64 schematic-catalog schematic-inputs site-frontend-build site-build qemu-metrics-test qemu-firewall-test qemu-vrrp-test qemu-bgp-test qemu-baremetal-test qemu-extensions-test pebble qemu-acme-test qemu-consul-test \
	kernel-build init initramfs qemu-boot-test haproxy-build \
	daemon-static initramfs-full qemu-network-test rootfs-build \
	qemu-verity-boot-test state-image qemu-state-persist-test \
	disk-image qemu-ab-boot-test uki-image qemu-uefi-boot-test \
	qemu-uefi-ab-boot-test qemu-lifecycle-rollback-test qemu-secureboot-test \
	qemu-lifecycle-upgrade-test qemu-lifecycle-upgrade-health-test \
	qemu-lifecycle-upgrade-url-test qemu-lifecycle-upgrade-relay-test qemu-lifecycle-upgrade-https-test qemu-packet-capture-test qemu-system-api-test qemu-network-config-test \
	lifecycle-install-test qemu-hardening-test selinux-policy qemu-selinux-test \
	proxmox-image qemu-system-info-test dashboard-frontend-build dashboard-build \
	qemu-dashboard-test dashboard-image controller-self-update-test controller-libvirt-test terraform-provider-build terraform-provider-dist terraform-provider-dist-test terraform-provider-test local-dev-image ca-certificates seed-controller-test \
	nocloud-seed-test kvm-image vmware-image iso-image qemu-iso-boot-test \
	qemu-iso-install-test iso-image-with-bundle qemu-pxe-fetch-test \
	rpi4-kernel-build rpi4-init rpi4-initramfs qemu-raspi4-boot-test \
	rpi4-daemon-static musl-toolchain-arm64 rpi4-haproxy-build rpi4-initramfs-full \
	qemu-raspi4-daemon-test qemu-arm64-network-test rpi4-rootfs-build \
	systemd-stub-arm64 rpi4-uki-image qemu-arm64-uefi-boot-test \
	pi4-firmware pi5-firmware pi4-sdcard-image pi5-sdcard-image \
	pi4-sdcard-image-test pi5-sdcard-image-test

all: build

build:
	mkdir -p $(BIN_DIR)
	for b in $(BINARIES); do \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$$b ./cmd/$$b ; \
	done

# janusctl's Debian packages, amd64 and arm64, in build/deb - what a
# release publishes on apt.sw-servers.net (see hack/janusctl-deb.sh).
janusctl-deb:
	for a in amd64 arm64; do ./hack/janusctl-deb.sh $(VERSION) $$a $(BUILD_DIR)/deb; done

# Installs the amd64 package in Debian and Ubuntu containers and runs it.
janusctl-deb-test: janusctl-deb
	./hack/janusctl-deb-test.sh $(VERSION) $(BUILD_DIR)/deb

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

# Regenerates gen/janus/v1alpha1 from api/proto/**.proto. Requires buf
# and the protoc-gen-go/protoc-gen-go-grpc plugins on PATH (`go install
# google.golang.org/protobuf/cmd/protoc-gen-go@latest` and
# `google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`, plus
# `github.com/bufbuild/buf/cmd/buf@latest`). CI's lint job checks the
# generated tree isn't stale (`make proto && git diff --exit-code -- gen`),
# same pattern as gotochanger's `make guide` drift check.
proto:
	buf lint
	buf generate

clean:
	rm -rf $(BIN_DIR)

# Opens an interactive `make menuconfig` inside a throwaway container built
# from kernel/Dockerfile's "config" stage, seeded from the currently
# committed kernel/configs/janus_defconfig, and writes the resulting
# defconfig back out so it can be reviewed with `git diff` and committed.
# This is the whole "module selection" workflow for Phase 0/1 - a Proxmox-
# hosted UI wrapping the same container is Phase 6, not required to get
# started.
kernel-menuconfig:
	docker build --target config -t janus-kernel-config \
		--build-arg KERNEL_VERSION=$(KERNEL_VERSION) \
		--build-arg KERNEL_SHA256=$(KERNEL_SHA256) kernel
	docker run --rm -it \
		-v "$(CURDIR)/kernel/configs:/out" \
		janus-kernel-config \
		sh -c 'make menuconfig && cp .config /out/janus_defconfig'
	@echo "Updated kernel/configs/janus_defconfig - review with 'git diff' and commit."

# Builds bzImage from kernel/configs/janus_defconfig via kernel/
# Dockerfile's "export" stage (needs Docker Buildx - `docker buildx
# version` to check) and pulls it out to build/bzImage.
kernel-build:
	mkdir -p $(BUILD_DIR)
	docker build --target export --build-arg KERNEL_VERSION=$(KERNEL_VERSION) \
		--build-arg KERNEL_SHA256=$(KERNEL_SHA256) \
		-o $(BUILD_DIR) kernel

# Builds the Phase 1 PID 1 (rootfs/init) as a static binary - CGO must stay
# disabled since the target has no libc.
init:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" \
		-o $(BUILD_DIR)/init ./rootfs/init

# /sbin/shutdown: signals PID 1 for a clean power-off or reboot (what the
# QEMU guest agent runs) - see rootfs/shutdown.
shutdown-bin:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" \
		-o $(BUILD_DIR)/shutdown ./rootfs/shutdown

# --- Optional extensions (extensions/<name>/, docs/image-factory.md) ---
# Each builds its file tree with its Dockerfile, then hack/extpack packs it
# with its manifest into build/extensions/extension-<name>-<arch>.tar.
EXT_DIR := $(BUILD_DIR)/extensions

extension-prometheus-node-exporter-%:
	rm -rf $(EXT_DIR)/tree-prometheus-node-exporter-$*
	docker build --target export --build-arg ARCH=$* \
		--build-arg NODE_EXPORTER_VERSION=$(NODE_EXPORTER_VERSION) \
		--build-arg NODE_EXPORTER_SHA256=$(NODE_EXPORTER_SHA256_$*) \
		-o $(EXT_DIR)/tree-prometheus-node-exporter-$* extensions/prometheus-node-exporter
	go run ./hack/extpack pack -name prometheus-node-exporter -arch $* -version $(NODE_EXPORTER_VERSION) \
		-tree $(EXT_DIR)/tree-prometheus-node-exporter-$* -out $(EXT_DIR)/extension-prometheus-node-exporter-$*.tar

extension-qemu-guest-agent-amd64:
	rm -rf $(EXT_DIR)/tree-qemu-guest-agent-amd64
	docker build --target export --build-arg QEMU_VERSION=$(QEMU_VERSION) \
		--build-arg QEMU_SHA256=$(QEMU_SHA256) \
		-o $(EXT_DIR)/tree-qemu-guest-agent-amd64 extensions/qemu-guest-agent
	go run ./hack/extpack pack -name qemu-guest-agent -arch amd64 -version $(QEMU_VERSION) \
		-tree $(EXT_DIR)/tree-qemu-guest-agent-amd64 -out $(EXT_DIR)/extension-qemu-guest-agent-amd64.tar

NFTABLES_BUILD_ARGS = --build-arg LIBMNL_VERSION=$(LIBMNL_VERSION) --build-arg LIBMNL_SHA256=$(LIBMNL_SHA256) \
	--build-arg LIBNFTNL_VERSION=$(LIBNFTNL_VERSION) --build-arg LIBNFTNL_SHA256=$(LIBNFTNL_SHA256) \
	--build-arg NFTABLES_VERSION=$(NFTABLES_VERSION) --build-arg NFTABLES_SHA256=$(NFTABLES_SHA256) \
	--build-arg JANSSON_VERSION=$(JANSSON_VERSION) --build-arg JANSSON_SHA256=$(JANSSON_SHA256)

extension-nftables-amd64:
	rm -rf $(EXT_DIR)/tree-nftables-amd64
	docker build --target export $(NFTABLES_BUILD_ARGS) -o $(EXT_DIR)/tree-nftables-amd64 extensions/nftables
	go run ./hack/extpack pack -name nftables -arch amd64 -version $(NFTABLES_VERSION) \
		-tree $(EXT_DIR)/tree-nftables-amd64 -out $(EXT_DIR)/extension-nftables-amd64.tar

extension-nftables-arm64: musl-toolchain-arm64
	rm -rf $(EXT_DIR)/tree-nftables-arm64
	docker build --target export-arm64 $(NFTABLES_BUILD_ARGS) \
		--build-context musltoolchain=$(BUILD_DIR)/musl-toolchain-arm64 \
		-o $(EXT_DIR)/tree-nftables-arm64 extensions/nftables
	go run ./hack/extpack pack -name nftables -arch arm64 -version $(NFTABLES_VERSION) \
		-tree $(EXT_DIR)/tree-nftables-arm64 -out $(EXT_DIR)/extension-nftables-arm64.tar

KEEPALIVED_BUILD_ARGS = --build-arg KEEPALIVED_VERSION=$(KEEPALIVED_VERSION) --build-arg KEEPALIVED_SHA256=$(KEEPALIVED_SHA256)

extension-keepalived-amd64:
	rm -rf $(EXT_DIR)/tree-keepalived-amd64
	docker build --target export $(KEEPALIVED_BUILD_ARGS) -o $(EXT_DIR)/tree-keepalived-amd64 extensions/keepalived
	go run ./hack/extpack pack -name keepalived -arch amd64 -version $(KEEPALIVED_VERSION) \
		-tree $(EXT_DIR)/tree-keepalived-amd64 -out $(EXT_DIR)/extension-keepalived-amd64.tar

extension-keepalived-arm64: musl-toolchain-arm64
	rm -rf $(EXT_DIR)/tree-keepalived-arm64
	docker build --target export-arm64 $(KEEPALIVED_BUILD_ARGS) \
		--build-context musltoolchain=$(BUILD_DIR)/musl-toolchain-arm64 \
		-o $(EXT_DIR)/tree-keepalived-arm64 extensions/keepalived
	go run ./hack/extpack pack -name keepalived -arch arm64 -version $(KEEPALIVED_VERSION) \
		-tree $(EXT_DIR)/tree-keepalived-arm64 -out $(EXT_DIR)/extension-keepalived-arm64.tar

BIRD_BUILD_ARGS = --build-arg BIRD_VERSION=$(BIRD_VERSION) --build-arg BIRD_SHA256=$(BIRD_SHA256)

extension-bird-amd64:
	rm -rf $(EXT_DIR)/tree-bird-amd64
	docker build --target export $(BIRD_BUILD_ARGS) -o $(EXT_DIR)/tree-bird-amd64 extensions/bird
	go run ./hack/extpack pack -name bird -arch amd64 -version $(BIRD_VERSION) \
		-tree $(EXT_DIR)/tree-bird-amd64 -out $(EXT_DIR)/extension-bird-amd64.tar

extension-bird-arm64: musl-toolchain-arm64
	rm -rf $(EXT_DIR)/tree-bird-arm64
	docker build --target export-arm64 $(BIRD_BUILD_ARGS) \
		--build-context musltoolchain=$(BUILD_DIR)/musl-toolchain-arm64 \
		-o $(EXT_DIR)/tree-bird-arm64 extensions/bird
	go run ./hack/extpack pack -name bird -arch arm64 -version $(BIRD_VERSION) \
		-tree $(EXT_DIR)/tree-bird-arm64 -out $(EXT_DIR)/extension-bird-arm64.tar

extension-consul-%:
	rm -rf $(EXT_DIR)/tree-consul-$*
	docker build --target export --build-arg ARCH=$* \
		--build-arg CONSUL_VERSION=$(CONSUL_VERSION) \
		--build-arg CONSUL_SHA256=$(CONSUL_SHA256_$*) \
		-o $(EXT_DIR)/tree-consul-$* extensions/consul
	go run ./hack/extpack pack -name consul -arch $* -version $(CONSUL_VERSION) \
		-tree $(EXT_DIR)/tree-consul-$* -out $(EXT_DIR)/extension-consul-$*.tar

# letsencrypt is this repository's own ACME client (cmd/janus-acme), built
# like janusd.
extension-letsencrypt-%:
	rm -rf $(EXT_DIR)/tree-letsencrypt-$*
	mkdir -p $(EXT_DIR)/tree-letsencrypt-$*/usr/local/sbin
	CGO_ENABLED=0 GOOS=linux GOARCH=$* go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(EXT_DIR)/tree-letsencrypt-$*/usr/local/sbin/janus-acme ./cmd/janus-acme
	go run ./hack/extpack pack -name letsencrypt -arch $* -version $(VERSION) \
		-tree $(EXT_DIR)/tree-letsencrypt-$* -out $(EXT_DIR)/extension-letsencrypt-$*.tar

extensions-amd64: extension-prometheus-node-exporter-amd64 extension-qemu-guest-agent-amd64 extension-nftables-amd64 extension-keepalived-amd64 extension-bird-amd64 extension-letsencrypt-amd64 extension-consul-amd64
extensions-arm64: extension-prometheus-node-exporter-arm64 extension-nftables-arm64 extension-keepalived-arm64 extension-bird-arm64 extension-letsencrypt-arm64 extension-consul-arm64

# The extensions a release can build a schematic with.
schematic-catalog:
	mkdir -p $(EXT_DIR)
	go run ./hack/extpack catalog -release $(VERSION) -out $(EXT_DIR)/schematic-catalog.json \
		prometheus-node-exporter=$(NODE_EXPORTER_VERSION) qemu-guest-agent=$(QEMU_VERSION) nftables=$(NFTABLES_VERSION) keepalived=$(KEEPALIVED_VERSION) bird=$(BIRD_VERSION) letsencrypt=$(VERSION) consul=$(CONSUL_VERSION)

# What a release publishes so custom schematics can be built from it
# without rebuilding anything (image/schematic/build.sh): per
# architecture, the kernel, the base rootfs tree and the extension packs,
# plus the catalog. Into build/inputs/.
schematic-inputs: kernel-build rpi4-kernel-build extensions-amd64 extensions-arm64 schematic-catalog
	rm -rf $(BUILD_DIR)/inputs && mkdir -p $(BUILD_DIR)/inputs
	JANUS_EXPORT_BASE=$(CURDIR)/$(BUILD_DIR)/inputs/rootfs-base-amd64.tar $(MAKE) rootfs-build
	JANUS_EXPORT_BASE=$(CURDIR)/$(BUILD_DIR)/inputs/rootfs-base-arm64.tar $(MAKE) rpi4-rootfs-build
	cp $(BUILD_DIR)/bzImage $(BUILD_DIR)/inputs/kernel-amd64
	cp $(BUILD_DIR)/rpi4/Image $(BUILD_DIR)/inputs/kernel-arm64
	cp $(EXT_DIR)/extension-*.tar $(EXT_DIR)/schematic-catalog.json $(BUILD_DIR)/inputs/

# SCHEMATIC=path/to/schematic.json builds the rootfs with that schematic's
# extensions (already built: make extensions-amd64) and puts its ID into
# every UKI's signed command line. Unset: the default schematic.
ifneq ($(SCHEMATIC),)
JANUS_SCHEMATIC := $(shell go run ./hack/extpack id -schematic $(SCHEMATIC))
export JANUS_SCHEMATIC
endif

# Packages build/init into build/initramfs.cpio.gz (see hack/build-initramfs.sh).
initramfs: init
	./hack/build-initramfs.sh $(BUILD_DIR)/init $(BUILD_DIR)/initramfs.cpio.gz

# The Phase 1 boot-proof: builds the kernel + initramfs and boots them
# under QEMU, checking for rootfs/init's success marker on the console
# (see hack/qemu-run.sh). Requires qemu-system-x86_64 on PATH.
qemu-boot-test: kernel-build initramfs
	./hack/qemu-run.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/initramfs.cpio.gz

# Single Board Computer tranche: cross-builds the aarch64/BCM2711 kernel
# Image + Raspberry Pi 4 device tree from kernel/configs/janus_rpi4_defconfig
# via kernel/Dockerfile's "export-arm64" stage, pulled out to build/rpi4/.
rpi4-kernel-build:
	mkdir -p $(BUILD_DIR)/rpi4
	docker build --target export-arm64 --build-arg KERNEL_VERSION=$(KERNEL_VERSION) \
		--build-arg KERNEL_SHA256=$(KERNEL_SHA256) \
		--build-arg ARCH=arm64 --build-arg CROSS_COMPILE=aarch64-linux-gnu- \
		--build-arg DEFCONFIG=janus_rpi4_defconfig --build-arg MAKE_TARGETS="Image dtbs" \
		-o $(BUILD_DIR)/rpi4 kernel

# Cross-builds rootfs/init for arm64 (same static Go PID 1, just a
# different GOARCH - no source changes needed).
rpi4-init:
	mkdir -p $(BUILD_DIR)/rpi4
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" \
		-o $(BUILD_DIR)/rpi4/init ./rootfs/init

rpi4-initramfs: rpi4-init
	./hack/build-initramfs.sh $(BUILD_DIR)/rpi4/init $(BUILD_DIR)/rpi4/initramfs.cpio.gz

# Single Board Computer tranche's own Phase-1-equivalent boot-proof:
# boots the aarch64 kernel + Pi 4 dtb + initramfs under QEMU's raspi4b
# machine, checking for rootfs/init's success marker on the console (see
# hack/qemu-raspi4-boot-test.sh). Requires qemu-system-aarch64 on PATH.
qemu-raspi4-boot-test: rpi4-kernel-build rpi4-initramfs
	./hack/qemu-raspi4-boot-test.sh $(BUILD_DIR)/rpi4/Image $(BUILD_DIR)/rpi4/bcm2711-rpi-4-b.dtb $(BUILD_DIR)/rpi4/initramfs.cpio.gz

# Cross-builds janusd for arm64 - CGO_ENABLED=0, same reasoning as
# daemon-static's own amd64 build (pure Go, no libc dependency either
# way).
rpi4-daemon-static:
	mkdir -p $(BUILD_DIR)/rpi4
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BUILD_DIR)/rpi4/janusd ./cmd/janusd

# Builds the real aarch64-linux-musl cross-toolchain pkgs/haproxy's own
# arm64 path needs - see pkgs/musl-toolchain/Dockerfile's own header for
# why this exists (QEMU-user-mode-emulated cross-platform Docker builds
# don't work on the self-hosted runner - it's an LXC container).
musl-toolchain-arm64:
	mkdir -p $(BUILD_DIR)/musl-toolchain-arm64
	docker build --target export --build-arg MUSL_CROSS_MAKE_REF=$(MUSL_CROSS_MAKE_REF) \
		-o $(BUILD_DIR)/musl-toolchain-arm64 pkgs/musl-toolchain

# Builds a fully static arm64 haproxy binary via a genuine cross-
# toolchain (pkgs/haproxy/Dockerfile's own "export-arm64" target, fed
# musl-toolchain-arm64's output as an external build context) - no
# QEMU/emulation anywhere, a normal amd64 build producing an arm64
# binary. zlib/AWS-LC are cross-compiled from source too, as on amd64.
rpi4-haproxy-build: musl-toolchain-arm64
	mkdir -p $(BUILD_DIR)/rpi4
	docker build --target export-arm64 \
		--build-arg HAPROXY_VERSION=$(HAPROXY_VERSION) \
		--build-arg HAPROXY_SHA256=$(HAPROXY_SHA256) \
		--build-arg ZLIB_VERSION=$(ZLIB_VERSION) --build-arg ZLIB_SHA256=$(ZLIB_SHA256) \
		--build-arg AWSLC_VERSION=$(AWSLC_VERSION) \
		--build-arg AWSLC_SHA256=$(AWSLC_SHA256) \
		--build-context musltoolchain=$(BUILD_DIR)/musl-toolchain-arm64 \
		-o $(BUILD_DIR)/rpi4 pkgs/haproxy

# The Single Board Computer tranche's own Phase-2-equivalent: init +
# janusd + haproxy, all arm64, in one initramfs - mirrors
# initramfs-full's exact amd64 shape (same embedded-file convention:
# src:dest pairs, see hack/build-initramfs.sh).
rpi4-initramfs-full: rpi4-init rpi4-daemon-static rpi4-haproxy-build
	./hack/build-initramfs.sh $(BUILD_DIR)/rpi4/init $(BUILD_DIR)/rpi4/initramfs-full.cpio.gz \
		$(BUILD_DIR)/rpi4/janusd:sbin/janusd \
		$(BUILD_DIR)/rpi4/haproxy:usr/local/sbin/haproxy \
		rootfs/base/etc/haproxy/haproxy.cfg:etc/haproxy/haproxy.cfg

# Single Board Computer tranche's own Phase-2-equivalent boot-proof:
# boots the same full init+janusd+haproxy stack qemu-network-test proves
# on amd64, but console-verified only (see hack/qemu-raspi4-daemon-
# test.sh's own header for why: QEMU's raspi4b machine emulates neither
# PCIe nor BCM GENET Ethernet, so there's no network path to curl over
# at all on this machine type - a real HTTP check has to wait for either
# real Pi hardware or accepting the generic aarch64 "virt" machine
# instead, a decision made in the next target below).
qemu-raspi4-daemon-test: rpi4-kernel-build rpi4-initramfs-full
	./hack/qemu-raspi4-daemon-test.sh $(BUILD_DIR)/rpi4/Image $(BUILD_DIR)/rpi4/bcm2711-rpi-4-b.dtb $(BUILD_DIR)/rpi4/initramfs-full.cpio.gz

# Single Board Computer tranche Phase-3-equivalent: the *same* aarch64
# kernel/initramfs, booted instead under QEMU's generic "virt" machine
# (real virtio-net-pci over a real PCIe root complex, no Pi-specific
# hardware at all) - real HTTP 200 verified, closing the network-proof
# gap qemu-raspi4-daemon-test can never close on raspi4b. See hack/
# qemu-arm64-network-test.sh's own header for the full reasoning.
qemu-arm64-network-test: rpi4-kernel-build rpi4-initramfs-full
	./hack/qemu-arm64-network-test.sh $(BUILD_DIR)/rpi4/Image $(BUILD_DIR)/rpi4/initramfs-full.cpio.gz

# Single Board Computer tranche, UEFI/UKI prep: the arm64 squashfs+dm-
# verity rootfs - the exact same rootfs/assemble.sh amd64 already uses,
# just fed arm64 binaries. selinux-policy/ca-certificates are reused
# as-is, not rebuilt per architecture - a compiled SELinux policy binary
# isn't CPU-architecture-specific (it's kernel-LSM-version-specific),
# and the CA bundle is plain PEM text either way. The policy is a
# harmless no-op here regardless: kernel/configs/janus_rpi4_defconfig
# has no CONFIG_SECURITY_SELINUX at all yet (Phase 4 cont'd's SELinux
# work is x86-only so far), so rootfs/init's loadSELinuxPolicy just logs
# and moves on, same tolerant pattern as a missing STATE drive.
rpi4-rootfs-build: rpi4-init rpi4-daemon-static rpi4-haproxy-build selinux-policy ca-certificates
	mkdir -p $(BUILD_DIR)/rpi4/rootfs
	JANUS_VERSION=$(VERSION) ./rootfs/assemble.sh $(BUILD_DIR)/rpi4/rootfs $(BUILD_DIR)/rpi4/init $(BUILD_DIR)/rpi4/janusd \
		$(BUILD_DIR)/rpi4/haproxy rootfs/base/etc/haproxy/haproxy.cfg \
		$(BUILD_DIR)/selinux/janus.policy $(BUILD_DIR)/ca-certificates/ca-certificates.crt

# Single Board Computer tranche, UEFI/UKI prep: exports systemd's
# aarch64 sd-stub (linuxaa64.efi.stub) - see systemd-stub-arm64/
# Dockerfile's own header for why this can't just be apt-installed on
# the build host directly.
systemd-stub-arm64:
	mkdir -p $(BUILD_DIR)/systemd-stub-arm64
	docker build --target export -o $(BUILD_DIR)/systemd-stub-arm64 systemd-stub-arm64

# Single Board Computer tranche: the aarch64 equivalent of uki-image -
# same ukify-based assembly, but UKIFY_STUB points at the fetched
# aarch64 sd-stub (the host's own ukify has no native aarch64 stub to
# auto-detect), UKI_CONSOLE=ttyAMA0 (PL011, not the x86 ttyS0),
# UKI_SELINUX_ENFORCING=0 (no SELinux support in this kernel config at
# all yet), and the ESP's boot file is BOOTAA64.EFI, not BOOTX64.EFI -
# a genuinely different UEFI-spec fallback name per architecture, not a
# build option (image/uki/esp-image.sh's own doc comment). root's
# data/hash devices are /dev/vdb+/dev/vdc, same reasoning as the amd64
# uki-image target: the ESP itself takes the vda slot once attached.
rpi4-uki-image: rpi4-kernel-build rpi4-rootfs-build systemd-stub-arm64
	UKIFY_STUB=$(BUILD_DIR)/systemd-stub-arm64/linuxaa64.efi.stub \
	UKI_CONSOLE=ttyAMA0 \
	UKI_SELINUX_ENFORCING=0 \
	./image/uki/assemble.sh $(BUILD_DIR)/rpi4/rootfs/janus.efi $(BUILD_DIR)/rpi4/Image \
		$(BUILD_DIR)/rpi4/rootfs /dev/vdb /dev/vdc
	./image/uki/esp-image.sh $(BUILD_DIR)/rpi4/rootfs/esp.img $(BUILD_DIR)/rpi4/rootfs/janus.efi 64 BOOTAA64.EFI

# Single Board Computer tranche: proves the arm64 UKI actually boots
# under *real* UEFI firmware (AAVMF/edk2-aarch64) on QEMU's generic
# "virt" machine - the aarch64 analog of qemu-uefi-boot-test, no
# -kernel/-append shortcut at all. See hack/qemu-arm64-uefi-boot-test.sh.
# Requires AAVMF (package: qemu-efi-aarch64).
qemu-arm64-uefi-boot-test: rpi4-uki-image
	./hack/qemu-arm64-uefi-boot-test.sh $(BUILD_DIR)/rpi4/rootfs $(BUILD_DIR)/rpi4/rootfs/esp.img

# Real Raspberry Pi 4/5 hardware follow-up: fetches each board's own
# real, pinned UEFI firmware release (versions.mk - see image/rpi-uefi/
# assemble.sh's own header for the real maturity gap between the two).
pi4-firmware:
	./image/rpi-uefi/fetch-firmware.sh \
		"https://github.com/pftf/RPi4/releases/download/$(PFTF_RPI4_UEFI_VERSION)/RPi4_UEFI_Firmware_$(PFTF_RPI4_UEFI_VERSION).zip" \
		$(PFTF_RPI4_UEFI_SHA256) $(BUILD_DIR)/rpi-uefi/pi4-firmware

pi5-firmware:
	./image/rpi-uefi/fetch-firmware.sh \
		"https://github.com/NumberOneGit/rpi5-uefi/releases/download/$(RPI5_UEFI_VERSION)/RPI5_D0.zip" \
		$(RPI5_UEFI_SHA256) $(BUILD_DIR)/rpi-uefi/pi5-firmware

# Real Raspberry Pi 4/5 hardware follow-up: the actual, flashable
# SD-card images - image/rpi-uefi/assemble.sh's own header has the full
# design (why one combined firmware+ESP partition, the Pi4-vs-Pi5
# maturity/driver-support gap). Shares rpi4-rootfs-build/rpi4-kernel-
# build/state-image with every other arm64 target - the rootfs/kernel
# content is identical for both boards, only the firmware partition and
# ESP boot filename differ. Requires sgdisk, mtools/dosfstools, ukify.
pi4-sdcard-image: rpi4-kernel-build rpi4-rootfs-build systemd-stub-arm64 state-image pi4-firmware
	mkdir -p $(BUILD_DIR)/rpi-uefi
	UKIFY_STUB=$(BUILD_DIR)/systemd-stub-arm64/linuxaa64.efi.stub \
	./image/rpi-uefi/assemble.sh $(BUILD_DIR)/rpi-uefi/pi4-disk.img \
		$(BUILD_DIR)/rpi-uefi/pi4-firmware $(BUILD_DIR)/rpi4/Image \
		$(BUILD_DIR)/rpi4/rootfs $(BUILD_DIR)/rootfs/state.img

pi5-sdcard-image: rpi4-kernel-build rpi4-rootfs-build systemd-stub-arm64 state-image pi5-firmware
	mkdir -p $(BUILD_DIR)/rpi-uefi
	UKIFY_STUB=$(BUILD_DIR)/systemd-stub-arm64/linuxaa64.efi.stub \
	./image/rpi-uefi/assemble.sh $(BUILD_DIR)/rpi-uefi/pi5-disk.img \
		$(BUILD_DIR)/rpi-uefi/pi5-firmware $(BUILD_DIR)/rpi4/Image \
		$(BUILD_DIR)/rpi4/rootfs $(BUILD_DIR)/rootfs/state.img

# Real Raspberry Pi 4/5 hardware follow-up: structural verification
# only - no real Pi4/Pi5 hardware exists in this environment (or in
# CI) to actually boot-test either image, see hack/
# rpi-sdcard-image-test.sh's own header for exactly what is and isn't
# proven this way, and image/rpi-uefi/assemble.sh's own header for why
# a real hardware test is the only way to close that gap.
pi4-sdcard-image-test: pi4-sdcard-image
	./hack/rpi-sdcard-image-test.sh $(BUILD_DIR)/rpi-uefi/pi4-disk.img RPI_EFI.fd

pi5-sdcard-image-test: pi5-sdcard-image
	./hack/rpi-sdcard-image-test.sh $(BUILD_DIR)/rpi-uefi/pi5-disk.img RPI_EFI.fd

# Builds a fully static (musl, via Alpine's own toolchain - see pkgs/
# haproxy/Dockerfile) haproxy binary with AWS-LC and pulls it out to
# build/haproxy. No PCRE2 (Alpine ships no static pcre2-posix lib;
# HAProxy's built-in regex engine covers Phase 2's needs).
haproxy-build:
	mkdir -p $(BUILD_DIR)
	docker build --target export --build-arg HAPROXY_VERSION=$(HAPROXY_VERSION) \
		--build-arg HAPROXY_SHA256=$(HAPROXY_SHA256) \
		--build-arg ZLIB_VERSION=$(ZLIB_VERSION) --build-arg ZLIB_SHA256=$(ZLIB_SHA256) \
		--build-arg AWSLC_VERSION=$(AWSLC_VERSION) \
		--build-arg AWSLC_SHA256=$(AWSLC_SHA256) \
		-o $(BUILD_DIR) pkgs/haproxy

# Builds janusd as a static binary (CGO_ENABLED=0, same reasoning as
# `init`) for packaging into the initramfs - separate from `build`'s
# bin/janusd, which doesn't force CGO off since it only needs to run
# on the build host there, not on the target kernel.
daemon-static:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(BUILD_DIR)/janusd ./cmd/janusd

# The Phase 2 network-integration rootfs: init + janusd + the real
# static haproxy binary + the bootstrap config (see rootfs/base/etc/
# haproxy/haproxy.cfg). rootfs/init detects janusd's presence and
# supervises it instead of powering off - see rootfs/init/main.go.
initramfs-full: init daemon-static haproxy-build
	./hack/build-initramfs.sh $(BUILD_DIR)/init $(BUILD_DIR)/initramfs-full.cpio.gz \
		$(BUILD_DIR)/janusd:sbin/janusd \
		$(BUILD_DIR)/haproxy:usr/local/sbin/haproxy \
		rootfs/base/etc/haproxy/haproxy.cfg:etc/haproxy/haproxy.cfg

# The Phase 2 network-integration boot test: boots the kernel + full
# initramfs under QEMU with virtio-net + DHCP, and polls a forwarded host
# port until HAProxy - running *inside* the VM - answers over real TCP/IP
# (see hack/qemu-network-test.sh). Requires qemu-system-x86_64 on PATH.
qemu-network-test: kernel-build initramfs-full
	./hack/qemu-network-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/initramfs-full.cpio.gz

# Phase 4: proves rootfs/init/main.go's hardenSysctls actually applies
# every kernel-hardening sysctl it claims to on a real boot (not just
# that the Go code runs without panicking, and not just that the
# matching kernel/configs/janus_defconfig options compile in - see
# hack/qemu-hardening-test.sh's own comment for the real gap that
# distinction caught: CONFIG_SYN_COOKIES missing, silently failing only
# the tcp_syncookies write while every other sysctl and the boot itself
# looked completely fine).
qemu-hardening-test: kernel-build initramfs-full
	./hack/qemu-hardening-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/initramfs-full.cpio.gz

# Phase 3: builds a squashfs image of the real rootfs (init + janusd +
# haproxy + bootstrap config, same content as initramfs-full but as a
# proper filesystem image instead of a cpio archive) and its dm-verity
# hash tree (see rootfs/assemble.sh). Requires mksquashfs (squashfs-tools)
# and veritysetup (cryptsetup-bin) on PATH - neither needs root. This is
# the build-side half of Phase 3's immutability story, verified by
# mounting + `veritysetup verify` - see qemu-verity-boot-test for the
# kernel actually booting from it.
# Phase 4 cont'd: compiles selinux/classes.conf + selinux/policy.conf
# (see both files' own headers) into the one binary policy
# rootfs/init loads at boot - checkpolicy only, no libselinux/semodule/
# policy store on the target. Requires Docker (same pattern as every
# other pkgs/-style component here).
selinux-policy:
	mkdir -p $(BUILD_DIR)/selinux
	docker build --target export -o $(BUILD_DIR)/selinux selinux

# internal/nocloud's seedfrom "mode B" (verify against the system trust
# store) needs a real CA bundle to exist on the rootfs at all - see
# ca-certificates/Dockerfile's own comment for why nothing else in this
# project has needed one until now.
ca-certificates:
	mkdir -p $(BUILD_DIR)/ca-certificates
	docker build --target export -o $(BUILD_DIR)/ca-certificates ca-certificates

rootfs-build: init shutdown-bin daemon-static haproxy-build selinux-policy ca-certificates
	mkdir -p $(BUILD_DIR)/rootfs
	layers="$$($(if $(SCHEMATIC),go run ./hack/extpack layers -schematic $(SCHEMATIC) -arch amd64 -dir $(EXT_DIR),true))" && \
	JANUS_SHUTDOWN_BIN=$(BUILD_DIR)/shutdown JANUS_VERSION=$(VERSION) JANUS_EXTENSIONS="$$layers" \
	./rootfs/assemble.sh $(BUILD_DIR)/rootfs $(BUILD_DIR)/init $(BUILD_DIR)/janusd \
		$(BUILD_DIR)/haproxy rootfs/base/etc/haproxy/haproxy.cfg \
		$(BUILD_DIR)/selinux/janus.policy $(BUILD_DIR)/ca-certificates/ca-certificates.crt

# Phase 3 cont'd: boots the kernel directly from rootfs-build's
# squashfs+dm-verity image via the "dm-mod.create=" cmdline parameter
# (CONFIG_DM_INIT) - no initramfs, no userspace verity setup at all. Two
# virtio-blk drives (squashfs data + verity hash tree), the kernel
# assembles and verifies /dev/dm-0 itself before mounting it as root and
# running /sbin/init straight out of the verified image. Also re-runs
# the boot against a corrupted copy of the image and checks the kernel
# refuses to mount it - see hack/qemu-verity-boot-test.sh for exactly
# why (superblock corruption, not a random offset) and how the dm-verity
# table's fields are derived from rootfs.verity.info.
qemu-verity-boot-test: kernel-build rootfs-build
	./hack/qemu-verity-boot-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs

# Phase 4 cont'd: proves selinux/classes.conf + selinux/policy.conf (the
# real, hand-written minimal policy - see that file's own header) loads
# at boot and mediates a full, working HTTP-200 boot with zero AVC
# denials, both permissively (the shipped default) and with a real
# enforcing=1 boot - the actual proof the rule set is complete, not
# merely quiet. See hack/qemu-selinux-test.sh.
qemu-selinux-test: kernel-build rootfs-build
	./hack/qemu-selinux-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs

# Phase 3 cont'd: builds a blank, pre-formatted ext4 image for the
# persistent STATE partition (/etc/janus/pki, /etc/haproxy, and -
# until real OCI/HTTPS image distribution exists - a staging area for
# LifecycleService.Upgrade's release bundles too, see image/disk/
# assemble.sh's own STATE_MB comment for why) - formatted here, at
# build time, not on the target (see rootfs/state-image.sh for why).
# Requires mkfs.ext4 (e2fsprogs) - doesn't need root. Must match
# image/disk/assemble.sh's own STATE_MB, or <state-image> won't
# actually fill the partition it gets dd'd into.
STATE_IMAGE_MB := 128
state-image:
	mkdir -p $(BUILD_DIR)/rootfs
	./rootfs/state-image.sh $(BUILD_DIR)/rootfs/state.img $(STATE_IMAGE_MB)

# Phase 3 cont'd: proves the STATE partition actually persists across a
# reboot, not just that it can be mounted - boots the same dm-verity
# image twice against the *same* state.img (a third, writable virtio-blk
# drive, unlike the two read-only root drives), and checks janusd's
# own "first boot" log line appears on the first boot and does NOT
# reappear on the second - see hack/qemu-state-persist-test.sh.
qemu-state-persist-test: kernel-build rootfs-build state-image
	./hack/qemu-state-persist-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs $(BUILD_DIR)/rootfs/state.img

# Phase 3 cont'd: assembles a single, real GPT-partitioned disk image -
# the actual, complete shape a deployed node would have: an ESP (with
# the active slot's Unified Kernel Image at \EFI\BOOT\BOOTX64.EFI),
# two independently bootable A/B slots (BOOT-A-DATA/HASH,
# BOOT-B-DATA/HASH - both slots get the same rootfs content for now,
# there's no LifecycleService.Upgrade yet to install something
# different into the inactive one), and STATE. As opposed to
# qemu-verity-boot-test/qemu-state-persist-test's separate-virtio-blk-
# drives harness (which keeps working, and still covers what it always
# covered). Requires sgdisk (gdisk), ukify (systemd-ukify),
# mtools/dosfstools - doesn't need root. See image/disk/assemble.sh and
# image/disk/activate-slot.sh (switches which slot's UKI is on the ESP,
# in place, without touching STATE or either slot's content - the
# groundwork for a real LifecycleService.Upgrade/Rollback).
disk-image: kernel-build rootfs-build state-image
	./image/disk/assemble.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR)/bzImage \
		$(BUILD_DIR)/rootfs $(BUILD_DIR)/rootfs/state.img A

# First real, deployable artifact: disk-image's raw GPT disk converted
# to qcow2 - Proxmox's own preferred import/storage format. Requires
# qemu-img on top of disk-image's own tools. See
# image/kvm-proxmox/assemble.sh.
proxmox-image: kernel-build rootfs-build state-image
	./image/kvm-proxmox/assemble.sh $(BUILD_DIR)/janus.qcow2 $(BUILD_DIR)/bzImage \
		$(BUILD_DIR)/rootfs $(BUILD_DIR)/rootfs/state.img A

# Same conversion as proxmox-image, but as its own artifact for a
# generic libvirt/KVM deployment (virt-install), not Proxmox's `qm`
# import flow. See image/kvm/assemble.sh and image/kvm/README.md.
kvm-image: kernel-build rootfs-build state-image
	./image/kvm/assemble.sh $(BUILD_DIR)/janus-kvm.qcow2 $(BUILD_DIR)/bzImage \
		$(BUILD_DIR)/rootfs $(BUILD_DIR)/rootfs/state.img A

# disk-image's raw GPT disk converted to a streamOptimized VMDK -
# VMware/ESXi's own native disk format. See image/vmware/assemble.sh
# and image/vmware/README.md.
vmware-image: kernel-build rootfs-build state-image
	./image/vmware/assemble.sh $(BUILD_DIR)/janus.vmdk $(BUILD_DIR)/bzImage \
		$(BUILD_DIR)/rootfs $(BUILD_DIR)/rootfs/state.img A

# Bare-metal Machine tranche: hybrid ISO/GPT installer/maintenance-mode
# medium (no A/B, no STATE - ephemeral), bootable both via a real
# El Torito EFI path and as a raw disk (dd'd to USB). Requires xorriso
# on top of uki-image's own tools (ukify, mtools/dosfstools).
iso-image: kernel-build rootfs-build
	./image/iso/assemble.sh $(BUILD_DIR)/janus.iso $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs

# Proves iso-image's hybrid ISO boots under real OVMF as a raw GPT disk
# - see hack/qemu-iso-boot-test.sh. Requires OVMF (package: ovmf).
qemu-iso-boot-test: iso-image
	./hack/qemu-iso-boot-test.sh $(BUILD_DIR)/janus.iso

# Same ISO, this time with a real release bundle embedded (image/
# release/assemble.sh's own output) so it's actually useful for a real
# LifecycleService.Install call with nothing else reachable from the
# host - see image/iso/assemble.sh's own [release-bundle-dir] arg and
# hack/qemu-iso-install-test.sh. This is the artifact actually meant
# for distribution, not plain iso-image above (which is what the boot
# test itself uses, kept bundle-free/faster to build).
#
# Its $(BUILD_DIR)/release is also the bundle a GitHub Release publishes
# (image-build.yml), so it's the one that has to be signed: pass
# SIGNING_KEY=<key.pem> SIGNING_CERT=<cert.pem> to sign both UKIs (left
# unsigned when unset). Signing a separately built bundle instead
# doesn't work - rootfs-build reruns for every target and mksquashfs
# isn't byte-reproducible, so another bundle's UKIs carry a different
# dm-verity root hash than this rootfs.squashfs.
SIGNING_KEY ?=
SIGNING_CERT ?=
iso-image-with-bundle: kernel-build rootfs-build
	./image/release/assemble.sh $(BUILD_DIR)/release $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs "$(SIGNING_KEY)" "$(SIGNING_CERT)"
	./image/iso/assemble.sh $(BUILD_DIR)/janus.iso $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs $(BUILD_DIR)/release

# Bare-metal Machine tranche cont'd: proves a node booted from the ISO
# with an embedded release bundle (image/iso/assemble.sh's own optional
# [release-bundle-dir] arg) can call LifecycleService.Install using
# only what's already on the medium (/etc/janus/release) - no bundle
# reachable from the host at all. Builds its own bundle+ISO internally,
# see hack/qemu-iso-install-test.sh. Requires janusctl built (`build`)
# and OVMF.
qemu-iso-install-test: build kernel-build rootfs-build
	./hack/qemu-iso-install-test.sh $(BUILD_DIR)/rootfs $(BUILD_DIR)/bzImage $(BIN_DIR)/janusctl

# Bare-metal Machine tranche cont'd: proves the network-delivery half
# of PXE/iPXE boot - see image/pxe/README.md for the full story
# (native PXE/HTTP Boot vs. the iPXE fallback this test actually
# exercises, and its own documented "known limitation"). Requires the
# `ipxe` Debian package and OVMF.
qemu-pxe-fetch-test: kernel-build rootfs-build
	./hack/qemu-pxe-fetch-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs

# Phase 3 cont'd: proves both A/B slots of disk-image's single GPT disk
# are actually, independently bootable via QEMU's own -kernel/-append
# (not through the ESP/UKI - see qemu-uefi-ab-boot-test for that) - not
# just that the partition table looks right. Boots the SAME disk image
# twice, once with dm-mod.create= pointed at BOOT-A-DATA/BOOT-A-HASH
# (partitions 2/3), once at BOOT-B-DATA/BOOT-B-HASH (partitions 4/5);
# both must serve real HTTP. See hack/qemu-ab-boot-test.sh.
qemu-ab-boot-test: kernel-build disk-image
	./hack/qemu-ab-boot-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs

# Phase 3 cont'd: proves the *whole* real, single-disk, UEFI-bootable
# shape end to end - real OVMF firmware, one virtio-blk drive, no
# -kernel/-append at all - and that image/disk/activate-slot.sh's
# in-place ESP swap actually works: boot slot A (fresh CA onto STATE),
# switch the ESP to slot B in place, boot again, and confirm slot B is
# now what's live *and* that STATE (the CA from the slot A boot)
# survived the switch untouched. See hack/qemu-uefi-ab-boot-test.sh.
qemu-uefi-ab-boot-test: disk-image
	./hack/qemu-uefi-ab-boot-test.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs

# Phase 3 cont'd: proves LifecycleService.Rollback (internal/api/
# lifecycle.go) works over a real gRPC call against a running node -
# boots slot A, calls `janusctl lifecycle rollback` over real mTLS
# (credentials extracted straight from disk.img's STATE partition via
# debugfs, not the console - see hack/qemu-lifecycle-rollback-test.sh
# for why), and watches the guest genuinely reboot itself (no
# -no-reboot this time) into slot B with STATE intact. Requires
# janusctl built (see `build`).
qemu-lifecycle-rollback-test: build disk-image
	./hack/qemu-lifecycle-rollback-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/janusctl

# Dashboard prep, tranche 1: proves the SystemService RPCs the dashboard's
# single-node view needs (Memory/CPUInfo/LoadAvg/DiskStats, plus
# VersionResponse's active_slot/kernel_version/go_version) return real,
# sane values from a real boot - see internal/api/system_stats.go and
# hack/qemu-system-info-test.sh.
qemu-system-info-test: build disk-image
	./hack/qemu-system-info-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/janusctl

# Dashboard prep, tranche 2: builds dashboardd (dashboard/backend) -
# lives outside cmd/ (see the rebranding/dashboard/client-native plan:
# a separate top-level dashboard/ tree, not another control-plane
# binary) so it isn't part of the $(BINARIES) loop above.
# Builds dashboard/frontend's React SPA straight into dashboard/backend/
# static (vite.config.js's own outDir) - go:embed needs it there at `go
# build` time. Requires npm. The build output is committed to the repo
# (like gen/janus/v1alpha1) so a plain `go build ./...` never needs
# a Node.js toolchain just to compile - this target is for actually
# picking up frontend source changes.
dashboard-frontend-build:
	cd dashboard/frontend && npm ci && npm run build

dashboard-build: dashboard-frontend-build
	mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/dashboardd ./dashboard/backend
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/janus-controller-updater ./dashboard/updater

# The companion site, janus.sw-servers.net (site/): frontend built into
# site/backend/static (committed, like the Controller's), then the Go
# binary. Deployed by .github/workflows/site-deploy.yml.
site-frontend-build:
	cd site/frontend && npm ci && npm run build

site-build: site-frontend-build
	mkdir -p $(BIN_DIR)
	go build -trimpath -o $(BIN_DIR)/janus-site ./site/backend

# Dashboard prep, tranche 2: proves the dashboard backend's whole
# add-node/list/per-node-mTLS-relay/delete/restart-persistence flow
# works against a real running node, not a mock - see
# dashboard/backend and hack/qemu-dashboard-test.sh.
qemu-dashboard-test: dashboard-build disk-image
	./hack/qemu-dashboard-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/dashboardd

# Dashboard prep, tranche 5: builds dashboard/Dockerfile's runnable
# image locally (see that file's own comment for why it needs no
# frontend-build stage - dashboard/backend/static is already
# committed). No `docker push` here on purpose - publishing is
# deliberately deferred until the project rename is decided, see
# dashboard/README.md. Run from the repo root (not dashboard/) since
# the build context needs go.mod/gen/internal alongside dashboard/
# itself.
# DASHBOARD_IMAGE: the tag (controller-self-update-test builds its own).
DASHBOARD_IMAGE ?= janus-controller
dashboard-image:
	docker build -f dashboard/Dockerfile --build-arg VERSION=$(VERSION) \
		--build-arg DOCKER_COMPOSE_VERSION=$(DOCKER_COMPOSE_VERSION) \
		--build-arg DOCKER_COMPOSE_SHA256_AMD64=$(DOCKER_COMPOSE_SHA256_AMD64) \
		--build-arg DOCKER_COMPOSE_SHA256_ARM64=$(DOCKER_COMPOSE_SHA256_ARM64) \
		-t $(DASHBOARD_IMAGE) .

# The Controller updating itself through janus-controller-updater, with
# real Docker Compose set up as dashboard/README.md documents it: an
# update that works, one that rolls back (needs Docker, python3).
controller-self-update-test:
	./hack/controller-self-update-test.sh

# The Janus Terraform provider (terraform-provider-janus/, its own Go
# module) - docs/terraform.md.
terraform-provider-build:
	mkdir -p $(BIN_DIR)
	cd terraform-provider-janus && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(CURDIR)/$(BIN_DIR)/terraform-provider-janus .

# The provider's release archives (linux/darwin, amd64/arm64) and their
# checksums - attached to every GitHub Release (docs/terraform.md).
terraform-provider-dist:
	rm -rf $(BUILD_DIR)/terraform-provider
	./hack/terraform-provider-dist.sh $(VERSION) $(BUILD_DIR)/terraform-provider

terraform-provider-dist-test: terraform-provider-dist
	./hack/terraform-provider-dist-test.sh $(VERSION) $(BUILD_DIR)/terraform-provider

# OpenTofu, checked against versions.mk, for terraform-provider-test.
TOFU_BIN := $(BUILD_DIR)/tools/tofu-$(OPENTOFU_VERSION)
$(TOFU_BIN):
	mkdir -p $(BUILD_DIR)/tools
	curl -fsSL -o $(BUILD_DIR)/tools/tofu.tar.gz https://github.com/opentofu/opentofu/releases/download/v$(OPENTOFU_VERSION)/tofu_$(OPENTOFU_VERSION)_linux_amd64.tar.gz
	echo "$(OPENTOFU_SHA256_AMD64)  $(BUILD_DIR)/tools/tofu.tar.gz" | sha256sum -c -
	tar -xzf $(BUILD_DIR)/tools/tofu.tar.gz -C $(BUILD_DIR)/tools tofu
	mv $(BUILD_DIR)/tools/tofu $@
	rm $(BUILD_DIR)/tools/tofu.tar.gz

# The provider driven by OpenTofu against a real Controller and libvirt
# host (the same container as controller-libvirt-test): hypervisor,
# node, in-place changes, replacement, import, destroy - see
# hack/terraform-provider-test.sh.
terraform-provider-test: kvm-image terraform-provider-build $(TOFU_BIN)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/dashboardd-static ./dashboard/backend
	./hack/terraform-provider-test.sh $(BUILD_DIR)/janus-kvm.qcow2 $(BIN_DIR)/dashboardd-static $(BIN_DIR)/terraform-provider-janus $(TOFU_BIN)

# The Controller creating, admitting, powering and destroying its own
# node on a libvirt/KVM host (a container: hack/libvirt-host), and
# refusing domains it didn't create - see hack/controller-libvirt-test.sh
# and docs/hypervisors.md. dashboardd runs inside that container: built
# static.
controller-libvirt-test: kvm-image
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/dashboardd-static ./dashboard/backend
	./hack/controller-libvirt-test.sh $(BUILD_DIR)/janus-kvm.qcow2 $(BIN_DIR)/dashboardd-static

# "Local Platform" tranche: a real janusd+haproxy pair as an ordinary
# Docker container, for fast local iteration - see local-dev/
# Dockerfile's own header for the full design and local-dev/README.md
# for how to run it. Run from the repo root (not local-dev/), same
# reasoning as dashboard-image above: the build context needs
# build/janusd and build/haproxy alongside local-dev/ itself.
local-dev-image: daemon-static haproxy-build
	docker build -f local-dev/Dockerfile -t janus-local-dev .

# Phase 3 cont'd: proves Secure Boot signing/enforcement actually works,
# both directions - a UKI signed with a throwaway test key (image/
# secureboot/gen-test-key.sh, generated fresh every run, never
# committed) boots under real, Secure-Boot-enabled UEFI firmware with
# that key enrolled (image/secureboot/enroll-vars.sh); an unsigned UKI
# on the exact same enrolled vars is refused by firmware itself
# ("Access Denied"), never reaching the kernel. Requires sbsigntool
# (ukify shells out to sbsign) and python3-virt-firmware (virt-fw-vars).
# See hack/qemu-secureboot-test.sh for why this needs
# OVMF_CODE_4M.secboot.fd and -machine q35,smm=on specifically, unlike
# every other UEFI boot test here.
qemu-secureboot-test: kernel-build rootfs-build
	./hack/qemu-secureboot-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs

# Phase 3 cont'd: proves LifecycleService.Upgrade actually installs a
# *genuinely new* rootfs onto the inactive slot over a real gRPC call -
# builds a second, genuinely different rootfs (different squashfs,
# different root hash) and release bundle (image/release/assemble.sh)
# on the fly, boots the existing disk.img, calls `janusctl
# lifecycle upgrade` with the new bundle, and watches the guest
# genuinely reboot itself into it - HTTP healthy again, STATE intact,
# and the kernel's own cmdline confirming the new slot's partitions and
# root hash (not which HTTP port answers - STATE's persisted config is
# shared across both slots by design, see hack/
# qemu-lifecycle-upgrade-test.sh's own header comment for the real bug
# that assumption caught) - then checks Rollback afterward still
# correctly brings back the untouched original slot. Requires
# janusctl built (see `build`). See
# hack/qemu-lifecycle-upgrade-test.sh.
qemu-lifecycle-upgrade-test: build disk-image
	./hack/qemu-lifecycle-upgrade-test.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR)/bzImage $(BUILD_DIR) $(BIN_DIR)/janusctl

# Node-initiated-fetch follow-up: the same proof as
# qemu-lifecycle-upgrade-test, but Source.Reference is a real
# http:// URL (a host-side python3 http.server) instead of a local
# path - proves LifecycleService.Upgrade's new fetchBundleFile URL
# branch actually works over a real network fetch. Requires python3.
qemu-lifecycle-upgrade-url-test: build disk-image
	./hack/qemu-lifecycle-upgrade-url-test.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR)/bzImage $(BUILD_DIR) $(BIN_DIR)/janusctl

# Every SystemService/HAProxyService/NetworkService method beyond the
# lifecycle ones, on a real enforcing node - including janusd restart,
# reboot, reset and shutdown.
# The node's network configuration end to end (offline seed, MAC rename,
# static address, 802.1Q VLAN, NTP over that VLAN setting a 2020 clock,
# confirmed/reverted trials, persistence) on a real enforcing boot -
# see the script's header.
qemu-network-config-test: build kernel-build disk-image
	./hack/qemu-network-config-test.sh $(BUILD_DIR)/bzImage $(BUILD_DIR)/rootfs $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/janusctl

# Optional extensions and image schematics end to end (node_exporter, the
# QEMU guest agent, the schematic in the signed cmdline, Upgrade keeping
# it, clean power-off from the hypervisor) - see the script's header.
qemu-extensions-test: SCHEMATIC = hack/testdata/schematic-all-extensions.json
qemu-extensions-test: build extensions-amd64
	$(MAKE) disk-image SCHEMATIC=$(SCHEMATIC)
	JANUS_SCHEMATIC=$$(go run ./hack/extpack id -schematic $(SCHEMATIC)) \
	./hack/qemu-extensions-test.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR)/bzImage $(BUILD_DIR) $(BIN_DIR)/janusctl $(SCHEMATIC)

# The firewall (nftables extension) on a real enforcing node - see the
# script's header.
qemu-firewall-test: SCHEMATIC = hack/testdata/schematic-firewall.json
qemu-firewall-test: build extension-nftables-amd64
	$(MAKE) disk-image SCHEMATIC=$(SCHEMATIC)
	./hack/qemu-firewall-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/janusctl

# VRRP (keepalived extension) with two real enforcing nodes on a shared
# LAN: master/backup, failover on HAProxy health and on a node going
# away - see the script's header.
qemu-vrrp-test: SCHEMATIC = hack/testdata/schematic-vrrp.json
qemu-vrrp-test: build extension-keepalived-amd64
	$(MAKE) disk-image SCHEMATIC=$(SCHEMATIC)
	./hack/qemu-vrrp-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/janusctl

# BGP (bird extension) between two real enforcing nodes: sessions, an
# anycast route withdrawn while HAProxy doesn't answer - see the script.
qemu-bgp-test: SCHEMATIC = hack/testdata/schematic-bgp.json
qemu-bgp-test: build extension-bird-amd64
	$(MAKE) disk-image SCHEMATIC=$(SCHEMATIC)
	./hack/qemu-bgp-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/janusctl

# Let's Encrypt (letsencrypt extension) against Pebble, the ACME test CA,
# on a real enforcing node: HTTP-01 answered by HAProxy, a wildcard over
# DNS-01, renewal swapped in without a reload - see the script.
pebble:
	GOBIN=$(abspath $(BUILD_DIR))/pebble go install github.com/letsencrypt/pebble/v2/cmd/pebble@$(PEBBLE_VERSION) \
		github.com/letsencrypt/pebble/v2/cmd/pebble-challtestsrv@$(PEBBLE_VERSION)

qemu-acme-test: SCHEMATIC = hack/testdata/schematic-letsencrypt.json
qemu-acme-test: build extension-letsencrypt-amd64 pebble
	$(MAKE) disk-image SCHEMATIC=$(SCHEMATIC)
	./hack/qemu-acme-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/janusctl $(BUILD_DIR)/pebble

# The Consul agent (consul extension) on two real enforcing nodes: a
# server and a client, HAProxy discovering a service through Consul's
# DNS - see the script.
qemu-consul-test: SCHEMATIC = hack/testdata/schematic-consul.json
qemu-consul-test: build extension-consul-amd64
	$(MAKE) disk-image SCHEMATIC=$(SCHEMATIC)
	./hack/qemu-consul-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/janusctl $(EXT_DIR)/tree-consul-amd64/usr/local/sbin/consul

# Bare metal: the same images on NVMe/SATA/pvscsi/USB/virtio-scsi disks
# and e1000e/igb/vmxnet3/e1000 NICs, 4 CPUs, a cloud-init CD-ROM, the ISO
# installing a disk that registers with a Controller - see the script.
qemu-baremetal-test: build dashboard-build disk-image
	./hack/qemu-baremetal-test.sh $(BUILD_DIR)/rootfs $(BUILD_DIR)/bzImage $(BIN_DIR)/janusctl $(BIN_DIR)/dashboardd

qemu-system-api-test: build disk-image
	./hack/qemu-system-api-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/janusctl

# SystemService.PacketCapture on a real enforcing node: a filtered capture
# of real HAProxy traffic over mTLS, the pcap parsed independently.
qemu-packet-capture-test: build disk-image
	./hack/qemu-packet-capture-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/janusctl

# The node's Prometheus exporter on a real enforcing node: Janus's own
# metrics with real values, moved through the API, persisted across a
# reboot, disabled (docs/metrics.md).
qemu-metrics-test: build disk-image
	./hack/qemu-metrics-test.sh $(BUILD_DIR)/rootfs/disk.img $(BIN_DIR)/janusctl

# The same proof over https:// against a real published GitHub Release,
# verified against the node's own bundled CA trust store (requires
# outbound internet).
qemu-lifecycle-upgrade-https-test: build disk-image
	./hack/qemu-lifecycle-upgrade-https-test.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR) $(BIN_DIR)/janusctl

# Controller-relay follow-up: the same proof, but the release bundle
# reaches the node via a real LifecycleService.UploadReleaseFile call
# (streamed over the same mTLS connection every other RPC already
# uses) instead of the node fetching or reading it itself - proves a
# node with zero outbound connectivity can still be updated.
qemu-lifecycle-upgrade-relay-test: build disk-image
	./hack/qemu-lifecycle-upgrade-relay-test.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR)/bzImage $(BUILD_DIR) $(BIN_DIR)/janusctl

# Phase 3 cont'd: proves LifecycleService.Upgrade's wait_for_health -
# a healthy new slot confirms (Supervisor.OnStable -> internal/
# bootcommit's marker cleared) and stays; an unhealthy one (janusd
# built dynamically-linked into a rootfs with no libc/dynamic linker at
# all, so it can never even exec - see hack/
# qemu-lifecycle-upgrade-health-test.sh's own comment) reverts and
# reboots back automatically (Supervisor.GiveUpAfter/OnGiveUp), with no
# RPC call driving the revert itself. Requires janusctl built (see
# `build`).
qemu-lifecycle-upgrade-health-test: build disk-image
	./hack/qemu-lifecycle-upgrade-health-test.sh $(BUILD_DIR)/rootfs/disk.img $(BUILD_DIR)/bzImage $(BUILD_DIR) $(BIN_DIR)/janusctl

# Phase 3 cont'd: proves LifecycleService.Install partitions a genuinely
# blank disk from scratch (internal/diskimage + go-diskfs) and produces
# a real, independently bootable image - janusd runs *natively* on
# the host for the Install call itself (no A/B/STATE machinery of its
# own to need a VM for, same pattern image-build.yml's own "HAProxy
# gRPC API integration test" step already uses), then the result is
# booted under real OVMF to confirm it. Requires root (sudo) for
# haproxy's chroot() and for the go-diskfs GPT/filesystem writes.
# Requires janusctl built (see `build`).
lifecycle-install-test: build rootfs-build
	./hack/lifecycle-install-test.sh $(BUILD_DIR)/rootfs $(BUILD_DIR)/bzImage $(BUILD_DIR)/haproxy $(BUILD_DIR)/janusd $(BIN_DIR)/janusctl

# Point 2 suite, tranche 5: proves a node provisioned with a Controller
# at Install time (tranche 4's controller_address/controller_ca_cert)
# genuinely self-registers with a real, running Janus Controller
# (dashboardd) on its own first boot - no RPC call from the test script
# drives the registration itself, only cmd/janusd's own background
# attempt (internal/selfregister). Requires root (sudo, same reasoning
# as lifecycle-install-test above) and janusctl/dashboardd built.
qemu-self-register-test: build dashboard-build rootfs-build
	./hack/qemu-self-register-test.sh $(BUILD_DIR)/rootfs $(BUILD_DIR)/bzImage $(BUILD_DIR)/haproxy $(BUILD_DIR)/janusd $(BIN_DIR)/janusctl $(BIN_DIR)/dashboardd

# Scaling-provisioning follow-up: proves `janusctl image seed-controller`
# (internal/diskseed) - writes controller_address/controller_ca_cert
# directly onto an already-built disk's existing STATE partition, no
# janusd/gRPC round trip at all - by Installing a disk with NO
# Controller config (the shape a shared, generic, downloadable image
# actually has), seeding it offline, and confirming it still
# self-registers with a real dashboardd on first boot, same as
# qemu-self-register-test's own Install-time-provisioned disk does.
seed-controller-test: build dashboard-build rootfs-build
	./hack/janusctl-seed-controller-test.sh $(BUILD_DIR)/rootfs $(BUILD_DIR)/bzImage $(BUILD_DIR)/haproxy $(BUILD_DIR)/janusd $(BIN_DIR)/janusctl $(BIN_DIR)/dashboardd

# NoCloud/cidata follow-up: proves internal/nocloud + rootfs/init's
# seedControllerFromNoCloud - a disk Installed with NO Controller at
# all, booted alongside a *separate*, locally-attached "cidata"-labeled
# volume, self-registers on its own using controller_address/
# controller_ca_cert read from that volume - the external,
# delivered-at-boot complement to seed-controller-test's embedded-at-
# generation-time approach (see internal/nocloud's own package doc).
nocloud-seed-test: build dashboard-build rootfs-build
	./hack/nocloud-seed-test.sh $(BUILD_DIR)/rootfs $(BUILD_DIR)/bzImage $(BUILD_DIR)/haproxy $(BUILD_DIR)/janusd $(BIN_DIR)/janusctl $(BIN_DIR)/dashboardd

# Phase 3 cont'd: assembles a real Unified Kernel Image (UKI) - kernel +
# exact boot cmdline, one PE/COFF executable - via `ukify`
# (systemd-ukify), and a FAT32 ESP image with it installed at the
# UEFI-spec removable-media fallback path (image/uki/esp-image.sh, no
# mount/loop device, mtools only). root's data/hash devices are baked
# in as /dev/vdb+/dev/vdc, not /dev/vda+/dev/vdb - the ESP itself takes
# the vda slot once it's attached (see hack/qemu-uefi-boot-test.sh's own
# comment for how that was actually caught). Requires ukify
# (systemd-ukify) and mtools/dosfstools.
uki-image: kernel-build rootfs-build
	./image/uki/assemble.sh $(BUILD_DIR)/rootfs/janus.efi $(BUILD_DIR)/bzImage \
		$(BUILD_DIR)/rootfs /dev/vdb /dev/vdc
	./image/uki/esp-image.sh $(BUILD_DIR)/rootfs/esp.img $(BUILD_DIR)/rootfs/janus.efi 64

# Phase 3 cont'd: proves the UKI actually boots under *real* UEFI
# firmware (OVMF) - no QEMU -kernel/-append shortcut at all, unlike
# every other boot test here. See hack/qemu-uefi-boot-test.sh. Requires
# OVMF (package: ovmf).
qemu-uefi-boot-test: uki-image
	./hack/qemu-uefi-boot-test.sh $(BUILD_DIR)/rootfs $(BUILD_DIR)/rootfs/esp.img
