#!/usr/bin/env bash
# Proves Janus on the hardware a real machine has, not only QEMU's virtio:
# every boot is UEFI (OVMF, q35), SELinux enforcing, the UKIs naming their
# root by partition label (PARTLABEL=BOOT-A-DATA...) - so the same image
# boots whatever its disk is called.
#
#   1. NVMe disk + Intel e1000e NIC, 4 CPUs: HTTP, the 4 CPUs seen (SMP),
#      the screen console shows text (a screenshot is kept), then a
#      Rollback reboots into slot B on nvme0n1p4 with STATE kept.
#   2. SATA disk (AHCI) + Intel igb NIC + a cloud-init CD-ROM (ISO9660
#      "cidata" on SATA, like Proxmox's cloud-init drive): the node finds
#      its Controller there and registers itself.
#   3. VMware's devices - a pvscsi disk + the vmxnet3 NIC (the system disk
#      on SATA: OVMF can't boot from pvscsi, VMware's firmware can).
#   4. The installer ISO on a USB stick + a blank virtio-scsi disk (both
#      sdX: which is which depends on probing order) + Intel e1000 NIC:
#      the ISO installs the disk with a Controller, the installed disk
#      boots alone and registers itself - docs/provisioning-a-node.md's
#      ISO method, end to end.
#
# A native dashboardd stands in for the Controller, reached from the
# guests at QEMU's host gateway (10.0.2.2).
#
# Usage: hack/qemu-baremetal-test.sh <rootfs-dir> <bzImage> <janusctl> <dashboardd>
#   <rootfs-dir> holds disk.img (make disk-image) and the rootfs
#   (rootfs.squashfs, rootfs.verity) the ISO and its bundle are built from.
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

ROOTFS_DIR="${1:?usage: $0 <rootfs-dir> <bzImage> <janusctl> <dashboardd>}"
KERNEL="${2:?}"
CTL="$(cd "$(dirname "$3")" && pwd)/$(basename "$3")"
DASHBOARDD="$(cd "$(dirname "$4")" && pwd)/$(basename "$4")"
SELF_DIR="$(cd "$(dirname "$0")" && pwd)"

BASE="${QEMU_BAREMETAL_BASE_PORT:-$((18700 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
HTTP_PORT=$((BASE)) GRPC_PORT=$((BASE + 1)) DASH_PORT=$((BASE + 2)) REG_PORT=$((BASE + 3))
TIMEOUT="${QEMU_BAREMETAL_TIMEOUT:-120}"
OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
GATEWAY=10.0.2.2
FIRST_BOOT_MSG="pki: first boot - generated a new CA"

WORKDIR="$(mktemp -d)"
QEMU_PID="" DASH_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  [ -n "$DASH_PID" ] && kill "$DASH_PID" 2>/dev/null || true
  rm -rf "${WORKDIR:?}"
}
trap cleanup EXIT
LOG=""
fail() {
  echo "Bare-metal test FAILED: $*" >&2
  [ -n "$LOG" ] && [ -f "$LOG" ] && { echo "--- console ($LOG) ---" >&2; tail -120 "$LOG" >&2; }
  exit 1
}

# --- the Controller -------------------------------------------------------
mkdir -p "$WORKDIR/dash"
"$DASHBOARDD" -addr "127.0.0.1:$DASH_PORT" -register-addr ":$REG_PORT" -data-dir "$WORKDIR/dash" \
  -advertise-address "$GATEWAY" > "$WORKDIR/dashboardd.log" 2>&1 &
DASH_PID=$!
sleep 1
JAR="$WORKDIR/jar"
code="$(curl -sk -c "$JAR" -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:$DASH_PORT/api/auth/setup" \
  -H 'Content-Type: application/json' -d '{"password":"baremetal-test-admin-pw","mfa_required":"nobody"}')"
[ "$code" = 204 ] || fail "Controller setup answered $code"
CONTROLLER_CA="$WORKDIR/dash/dashboard-identity.crt"
pending() { curl -sk -b "$JAR" "https://127.0.0.1:$DASH_PORT/api/pending" | python3 -c 'import json,sys; d=json.load(sys.stdin) or []; print(len(d))'; }

# --- helpers ----------------------------------------------------------------
# boot NAME ARGS...: a q35 UEFI machine with a std VGA screen and a
# monitor socket; NIC and disks come from ARGS.
boot() {
  local name=$1
  shift
  LOG="$WORKDIR/$name.log"
  cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/$name.vars"
  qemu-system-x86_64 -accel kvm -accel tcg -machine q35 -m 1024 -display none -vga std \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$WORKDIR/$name.vars" \
    -monitor "unix:$WORKDIR/$name.mon,server,nowait" \
    -serial file:"$LOG" "$@" &
  QEMU_PID=$!
}
stop() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  wait "$QEMU_PID" 2>/dev/null || true
  QEMU_PID=""
}
# net DEVICE: the NIC, user networking with the HTTP and API ports forwarded.
net() { echo "-netdev user,id=net0,hostfwd=tcp::$HTTP_PORT-:8080,hostfwd=tcp::$GRPC_PORT-:9505 -device $1,netdev=net0"; }
http() { curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$HTTP_PORT/" || true; }
wait_up() { # wait_up WHAT: HTTP 200 and janusd listening (after its PKI)
  local deadline=$((SECONDS + TIMEOUT))
  until [ "$(http)" = 200 ] && grep -aq "listening on :9505" "$LOG"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "$1: never answered"
    kill -0 "$QEMU_PID" 2>/dev/null || fail "$1: QEMU exited"
    sleep 1
  done
}
creds() { # creds PREFIX: the first boot's credentials, off the console
  python3 - "$LOG" "$1" <<'PY'
import re, sys
log = open(sys.argv[1], errors="replace").read().replace("\r", "")
pems = re.findall(r"(-----BEGIN ([A-Z ]+)-----.*?-----END \2-----)", log, re.S)
if len(pems) < 3:
    sys.exit("only %d PEM blocks on the console" % len(pems))
for (pem, _), name in zip(pems, ["ca.crt", "admin.crt", "admin.key"]):
    open(sys.argv[2] + name, "w").write(pem + "\n")
PY
}
ctl() { local p=$1; shift; "$CTL" -endpoint "127.0.0.1:$GRPC_PORT" -ca "${p}ca.crt" -cert "${p}admin.crt" -key "${p}admin.key" "$@"; }
no_denials() { ! grep -aq "avc:.*denied" "$LOG" || fail "$1: AVC denials: $(grep -a 'avc:.*denied' "$LOG" | head -5)"; }
wait_pending() { # wait_pending N WHAT
  local deadline=$((SECONDS + 60))
  until [ "$(pending)" -ge "$1" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "$2: no registration reached the Controller (pending: $(pending))"
    sleep 1
  done
}

DISK_SRC="$ROOTFS_DIR/disk.img"
[ -f "$DISK_SRC" ] || fail "no $DISK_SRC (make disk-image)"

# === 1. NVMe + e1000e, 4 CPUs ================================================
cp "$DISK_SRC" "$WORKDIR/nvme.img"
# shellcheck disable=SC2046
boot nvme -smp 4 -drive "if=none,id=d0,file=$WORKDIR/nvme.img,format=raw" -device nvme,drive=d0,serial=janus0 $(net e1000e)
wait_up "NVMe + e1000e"
grep -aq "Kernel command line:.*PARTLABEL=BOOT-A-DATA" "$LOG" || fail "the UKI doesn't name its root by label"
grep -aq "nvme0n1" "$LOG" || fail "no nvme0n1 on the console"
grep -aq "e1000e" "$LOG" || fail "the e1000e driver didn't show up"
creds "$WORKDIR/n-"
info="$(ctl "$WORKDIR/n-" system info)" || fail "system info"
grep -q "^cpus: 4$" <<<"$info" || fail "the kernel doesn't use the 4 CPUs: $(grep cpus <<<"$info")"
grep -qi "active slot: A" <<<"$info" || fail "the active slot isn't A: $info"
echo "  ok: NVMe + e1000e - booted by partition label, 4 CPUs, slot A"

# The screen: what the framebuffer console shows (kept for a look).
sleep 2
python3 - "$WORKDIR/nvme.mon" "$WORKDIR/screen.ppm" <<'PY'
import socket, sys, time
s = socket.socket(socket.AF_UNIX); s.connect(sys.argv[1])
time.sleep(0.3); s.recv(65536)
s.sendall(("screendump %s\n" % sys.argv[2]).encode()); time.sleep(1.5); s.close()
PY
python3 - "$WORKDIR/screen.ppm" <<'PY' || fail "the screen shows nothing"
import sys
data = open(sys.argv[1], "rb").read()
# P6\n<w> <h>\n255\n<rgb...>
parts = data.split(b"\n", 3)
w, h = map(int, parts[1].split()); px = parts[3]
lit = sum(1 for i in range(0, len(px) - 2, 3) if px[i] + px[i+1] + px[i+2] > 300)
print("  screen %dx%d, %d lit pixels" % (w, h, lit))
sys.exit(0 if lit > 2000 else 1)
PY
cp "$WORKDIR/screen.ppm" "${SCREENSHOT:-/dev/null}" 2>/dev/null || true
echo "  ok: the screen console shows text"

ctl "$WORKDIR/n-" lifecycle rollback >/dev/null || fail "Rollback"
deadline=$((SECONDS + TIMEOUT))
until [ "$(grep -ac 'Kernel command line:' "$LOG")" -ge 2 ] && [ "$(http)" = 200 ] && [ "$(grep -ac 'listening on :9505' "$LOG")" -ge 2 ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "no reboot into slot B after the Rollback"
  sleep 1
done
grep -a "Kernel command line:" "$LOG" | tail -1 | grep -q "PARTLABEL=BOOT-B-DATA" || fail "the second boot isn't slot B"
[ "$(grep -ac "$FIRST_BOOT_MSG" "$LOG")" -eq 1 ] || fail "STATE (nvme0n1p6) wasn't kept across the Rollback"
info="$(ctl "$WORKDIR/n-" system info 2>&1)" || fail "system info after the Rollback: $info"
grep -qi "active slot: B" <<<"$info" || fail "janusd doesn't report slot B: $info"
no_denials "NVMe"
stop
echo "  ok: Rollback switched the ESP on nvme0n1p1 and rebooted into slot B, STATE kept"

# === 2. SATA + igb + a cidata CD-ROM =========================================
mkdir -p "$WORKDIR/cidata"
python3 - "$CONTROLLER_CA" "$GATEWAY:$REG_PORT" > "$WORKDIR/cidata/user-data" <<'PY'
import json, sys
print(json.dumps({"controller_address": sys.argv[2], "controller_ca_cert": open(sys.argv[1]).read()}))
PY
: > "$WORKDIR/cidata/meta-data"
xorriso -as mkisofs -quiet -V cidata -J -r -o "$WORKDIR/cidata.iso" "$WORKDIR/cidata" 2>/dev/null
cp "$DISK_SRC" "$WORKDIR/sata.img"
# shellcheck disable=SC2046
boot sata -drive "if=none,id=d0,file=$WORKDIR/sata.img,format=raw" -device ide-hd,drive=d0,bus=ide.0 \
  -drive "if=none,id=cd,file=$WORKDIR/cidata.iso,format=raw,media=cdrom,readonly=on" -device ide-cd,drive=cd,bus=ide.1 \
  $(net igb)
wait_up "SATA + igb"
grep -aq "igb" "$LOG" || fail "the igb driver didn't show up"
wait_pending 1 "SATA + cidata CD-ROM"
no_denials "SATA"
stop
echo "  ok: SATA (AHCI) + igb - the node read its Controller off the cloud-init CD-ROM and registered itself"

# === 3. VMware: pvscsi + vmxnet3 =============================================
# OVMF has no pvscsi driver (VMware's own firmware does), so it can't boot
# from one: the system disk is SATA - VMware offers that too - and a
# second disk sits on pvscsi, for the kernel's driver.
cp "$DISK_SRC" "$WORKDIR/vmware.img"
truncate -s 64M "$WORKDIR/pvscsi-data.img"
# shellcheck disable=SC2046
boot vmware -drive "if=none,id=d0,file=$WORKDIR/vmware.img,format=raw" -device ide-hd,drive=d0,bus=ide.0 \
  -device pvscsi,id=pv -drive "if=none,id=d1,file=$WORKDIR/pvscsi-data.img,format=raw" -device scsi-hd,drive=d1,bus=pv.0 \
  $(net vmxnet3)
wait_up "SATA + pvscsi + vmxnet3"
grep -aq "vmw_pvscsi" "$LOG" || fail "the pvscsi driver didn't show up"
grep -aqE "sd [0-9:]+: \[sd[a-z]\] 131072 512-byte logical blocks" "$LOG" || fail "the kernel doesn't see the 64 MiB pvscsi disk"
grep -aq "vmxnet3" "$LOG" || fail "the vmxnet3 driver didn't show up"
no_denials "VMware devices"
stop
echo "  ok: VMware's devices - a pvscsi disk, the API and HTTP over vmxnet3"

# === 4. The ISO on a USB stick installs a virtio-scsi disk ===================
BUNDLE="$WORKDIR/bundle"
"$SELF_DIR/../image/release/assemble.sh" "$BUNDLE" "$KERNEL" "$ROOTFS_DIR" >/dev/null
SHA256="$(cat "$BUNDLE/rootfs.squashfs.sha256")"
"$SELF_DIR/../image/iso/assemble.sh" "$WORKDIR/janus.iso" "$KERNEL" "$ROOTFS_DIR" "$BUNDLE" >/dev/null 2>&1
truncate -s 1G "$WORKDIR/target.img"
# shellcheck disable=SC2046
boot iso -device qemu-xhci -drive "if=none,id=usb,file=$WORKDIR/janus.iso,format=raw" -device usb-storage,drive=usb,bootindex=0 \
  -device virtio-scsi-pci,id=vs -drive "if=none,id=t,file=$WORKDIR/target.img,format=raw" -device scsi-hd,drive=t,bus=vs.0 \
  $(net e1000)
wait_up "the ISO on USB"
grep -aq "Kernel command line:.*PARTLABEL=JANUS-ISO-DATA" "$LOG" || fail "the ISO doesn't name its root by its own label"
creds "$WORKDIR/i-"
mounts="$(ctl "$WORKDIR/i-" system mounts)"
iso_disk="$(awk '$3 == "/etc/janus/release" {sub(/[0-9]+$/, "", $1); print $1}' <<<"$mounts")"
[ -n "$iso_disk" ] || fail "the ISO's bundle isn't mounted: $mounts"
target=""
for d in /dev/sda /dev/sdb; do [ "$d" != "$iso_disk" ] && target=$d; done
echo "  the ISO is $iso_disk, the target $target"
out="$(ctl "$WORKDIR/i-" lifecycle install -insecure-skip-signature-check -sha256 "$SHA256" \
  -controller-address "$GATEWAY:$REG_PORT" -controller-ca "$CONTROLLER_CA" "$target" /etc/janus/release 2>&1)" \
  || fail "Install: $out"
grep -q '\[done ' <<<"$out" || fail "Install didn't finish: $out"
no_denials "the ISO"
stop
echo "  ok: the ISO booted from USB and installed $target with a Controller"

before="$(pending)"
# shellcheck disable=SC2046
boot installed -device virtio-scsi-pci,id=vs -drive "if=none,id=t,file=$WORKDIR/target.img,format=raw" -device scsi-hd,drive=t,bus=vs.0 \
  $(net e1000)
wait_up "the installed disk"
grep -aq "Kernel command line:.*PARTLABEL=BOOT-A-DATA" "$LOG" || fail "the installed disk doesn't boot by label"
wait_pending $((before + 1)) "the installed disk"
no_denials "the installed disk"
stop
echo "  ok: the installed disk (virtio-scsi) booted and registered itself with the Controller"

echo "Bare-metal test OK: NVMe/e1000e with 4 CPUs and a rollback, SATA/igb with a cloud-init CD-ROM, VMware's pvscsi/vmxnet3, the ISO on USB installing a virtio-scsi disk that registers itself - all by partition label, enforcing, zero AVC denials"
