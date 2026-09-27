#!/usr/bin/env bash
# Proves internal/diskseed.SeedController (exposed as `janusctl image
# seed-controller`) end to end: a disk Installed with NO Controller at
# all - the shape a generic, shared, downloadable image actually has -
# gets its controller_address/controller_ca_cert written afterward,
# offline, with no janusd/gRPC round trip at all, and the resulting
# disk still self-registers with a real dashboardd on first boot,
# exactly like hack/qemu-self-register-test.sh's own Install-time-
# provisioned disk does. That's the whole point of this package: the
# same shared image, seeded once per environment instead of reinstalled
# once per node.
#
# Reuses hack/qemu-self-register-test.sh's own native-dashboardd/
# native-janusd-for-Install/OVMF-boot pattern; see that script's own
# header comment for the full reasoning behind each piece.
#
# Usage: hack/janusctl-seed-controller-test.sh <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

ROOTFS_DIR="${1:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
KERNEL="${2:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
HAPROXY_BIN="${3:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
JANUSD_BIN="${4:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
CTL_REL="${5:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
DASHBOARDD_REL="${6:?usage: $0 <rootfs-dir> <bzImage> <haproxy-bin> <janusd-bin> <janusctl-bin> <dashboardd-bin>}"
CTL="$(cd "$(dirname "$CTL_REL")" && pwd)/$(basename "$CTL_REL")"
DASHBOARDD="$(cd "$(dirname "$DASHBOARDD_REL")" && pwd)/$(basename "$DASHBOARDD_REL")"

DISK_MB="${SEED_TEST_DISK_MB:-512}"
HTTP_TIMEOUT_SECS="${SEED_TEST_HTTP_TIMEOUT:-40}"
REGISTER_TIMEOUT_SECS="${SEED_TEST_REGISTER_TIMEOUT:-30}"
HOST_HTTP_PORT="${SEED_TEST_HOST_HTTP_PORT:-18150}"
HOST_GRPC_PORT="${SEED_TEST_HOST_GRPC_PORT:-18151}"
NATIVE_GRPC_PORT="${SEED_TEST_NATIVE_GRPC_PORT:-19520}"
DASHBOARD_ADDR_PORT="${SEED_TEST_DASHBOARD_ADDR_PORT:-18152}"
DASHBOARD_REGISTER_PORT="${SEED_TEST_DASHBOARD_REGISTER_PORT:-18153}"
QEMU_HOST_GATEWAY="10.0.2.2"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf) - set \$OVMF_CODE to override" >&2; exit 1; }
[ -f "$OVMF_VARS_TEMPLATE" ] || { echo "OVMF vars template not found at $OVMF_VARS_TEMPLATE - set \$OVMF_VARS_TEMPLATE to override" >&2; exit 1; }

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
WORKDIR="$(mktemp -d)"
JANUSD_PID=""
DASHBOARD_PID=""
QEMU_PID=""
kill_native_haproxy() {
  sudo pkill -x haproxy 2>/dev/null || true
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    pgrep -x haproxy >/dev/null 2>&1 || return 0
    sleep 0.5
  done
  sudo pkill -x -9 haproxy 2>/dev/null || true
}
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  [ -n "$JANUSD_PID" ] && sudo kill "$JANUSD_PID" 2>/dev/null || true
  kill_native_haproxy
  [ -n "$DASHBOARD_PID" ] && kill "$DASHBOARD_PID" 2>/dev/null || true
  sudo rm -rf "$WORKDIR"
}
trap cleanup EXIT

# =========================================================================
# Part 1: a real dashboardd, natively.
# =========================================================================
mkdir -p "$WORKDIR/dashboard-data"
"$DASHBOARDD" -addr ":${DASHBOARD_ADDR_PORT}" -register-addr ":${DASHBOARD_REGISTER_PORT}" \
  -data-dir "$WORKDIR/dashboard-data" -advertise-address "$QEMU_HOST_GATEWAY" \
  > "$WORKDIR/dashboardd.log" 2>&1 &
DASHBOARD_PID=$!
sleep 1
if ! kill -0 "$DASHBOARD_PID" 2>/dev/null; then
  echo "seed-controller test FAILED: dashboardd exited immediately" >&2
  cat "$WORKDIR/dashboardd.log" >&2
  exit 1
fi

COOKIE_JAR="$WORKDIR/cookies.txt"
setup_code="$(curl -sk -c "$COOKIE_JAR" -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/auth/setup" -H "Content-Type: application/json" -d '{"password":"seed-controller-test-admin-pw"}')"
[ "$setup_code" = "204" ] || { echo "seed-controller test FAILED: dashboard admin setup returned $setup_code, want 204" >&2; exit 1; }
echo "Part 1 OK: dashboardd running natively, admin auth established"

CONTROLLER_CA="$WORKDIR/dashboard-data/dashboard-identity.crt"
[ -s "$CONTROLLER_CA" ] || { echo "seed-controller test FAILED: dashboardd never wrote its own identity cert to $CONTROLLER_CA" >&2; exit 1; }

# =========================================================================
# Part 2: Install a blank disk with NO Controller at all - the shape a
# generic, shared, downloadable image actually has.
# =========================================================================
BUNDLE="$WORKDIR/bundle"
"$SELF_DIR/../image/release/assemble.sh" "$BUNDLE" "$KERNEL" "$ROOTFS_DIR"
SHA256="$(cat "$BUNDLE/rootfs.squashfs.sha256")"

PKI_DIR="$WORKDIR/pki"
sudo mkdir -p /run/janus /etc/haproxy /etc/janus
sudo cp "$SELF_DIR/../rootfs/base/etc/haproxy/haproxy.cfg" /etc/haproxy/haproxy.cfg
sudo mkdir -p /var/empty && sudo chmod 000 /var/empty

sudo "$JANUSD_BIN" -addr ":$NATIVE_GRPC_PORT" -haproxy-binary "$HAPROXY_BIN" -pki-dir "$PKI_DIR" \
  > "$WORKDIR/native.log" 2>&1 &
JANUSD_PID=$!

DEADLINE=$((SECONDS + 20))
while ! sudo test -s "$PKI_DIR/admin.crt" && [ "$SECONDS" -lt "$DEADLINE" ]; do sleep 1; done
if ! sudo test -s "$PKI_DIR/admin.crt"; then
  echo "seed-controller test FAILED: janusd never bootstrapped PKI at $PKI_DIR within 20s" >&2
  cat "$WORKDIR/native.log" >&2
  exit 1
fi
NATIVE_CTL_ARGS=(-endpoint "127.0.0.1:${NATIVE_GRPC_PORT}" -ca "$PKI_DIR/ca.crt" -cert "$PKI_DIR/admin.crt" -key "$PKI_DIR/admin.key")

DISK="$WORKDIR/generic-disk.img"
truncate -s "${DISK_MB}M" "$DISK"

INSTALL_OUT="$(sudo "$CTL" "${NATIVE_CTL_ARGS[@]}" lifecycle install -sha256 "$SHA256" "$DISK" "$BUNDLE")"
echo "$INSTALL_OUT"
if ! echo "$INSTALL_OUT" | grep -qi '\[done '; then
  echo "seed-controller test FAILED: Install never reached the 'done' stage" >&2
  exit 1
fi
sudo chown "$(id -u):$(id -g)" "$DISK"

sudo kill "$JANUSD_PID" 2>/dev/null || true
wait "$JANUSD_PID" 2>/dev/null || true
JANUSD_PID=""
kill_native_haproxy
echo "Part 2 OK: Install wrote a generic image with no Controller config at all"

# =========================================================================
# Part 3: seed the Controller config in - offline, no janusd/gRPC.
# =========================================================================
"$CTL" image seed-controller -controller-address "${QEMU_HOST_GATEWAY}:${DASHBOARD_REGISTER_PORT}" -controller-ca "$CONTROLLER_CA" "$DISK"
echo "Part 3 OK: janusctl image seed-controller wrote controller config with no janusd/gRPC round trip"

# A second seed-controller call against the same, already-seeded disk
# must be refused, not silently overwrite - see internal/diskseed's own
# doc comment for why (a real go-diskfs bug found writing this
# package's own unit tests, not a stylistic choice).
if "$CTL" image seed-controller -controller-address "other:9999" -controller-ca "$CONTROLLER_CA" "$DISK" 2>"$WORKDIR/reseed.err"; then
  echo "seed-controller test FAILED: a second seed-controller call against an already-seeded disk should have been refused" >&2
  exit 1
fi
grep -qi "already seeded" "$WORKDIR/reseed.err" || { echo "seed-controller test FAILED: refusal message doesn't mention 'already seeded': $(cat "$WORKDIR/reseed.err")" >&2; exit 1; }
echo "Part 3b OK: a second seed-controller call against the same disk was correctly refused"

# =========================================================================
# Part 4: boot the seeded disk for real - self-registration is expected
# to happen entirely on its own, no RPC call from this script.
# =========================================================================
LOG="$WORKDIR/console.log"
OVMF_VARS="$WORKDIR/vars.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"
qemu-system-x86_64 \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$OVMF_VARS" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -no-reboot -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_HTTP_PORT}-:8080,hostfwd=tcp::${HOST_GRPC_PORT}-:9505" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

DEADLINE=$((SECONDS + HTTP_TIMEOUT_SECS))
CODE=""
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  CODE="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_HTTP_PORT}/" || true)"
  [ "$CODE" = "200" ] && break
  sleep 1
done
if [ "$CODE" != "200" ]; then
  echo "seed-controller test FAILED: the seeded disk never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "Part 4 OK: the seeded disk boots for real"

PENDING_JSON=""
DEADLINE=$((SECONDS + REGISTER_TIMEOUT_SECS))
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  PENDING_JSON="$(curl -sk -b "$COOKIE_JAR" "https://127.0.0.1:${DASHBOARD_ADDR_PORT}/api/pending")"
  [ "$PENDING_JSON" != "null" ] && [ -n "$PENDING_JSON" ] && break
  sleep 1
done
if [ "$PENDING_JSON" = "null" ] || [ -z "$PENDING_JSON" ]; then
  echo "seed-controller test FAILED: no self-registration ever showed up in GET /api/pending within ${REGISTER_TIMEOUT_SECS}s" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  echo "--- dashboardd log ---" >&2; cat "$WORKDIR/dashboardd.log" >&2
  exit 1
fi
grep -q "selfregister: successfully announced" "$LOG" || { echo "seed-controller test FAILED: guest console never logged a successful self-registration" >&2; cat "$LOG" >&2; exit 1; }

PENDING_ID="$(echo "$PENDING_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')"
PENDING_NAME="$(echo "$PENDING_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["name"])')"
[ -n "$PENDING_ID" ] && [ -n "$PENDING_NAME" ] || { echo "seed-controller test FAILED: pending entry missing id/name: $PENDING_JSON" >&2; exit 1; }
echo "Part 5 OK: the seeded (not Installed-with-a-Controller) node self-registered on its own - id=$PENDING_ID name=$PENDING_NAME"

echo "seed-controller test OK: a generic image, seeded offline with janusctl image seed-controller (no janusd/gRPC round trip), genuinely self-registers with a real Controller on first boot, and a second seed attempt against the same disk is correctly refused"
