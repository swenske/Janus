#!/usr/bin/env bash
# Proves the kernel parameters rootfs/init writes at boot (internal/
# sysctl's Baseline: the CIS benchmark's, Janus's own, HAProxy's
# defaults) actually take effect - every one of them, on a real
# boot - not just that the Go code doesn't panic, and not just that the
# matching kernel/configs/janus_<track>_defconfig options compile in (a
# real gap this test's own first draft caught: CONFIG_SYN_COOKIES
# wasn't set, so /proc/sys/net/ipv4/tcp_syncookies didn't exist at all
# and that one write silently logged "no such file or directory" while
# every other sysctl succeeded - the boot itself, and even the network
# test, looked completely fine either way, since a non-fatal write
# failure doesn't block anything). Same boot pattern hack/
# qemu-network-test.sh uses (janusd -> haproxy inside the VM,
# real HTTP over virtio-net), plus a check of every "init: sysctl ..."
# console line hardenSysctls prints - each one is logged as it's
# written, success or failure, specifically so an external test like
# this one can verify the actual outcome without needing a shell inside
# the guest to read /proc/sys back.
#
# Usage: hack/qemu-hardening-test.sh <bzImage> <initramfs.cpio.gz>
set -euo pipefail

KERNEL="${1:?usage: $0 <bzImage> <initramfs.cpio.gz>}"
INITRD="${2:?usage: $0 <bzImage> <initramfs.cpio.gz>}"
HOST_PORT="${QEMU_HARDENING_TEST_PORT:-$((18099 + ${JANUS_TEST_PORT_OFFSET:-0}))}"
TIMEOUT_SECS="${QEMU_HARDENING_TEST_TIMEOUT:-30}"

# Every key internal/sysctl's Baseline writes at boot, and its value -
# the CIS benchmark's controls, Janus's own settings, then the defaults
# of the parameters HAProxy depends on. internal/sysctl's
# TestHardeningScriptMatchesBaseline keeps this list equal to Baseline(),
# so a real boot's console output is checked against it line for line.
declare -A EXPECTED=(
  ["/proc/sys/fs/protected_hardlinks"]="1"
  ["/proc/sys/fs/protected_symlinks"]="1"
  ["/proc/sys/kernel/yama/ptrace_scope"]="2"
  ["/proc/sys/fs/suid_dumpable"]="0"
  ["/proc/sys/kernel/dmesg_restrict"]="1"
  ["/proc/sys/kernel/kptr_restrict"]="2"
  ["/proc/sys/kernel/randomize_va_space"]="2"
  ["/proc/sys/net/ipv4/ip_forward"]="0"
  ["/proc/sys/net/ipv4/conf/all/forwarding"]="0"
  ["/proc/sys/net/ipv4/conf/default/forwarding"]="0"
  ["/proc/sys/net/ipv4/conf/all/send_redirects"]="0"
  ["/proc/sys/net/ipv4/conf/default/send_redirects"]="0"
  ["/proc/sys/net/ipv4/icmp_ignore_bogus_error_responses"]="1"
  ["/proc/sys/net/ipv4/icmp_echo_ignore_broadcasts"]="1"
  ["/proc/sys/net/ipv4/conf/all/accept_redirects"]="0"
  ["/proc/sys/net/ipv4/conf/default/accept_redirects"]="0"
  ["/proc/sys/net/ipv4/conf/all/secure_redirects"]="0"
  ["/proc/sys/net/ipv4/conf/default/secure_redirects"]="0"
  ["/proc/sys/net/ipv4/conf/all/rp_filter"]="1"
  ["/proc/sys/net/ipv4/conf/default/rp_filter"]="1"
  ["/proc/sys/net/ipv4/conf/all/accept_source_route"]="0"
  ["/proc/sys/net/ipv4/conf/default/accept_source_route"]="0"
  ["/proc/sys/net/ipv4/conf/all/log_martians"]="1"
  ["/proc/sys/net/ipv4/conf/default/log_martians"]="1"
  ["/proc/sys/net/ipv4/tcp_syncookies"]="1"
  ["/proc/sys/net/ipv6/conf/all/forwarding"]="0"
  ["/proc/sys/net/ipv6/conf/default/forwarding"]="0"
  ["/proc/sys/net/ipv6/conf/all/accept_redirects"]="0"
  ["/proc/sys/net/ipv6/conf/default/accept_redirects"]="0"
  ["/proc/sys/net/ipv6/conf/all/accept_source_route"]="0"
  ["/proc/sys/net/ipv6/conf/default/accept_source_route"]="0"
  ["/proc/sys/net/ipv6/conf/all/accept_ra"]="0"
  ["/proc/sys/net/ipv6/conf/default/accept_ra"]="0"
  ["/proc/sys/net/ipv4/conf/all/promote_secondaries"]="1"
  ["/proc/sys/net/ipv4/conf/default/promote_secondaries"]="1"
  ["/proc/sys/net/core/somaxconn"]="60000"
  ["/proc/sys/net/ipv4/ip_local_port_range"]="10240 65023"
  ["/proc/sys/net/ipv4/ip_local_reserved_ports"]=""
  ["/proc/sys/net/ipv4/tcp_tw_reuse"]="1"
  ["/proc/sys/net/ipv4/tcp_fin_timeout"]="30"
  ["/proc/sys/net/ipv4/tcp_synack_retries"]="3"
  ["/proc/sys/net/ipv4/ip_nonlocal_bind"]="1"
  ["/proc/sys/net/ipv6/ip_nonlocal_bind"]="1"
  ["/proc/sys/net/core/netdev_max_backlog"]="10000"
  ["/proc/sys/net/ipv4/tcp_keepalive_time"]="7200"
  ["/proc/sys/net/ipv4/tcp_keepalive_intvl"]="75"
  ["/proc/sys/net/ipv4/tcp_keepalive_probes"]="9"
  ["/proc/sys/net/ipv4/tcp_fastopen"]="1"
  # IPv6 on, last - the node boots with it off (ipv6.disable_ipv6=1).
  ["/proc/sys/net/ipv6/conf/all/disable_ipv6"]="0"
)

LOG="$(mktemp)"
trap 'rm -f "$LOG"; [ -n "${QEMU_PID:-}" ] && kill "$QEMU_PID" 2>/dev/null || true' EXIT

# ipv6.disable_ipv6=1, as in the UKI's cmdline (image/uki/assemble.sh).
qemu-system-x86_64 -accel kvm -accel tcg \
  -kernel "$KERNEL" \
  -initrd "$INITRD" \
  -append "console=ttyS0 panic=-1 ip=dhcp ipv6.disable_ipv6=1" \
  -nographic -no-reboot -display none -m 256M \
  -netdev "user,id=net0,hostfwd=tcp::${HOST_PORT}-:8080" \
  -device virtio-net-pci,netdev=net0 \
  -serial file:"$LOG" \
  &
QEMU_PID=$!

deadline=$((SECONDS + TIMEOUT_SECS))
code=""
while [ "$SECONDS" -lt "$deadline" ]; do
  code="$(curl -s -m 2 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${HOST_PORT}/" || true)"
  [ "$code" = "200" ] && break
  sleep 1
done
if [ "$code" != "200" ]; then
  echo "Hardening test FAILED: no HTTP 200 within ${TIMEOUT_SECS}s (last code: '${code}') - hardening shouldn't have broken this" >&2
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "HAProxy OK: HTTP 200 - hardening didn't break normal boot/serving"

# A moment for the last sysctl writes (which happen before HAProxy even
# starts) to have long since flushed to the log file.
sleep 1

fail=0
for path in "${!EXPECTED[@]}"; do
  want="${EXPECTED[$path]}"
  if grep -qF "init: sysctl ${path}=${want}" "$LOG"; then
    continue
  fi
  if grep -qF "init: sysctl ${path}=" "$LOG"; then
    echo "Hardening test FAILED: $path was set, but not to the expected value $want:" >&2
    grep -F "init: sysctl ${path}=" "$LOG" >&2
  else
    echo "Hardening test FAILED: $path was never even attempted (no console line for it at all)" >&2
  fi
  fail=1
done
if [ "$fail" -ne 0 ]; then
  echo "--- console output ---" >&2; cat "$LOG" >&2
  exit 1
fi
echo "All ${#EXPECTED[@]} baseline sysctls confirmed set to their expected values on a real boot"

# IPv6 doesn't copy all/default onto the interfaces that already exist -
# eth0 comes up with the kernel's boot DHCP, before init: the CIS keys
# the kernel reads per interface are written on eth0 itself.
for leaf in accept_ra accept_redirects; do
  if ! grep -qF "init: sysctl /proc/sys/net/ipv6/conf/eth0/${leaf}=0" "$LOG"; then
    echo "Hardening test FAILED: eth0's IPv6 ${leaf} wasn't set to 0" >&2
    exit 1
  fi
done
# And the audit after the saved values: every control compliant.
if ! grep -qE "^init: sysctl: CIS .*: 33/33 controls compliant" <(tr -d '\r' < "$LOG"); then
  echo "Hardening test FAILED: no compliant CIS audit on the console:" >&2
  grep -a "init: sysctl: " "$LOG" >&2
  exit 1
fi
echo "CIS benchmark: 33/33 controls compliant, eth0's own IPv6 values included"

# The one failure mode this test exists specifically to catch: a
# sysctl write that *failed* (a missing /proc/sys node - e.g. the real
# CONFIG_SYN_COOKIES gap this test's own first draft found) would still
# leave every check above passing for every *other* sysctl, so also
# fail loudly on any "init: sysctl ...: <error>" line at all, expected
# or not.
if grep -qE "^init: sysctl .*: [a-zA-Z]" "$LOG"; then
  echo "Hardening test FAILED: at least one sysctl write logged an error:" >&2
  grep -E "^init: sysctl .*: [a-zA-Z]" "$LOG" >&2
  exit 1
fi
grep -qx "init: open files limit 524288" <(tr -d '\r' < "$LOG") || {
  echo "Hardening test FAILED: init didn't raise the open files limit:" >&2
  grep -a "open files limit" "$LOG" >&2
  exit 1
}
echo "Hardening test OK: every kernel hardening sysctl this project sets took effect on a real boot, and HAProxy still serves traffic normally"
