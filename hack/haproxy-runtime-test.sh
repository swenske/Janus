#!/usr/bin/env bash
# Proves janusd's HAProxy runtime API code (internal/haproxy/
# runtime_maps.go, runtime_certs.go) against a real HAProxy binary -
# file-backed map/ACL entries and real uploaded certificates, not just
# the fixtures captured in internal/haproxy/*_test.go. Run once per HAProxy
# branch an image can carry (variants.mk): the runtime API's answers
# changed between branches before.
#
# janusd runs natively here, its HAProxy started with a config that
# declares one file-backed map and ACL, plus an HTTPS frontend backed by a
# crt-list (seeded with one starter cert - HAProxy refuses to start a bind
# with an empty crt-list) to exercise CertificateUpload's -crt-list/-sni
# binding. Uses the real target paths (/run/janus/haproxy-admin.sock,
# ports 8080 and 8443): never run two at once on the same host.
#
# Usage: hack/haproxy-runtime-test.sh <janusd-bin> <janusctl-bin> <haproxy-bin>
set -euo pipefail

JANUSD="$(realpath "${1:?usage: $0 <janusd-bin> <janusctl-bin> <haproxy-bin>}")"
CTL_BIN="$(realpath "${2:?usage: $0 <janusd-bin> <janusctl-bin> <haproxy-bin>}")"
HAPROXY="$(realpath "${3:?usage: $0 <janusd-bin> <janusctl-bin> <haproxy-bin>}")"
P_GRPC="${HAPROXY_RUNTIME_GRPC_PORT:-19506}"

echo "haproxy-runtime-test: $("$HAPROXY" -v | head -1)"

WORKDIR="$(mktemp -d)"
JANUSD_PID=""
cleanup() {
    [ -n "$JANUSD_PID" ] && kill "$JANUSD_PID" 2>/dev/null || true
    # janusd's own pid file for the HAProxy it started - never pkill -f a
    # path that is also on this script's command line.
    if [ -f /run/janus/haproxy.pid ]; then
        kill "$(cat /run/janus/haproxy.pid)" 2>/dev/null || true
    fi
    rm -rf "$WORKDIR"
}
trap cleanup EXIT

# janusd keeps uploaded certificates next to -haproxy-config: start from an
# empty store.
mkdir -p "$WORKDIR/maps" "$WORKDIR/acls" "$WORKDIR/certs"
echo "www.example.com backend1" > "$WORKDIR/maps/hosts.map"
echo "1.2.3.4" > "$WORKDIR/acls/blocked.acl"

selfsigned() { # <cn> <key> <crt>
    openssl req -x509 -newkey ed25519 -keyout "$2" -out "$3" -days 1 -nodes -subj "/CN=$1" 2>/dev/null
}
selfsigned default.local "$WORKDIR/certs/default.key" "$WORKDIR/certs/default.crt"
cat "$WORKDIR/certs/default.crt" "$WORKDIR/certs/default.key" > "$WORKDIR/certs/default.pem"
echo "$WORKDIR/certs/default.pem" > "$WORKDIR/certs/crt-list.txt"

cat > "$WORKDIR/haproxy.cfg" <<EOF
global
    stats socket /run/janus/haproxy-admin.sock mode 660 level admin
defaults
    mode http
    timeout connect 5s
    timeout client 30s
    timeout server 30s
frontend janus-health
    bind *:8080
    acl blocked src -f $WORKDIR/acls/blocked.acl
    http-request deny if blocked
    http-request set-header X-Mapped %[req.hdr(host),map($WORKDIR/maps/hosts.map,default)]
    http-request return status 200 content-type text/plain string "up\n"
frontend janus-tls
    bind *:8443 ssl crt-list $WORKDIR/certs/crt-list.txt
    http-request return status 200 content-type text/plain string "tls-up\n"
EOF

# A separate -pki-dir, possibly left over by a previous run on a
# persistent runner: start from scratch.
sudo rm -rf /etc/janus/pki-rt
sudo mkdir -p /run/janus /etc/haproxy /etc/janus
sudo chown -R "$(id -u):$(id -g)" /run/janus /etc/haproxy /etc/janus
sudo mkdir -p /var/empty && sudo chmod 000 /var/empty

# janusd starts HAProxy with this exact config - no need to apply it over
# the API afterward (doing so raced the seamless reload against the
# runtime-API calls below once: calls landed on the old, draining
# process).
"$JANUSD" -addr ":$P_GRPC" -haproxy-binary "$HAPROXY" \
    -pki-dir /etc/janus/pki-rt -haproxy-config "$WORKDIR/haproxy.cfg" \
    > "$WORKDIR/janusd.log" 2>&1 &
JANUSD_PID=$!

for _ in $(seq 1 50); do
    grep -q "listening on" "$WORKDIR/janusd.log" && break
    sleep 0.2
done
grep -q "listening on" "$WORKDIR/janusd.log" || { echo "janusd never listened:" >&2; cat "$WORKDIR/janusd.log" >&2; exit 1; }

ctl() {
    "$CTL_BIN" -endpoint "127.0.0.1:$P_GRPC" -ca /etc/janus/pki-rt/ca.crt \
        -cert /etc/janus/pki-rt/admin.crt -key /etc/janus/pki-rt/admin.key "$@"
}
fail() { echo "haproxy-runtime-test: $*" >&2; cat "$WORKDIR/janusd.log" >&2; exit 1; }

for _ in $(seq 1 50); do
    ctl haproxy show-info > /dev/null 2>&1 && break
    sleep 0.2
done

[ "$(ctl haproxy map-list)" = "$WORKDIR/maps/hosts.map" ] || fail "map-list doesn't show the file-backed map"
ctl haproxy map-set "$WORKDIR/maps/hosts.map" www.new.com backendX
ctl haproxy map-get "$WORKDIR/maps/hosts.map" | grep -q "^www.new.com backendX$" || fail "map-set didn't add the entry"
ctl haproxy map-delete "$WORKDIR/maps/hosts.map" www.new.com
if ctl haproxy map-get "$WORKDIR/maps/hosts.map" | grep -q www.new.com; then
    fail "map-delete left the entry"
fi

ctl haproxy acl-add "$WORKDIR/acls/blocked.acl" 5.6.7.8
ctl haproxy acl-delete "$WORKDIR/acls/blocked.acl" 5.6.7.8

selfsigned rt.example.com "$WORKDIR/rt.key" "$WORKDIR/rt.crt"
cat "$WORKDIR/rt.crt" "$WORKDIR/rt.key" > "$WORKDIR/rt-bundle.pem"
ctl haproxy cert-upload "$WORKDIR/uploaded.pem" "$WORKDIR/rt-bundle.pem"
ctl haproxy cert-list | grep -q "^$WORKDIR/uploaded.pem.*status=Unused" || fail "an unbound uploaded cert isn't listed Unused"
ctl haproxy cert-delete "$WORKDIR/uploaded.pem"

# The crt-list binding: upload+bind with an SNI filter, HAProxy serves it
# only for that SNI (no SNI still gets the seeded default), then
# unbind+delete.
selfsigned uploaded.local "$WORKDIR/certs/uploaded.key" "$WORKDIR/certs/uploaded.crt"
cat "$WORKDIR/certs/uploaded.crt" "$WORKDIR/certs/uploaded.key" > "$WORKDIR/certs/uploaded.pem"
ctl haproxy cert-upload -crt-list "$WORKDIR/certs/crt-list.txt" -sni uploaded.local \
    "$WORKDIR/certs/uploaded.pem" "$WORKDIR/certs/uploaded.pem"
ctl haproxy cert-list | grep -q "^$WORKDIR/certs/uploaded.pem.*status=Used" || fail "the bound cert isn't listed Used"

subject() { openssl s_client -connect 127.0.0.1:8443 "$@" </dev/null 2>/dev/null | grep subject= || true; }
got="$(subject -servername uploaded.local)"
[ "$got" = "subject=CN=uploaded.local" ] || fail "expected the SNI-bound cert, got: $got"
got="$(subject)"
[ "$got" = "subject=CN=default.local" ] || fail "expected the default cert with no SNI, got: $got"

# A reload starts a new HAProxy from the configuration: janusd puts the
# uploaded certificate and its crt-list binding back.
ctl haproxy apply-config "$WORKDIR/haproxy.cfg"
ctl haproxy cert-list | grep -q "^$WORKDIR/certs/uploaded.pem.*status=Used" || fail "the bound cert is gone after a reload"
got="$(subject -servername uploaded.local)"
[ "$got" = "subject=CN=uploaded.local" ] || fail "expected the SNI-bound cert after a reload, got: $got"

ctl haproxy cert-delete -crt-list "$WORKDIR/certs/crt-list.txt" "$WORKDIR/certs/uploaded.pem"
if ctl haproxy cert-list | grep -q "^$WORKDIR/certs/uploaded.pem"; then
    fail "the cert is still listed after delete"
fi

# What the Controller and the exporter read: show info and show stat
# parse on this branch.
ctl haproxy show-info | grep -q "$("$HAPROXY" -v | head -1 | awk '{print $3}')" || fail "show-info doesn't report this HAProxy's version"
ctl haproxy backends > /dev/null || fail "show stat doesn't parse"

echo "haproxy-runtime-test: OK"
