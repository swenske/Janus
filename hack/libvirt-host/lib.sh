# shellcheck shell=bash
# Sourced by the tests that run a libvirt host in a container
# (hack/libvirt-host) with a Controller inside it - libvirtd, QEMU with
# OVMF and sshd, the Controller talking to libvirt over SSH as on a real
# host. Set before sourcing: TEST_NAME, IMAGE (a janus-kvm.qcow2, served
# inside at http://127.0.0.1:8000/janus-kvm.qcow2), DASHBOARDD (static),
# HOST_PORT (the Controller's API on 127.0.0.1), and optionally
# EXTRA_DOCKER_ARGS. Gives NAME, WORKDIR, API, JAR and:
#
#   lh_start               the host and the Controller, admin set up
#   lh_restart_controller  dashboardd again, logged in again
#   in_host / in_host_i    run inside (in_host_i: with stdin)
#   api                    curl the Controller's API with the session
#   json EXPR              a Python expression on JSON from stdin (d)
#   fail MSG               report, with the Controller's log, and exit

NAME="janus-${TEST_NAME}-${JANUS_TEST_PORT_OFFSET:-0}"
WORKDIR="$(mktemp -d)"
API="https://127.0.0.1:${HOST_PORT}"
JAR="$WORKDIR/cookies"
LH_PASSWORD="libvirt-test-password"

fail() {
  echo "${TEST_NAME} test FAILED: $*" >&2
  docker exec "$NAME" sh -c 'cat /work/dashboardd.log; virsh list --all' >&2 2>/dev/null || true
  exit 1
}
lh_cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap lh_cleanup EXIT

in_host() { docker exec "$NAME" "$@"; }
in_host_i() { docker exec -i "$NAME" "$@"; }
api() { curl -sk -b "$JAR" "$@"; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

lh_wait_controller() {
  for _ in $(seq 1 30); do
    [ "$(curl -sk -o /dev/null -w '%{http_code}' "$API/api/auth/status")" = 200 ] && return 0
    sleep 0.5
  done
  fail "the Controller never answered"
}

# Nodes boot on janus-test (192.168.123.0/24) and register at its gateway.
lh_run_controller() {
  docker exec -d "$NAME" sh -c 'exec dashboardd -addr :18080 -register-addr :18443 -data-dir /work/data -advertise-address 192.168.123.1 >>/work/dashboardd.log 2>&1'
  lh_wait_controller
}

lh_start() {
  local self_dir
  self_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  docker build -q -t janus-libvirt-host "$self_dir" >/dev/null
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  # --init: libvirt daemonizes its QEMU probes, and an orphan left to a
  # PID 1 that never reaps it stays a zombie libvirt waits on forever.
  # --group-add: /dev/kvm's group, which is all root may rely on in an
  # unprivileged LXC (the CI runners) - see entrypoint.sh.
  # shellcheck disable=SC2086
  docker run -d --init --name "$NAME" --privileged --device /dev/kvm --group-add "$(stat -c %g /dev/kvm)" \
    -p "127.0.0.1:${HOST_PORT}:18080" \
    -v "$DASHBOARDD:/usr/local/bin/dashboardd:ro" -v "$IMAGE:/images/janus-kvm.qcow2:ro" \
    ${EXTRA_DOCKER_ARGS:-} janus-libvirt-host >/dev/null
  for _ in $(seq 1 60); do
    docker logs "$NAME" 2>&1 | grep -q "libvirt-host ready" && break
    sleep 1
  done
  docker logs "$NAME" 2>&1 | grep -q "libvirt-host ready" || fail "the libvirt host never came up: $(docker logs "$NAME" 2>&1 | tail -5)"
  in_host mkdir -p /work/data
  docker exec -d "$NAME" sh -c 'cd /images && exec python3 -m http.server 8000 --bind 127.0.0.1 >/work/http.log 2>&1'
  lh_run_controller
  local code
  code="$(curl -sk -c "$JAR" -o /dev/null -w '%{http_code}' -X POST "$API/api/auth/setup" -H 'Content-Type: application/json' -d "{\"password\":\"$LH_PASSWORD\"}")"
  [ "$code" = 204 ] || fail "admin setup returned $code"
}

lh_restart_controller() {
  in_host pkill -x dashboardd
  sleep 1
  lh_run_controller
  curl -sk -c "$JAR" -o /dev/null -X POST "$API/api/auth/login" -H 'Content-Type: application/json' -d "{\"password\":\"$LH_PASSWORD\"}"
}
