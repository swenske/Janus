#!/usr/bin/env bash
# Proves the node's Prometheus exporter (internal/exporter, docs/metrics.md)
# on a real node: the full disk image under real OVMF, SELinux enforcing
# (baked into the UKI). Scrapes :10056/metrics and checks Janus's own
# metrics hold real values - boot slot, API certificates, a certificate
# uploaded to HAProxy with its real expiry, HAProxy up and its config
# applies counted, SELinux enforcing with zero
# denials, STATE with zero errors, API calls counted; then moves the
# exporter to another port through the API, reboots the node, finds it on
# that port (persisted on STATE), disables it, and checks no AVC denial
# appeared at any point.
#
# Usage: hack/qemu-metrics-test.sh <disk.img> <janusctl-bin>
set -euo pipefail

export PATH="$PATH:/usr/sbin:/sbin"

SRC_DISK="${1:?usage: $0 <disk.img> <janusctl-bin>}"
CTL_BIN="${2:?usage: $0 <disk.img> <janusctl-bin>}"
HTTP_TIMEOUT_SECS="${QEMU_METRICS_HTTP_TIMEOUT:-60}"
P_HTTP="${QEMU_METRICS_HTTP_PORT:-$((18301 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_GRPC="${QEMU_METRICS_GRPC_PORT:-$((18302 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_METRICS="${QEMU_METRICS_PORT:-$((18303 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
P_METRICS2="${QEMU_METRICS_PORT2:-$((18304 + ${JANUS_TEST_PORT_OFFSET:-0}))}"

OVMF_CODE="${OVMF_CODE:-/usr/share/OVMF/OVMF_CODE_4M.fd}"
OVMF_VARS_TEMPLATE="${OVMF_VARS_TEMPLATE:-/usr/share/OVMF/OVMF_VARS_4M.fd}"
[ -f "$OVMF_CODE" ] || { echo "OVMF firmware not found at $OVMF_CODE (package: ovmf)" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
QEMU_PID=""
cleanup() {
  [ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

LOG="$WORKDIR/boot.log"
fail() {
  echo "Metrics test FAILED: $*" >&2
  [ -f "$LOG" ] && { echo "--- console output ---" >&2; cat "$LOG" >&2; }
  exit 1
}

DISK="$WORKDIR/disk.img"
cp "$SRC_DISK" "$DISK"
cp "$OVMF_VARS_TEMPLATE" "$WORKDIR/OVMF_VARS.fd"
# No -no-reboot: the node reboots itself in the middle of the test.
qemu-system-x86_64 -accel kvm -accel tcg \
  -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
  -drive if=pflash,format=raw,file="$WORKDIR/OVMF_VARS.fd" \
  -drive file="$DISK",format=raw,if=virtio \
  -nographic -display none -m 512M \
  -netdev "user,id=net0,hostfwd=tcp::${P_HTTP}-:8080,hostfwd=tcp::${P_GRPC}-:9505,hostfwd=tcp::${P_METRICS}-:10056,hostfwd=tcp::${P_METRICS2}-:10099" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

wait_http() {
  local deadline=$((SECONDS + HTTP_TIMEOUT_SECS))
  until [ "$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${P_HTTP}/" || true)" = "200" ]; do
    [ "$SECONDS" -lt "$deadline" ] || fail "HAProxy never answered HTTP 200 within ${HTTP_TIMEOUT_SECS}s"
    sleep 1
  done
}
wait_http

STATE_START_SECTOR="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^First sector/ {print $2}' | awk '{print $1}')"
STATE_SIZE_SECTORS="$(sgdisk -i 6 "$DISK" | awk -F': ' '/^Partition size/ {print $2}' | awk '{print $1}')"
dd if="$DISK" of="$WORKDIR/state.img" bs=512 skip="$STATE_START_SECTOR" count="$STATE_SIZE_SECTORS" status=none
for f in ca.crt admin.crt admin.key; do
  debugfs -R "dump pki/$f $WORKDIR/$f" "$WORKDIR/state.img" >/dev/null 2>&1
  [ -s "$WORKDIR/$f" ] || fail "couldn't extract pki/$f from STATE"
done
ctl() { "$CTL_BIN" -endpoint "127.0.0.1:${P_GRPC}" -ca "$WORKDIR/ca.crt" -cert "$WORKDIR/admin.crt" -key "$WORKDIR/admin.key" "$@"; }

# Before the first scrape (HAProxy's certificates are cached a minute):
# a certificate in HAProxy's store, then two config applies - the reload
# puts the stored certificate back into the new process.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 45 \
  -subj "/O=Janus test/CN=metrics.example.test" -keyout "$WORKDIR/k.pem" -out "$WORKDIR/c.pem" 2>/dev/null
cat "$WORKDIR/c.pem" "$WORKDIR/k.pem" > "$WORKDIR/site.pem"
ctl haproxy cert-upload metrics-test.pem "$WORKDIR/site.pem" >/dev/null || fail "cert-upload"
CERT_NOT_AFTER="$(date -d "$(openssl x509 -in "$WORKDIR/c.pem" -noout -enddate | cut -d= -f2)" +%s)"
ctl haproxy get-config > "$WORKDIR/haproxy.cfg"
ctl haproxy apply-config "$WORKDIR/haproxy.cfg" >/dev/null || fail "re-applying the running config was refused"
printf 'global\n  this is not haproxy\n' > "$WORKDIR/bad.cfg"
ctl haproxy apply-config "$WORKDIR/bad.cfg" >/dev/null 2>&1 && fail "a broken config was accepted"
wait_http

scrape() { curl -fsS -m 5 "http://127.0.0.1:$1/metrics"; }
METRICS="$(scrape "$P_METRICS")" || fail "no metrics on :10056"

# The listen address: on the guest's loopback only, the host (QEMU
# forwards to the guest's own address) doesn't reach it any more; back
# on every address, it does. A bad address is refused.
ctl system metrics -address 127.0.0.1 | grep -q 'http://127.0.0.1:10056/metrics' || fail "-address 127.0.0.1 wasn't taken"
if scrape "$P_METRICS" >/dev/null 2>&1; then fail "the exporter still answers on every address after -address 127.0.0.1"; fi
if ctl system metrics -address not-an-ip >/dev/null 2>&1; then fail "an address that isn't an IP was accepted"; fi
ctl system metrics -address '*' | grep -q 'http://<node>:10056/metrics' || fail "-address '*' didn't put it back on every address"
scrape "$P_METRICS" >/dev/null || fail "no metrics on :10056 after -address '*'"
echo "  ok: the exporter's listen address (one address, every address, a bad one refused)"
echo "$METRICS" > "$WORKDIR/metrics.txt"
# value SAMPLE: the value of the sample written exactly SAMPLE (name and
# labels), or of the first one starting with SAMPLE if it ends in "*".
value() {
  case "$1" in
    *\*) { grep -v '^#' "$WORKDIR/metrics.txt" | grep -F "${1%\*}" || true; } | head -1 | awk '{print $NF}' ;;
    *) { grep -v '^#' "$WORKDIR/metrics.txt" | grep -F "$1 " || true; } | head -1 | awk '{print $NF}' ;;
  esac
}
expect() { # expect DESCRIPTION SAMPLE PYTHON-CONDITION-ON-v
  local v
  v="$(value "$2")"
  [ -n "$v" ] || fail "$1: no sample $2 in:
$(cat "$WORKDIR/metrics.txt")"
  python3 -c "import sys,time; v=float(sys.argv[1]); now=time.time(); sys.exit(0 if ($3) else 1)" "$v" \
    || fail "$1: $2 = $v"
  echo "  ok: $1 ($2 = $v)"
}
DEFAULT_ID="$(cd "$(dirname "$0")/.." && go run ./hack/extpack id)"
grep -q "^janus_build_info{version=\"[^\"]\+\",go_version=\"go[0-9.]\+\",arch=\"amd64\",schematic=\"$DEFAULT_ID\"} 1$" "$WORKDIR/metrics.txt" \
  || fail "janus_build_info doesn't carry the version and the default schematic"
echo "  ok: build info with the default schematic"
grep -q '^janus_boot_info{slot="A",kernel="[0-9.]\+"} 1$' "$WORKDIR/metrics.txt" || fail "janus_boot_info isn't slot A"
echo "  ok: boot slot A"
# The image's HAProxy branch and kernel track (its image.json): the
# defaults, which a default image doesn't pin.
grep -q '^janus_component_info{component="haproxy",variant="[0-9]\+\.[0-9]\+",version="[0-9.]\+",pinned="false"} 1$' "$WORKDIR/metrics.txt" \
  || fail "janus_component_info doesn't carry the image's HAProxy branch"
grep -q '^janus_component_info{component="kernel",variant="[a-z]\+",version="[0-9.]\+",pinned="false"} 1$' "$WORKDIR/metrics.txt" \
  || fail "janus_component_info doesn't carry the image's kernel track"
echo "  ok: the image's HAProxy branch and kernel track"
expect "the API CA expires in years" 'janus_certificate_expiry_timestamp_seconds{source="api",certificate="ca",cn="Janus node CA: *' 'v > now + 5*365*86400'
expect "the API server certificate is valid" 'janus_certificate_expiry_timestamp_seconds{source="api",certificate="server",cn=""}' 'v > now + 30*86400'
expect "the uploaded HAProxy certificate's real expiry" 'janus_certificate_expiry_timestamp_seconds{source="haproxy",certificate="metrics-test.pem",cn="metrics.example.test"}' "v == $CERT_NOT_AFTER"
expect "HAProxy is up" 'janus_haproxy_up' 'v == 1'
expect "the accepted apply is counted" 'janus_haproxy_config_applies_total{result="accepted"}' 'v == 1'
expect "the rejected apply is counted" 'janus_haproxy_config_applies_total{result="rejected"}' 'v == 1'
expect "the apply reloaded HAProxy" 'janus_haproxy_reloads_total' 'v == 1'
expect "no unexpected HAProxy exit" 'janus_haproxy_unexpected_exits_total' 'v == 0'
expect "the apply's time" 'janus_haproxy_config_last_apply_timestamp_seconds' 'now - 300 < v <= now + 5'
expect "SELinux enforcing" 'janus_selinux_enforcing' 'v == 1'
expect "no SELinux denial" 'janus_selinux_denials_total' 'v == 0'
expect "no OOM kill" 'janus_kernel_oom_kills_total' 'v == 0'
expect "no STATE filesystem error" 'janus_state_filesystem_errors' 'v == 0'
expect "no upgrade waiting for confirmation" 'janus_upgrade_pending_confirmation' 'v == 0'
expect "no network trial" 'janus_network_trial_pending' 'v == 0'
expect "API calls are counted" 'janus_api_requests_total{method="HAProxyService/CertificateUpload",code="OK"}' 'v == 1'
grep -q '^janus_time_synchronized [01]$' "$WORKDIR/metrics.txt" || fail "no janus_time_synchronized"
echo "  ok: time sync reported"

# Move the exporter through the API; it persists on STATE across a reboot.
ctl system metrics -port 10099 | grep -q ":10099/metrics" || fail "moving the exporter to 10099"
scrape "$P_METRICS2" >/dev/null || fail "nothing on :10099 after the move"
if scrape "$P_METRICS" >/dev/null 2>&1; then fail ":10056 still answers after the move"; fi
echo "  ok: moved to port 10099 through the API"
boots_before="$(grep -ac "J A N U S" "$LOG" || true)"
ctl system reboot >/dev/null || fail "reboot"
deadline=$((SECONDS + HTTP_TIMEOUT_SECS + 30))
until [ "$(grep -ac "J A N U S" "$LOG" || true)" -gt "$boots_before" ] && scrape "$P_METRICS2" >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "the node never came back with the exporter on :10099"
  sleep 2
done
echo "  ok: after a reboot, still on 10099"
ctl system metrics -disable | grep -q "disabled" || fail "disabling the exporter"
if scrape "$P_METRICS2" >/dev/null 2>&1; then fail "still answering once disabled"; fi
echo "  ok: disabled"

if grep -aq "avc:.*denied" "$LOG"; then
  grep -a "avc:.*denied" "$LOG" >&2
  fail "AVC denials under enforcing"
fi
echo "Metrics test OK: the exporter serves Janus's own metrics with real values under SELinux enforcing, moves and persists through the API, zero AVC denials"
