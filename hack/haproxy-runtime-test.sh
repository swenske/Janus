#!/usr/bin/env bash
# Proves janusd's HAProxy runtime API code (internal/haproxy/
# runtime_maps.go, runtime_certs.go) against a real HAProxy binary -
# file-backed map/ACL entries and real uploaded certificates, not just
# the fixtures captured in internal/haproxy/*_test.go - that a reader
# gets the configuration with its secrets masked (redact.go), and the
# runtime diagnostics (runtime_tables.go, runtime_sessions.go,
# runtime_errors.go): a stick table filled by real requests and cleared,
# a stream held by a server that never answers and shut down, an invalid
# request carrying credentials, map/ACL lookups. Run once per HAProxy
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
# The diagnostics' frontend, and the server behind it that never answers.
P_DIAG="$((18420 + ${JANUS_TEST_PORT_OFFSET:-0}))"
P_SILENT="$((18421 + ${JANUS_TEST_PORT_OFFSET:-0}))"

echo "haproxy-runtime-test: $("$HAPROXY" -v | head -1)"

WORKDIR="$(mktemp -d)"
JANUSD_PID=""
SILENT_PID=""
# gone PID: waits up to 10 s for PID to exit.
gone() {
    for _ in $(seq 1 50); do
        kill -0 "$1" 2>/dev/null || return 0
        sleep 0.2
    done
    echo "haproxy-runtime-test: pid $1 still running" >&2
}
cleanup() {
    # janusd's own pid file for the HAProxy it started - never pkill -f a
    # path that is also on this script's command line.
    local haproxy_pid=""
    [ -f /run/janus/haproxy.pid ] && haproxy_pid="$(cat /run/janus/haproxy.pid)"
    [ -n "$JANUSD_PID" ] && kill "$JANUSD_PID" 2>/dev/null || true
    [ -n "$SILENT_PID" ] && kill "$SILENT_PID" 2>/dev/null || true
    [ -n "$haproxy_pid" ] && kill "$haproxy_pid" 2>/dev/null || true
    # image-build runs this once per HAProxy branch, back to back, on the
    # same ports: the next run's janusd failed to listen while this one
    # was still exiting (v2026.10.18-3's first build).
    [ -n "$JANUSD_PID" ] && gone "$JANUSD_PID"
    [ -n "$haproxy_pid" ] && gone "$haproxy_pid"
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
userlist ops
    user rt insecure-password s3cret-rt
backend stats-auth
    stats auth admin:hunter2-rt
frontend diag
    bind 127.0.0.1:$P_DIAG
    stick-table type ip size 1k expire 10m store http_req_rate(10s),gpc0
    http-request track-sc0 src
    http-request return status 200 content-type text/plain string "ok\n" if { path /fill }
    default_backend silent
backend silent
    server never 127.0.0.1:$P_SILENT
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

# The configuration: whole for an admin, its secrets masked for a reader
# (JANUS-2026-006) - which HAProxy itself still accepts as written.
ctl haproxy get-config | grep -q "insecure-password s3cret-rt" || fail "an admin doesn't get the configuration whole"
ctl pki generate-client-config -role os:reader -name rt-reader -ttl 1h "$WORKDIR/reader" > /dev/null || fail "generate-client-config failed"
reader() { "$CTL_BIN" -endpoint "127.0.0.1:$P_GRPC" -ca "$WORKDIR/reader/ca.crt" -cert "$WORKDIR/reader/client.crt" -key "$WORKDIR/reader/client.key" "$@"; }
masked="$(reader haproxy get-config 2> "$WORKDIR/reader.err")" || fail "a reader can't read the configuration"
if grep -q -e s3cret-rt -e hunter2-rt <<<"$masked"; then fail "a reader got a secret: $masked"; fi
grep -q "insecure-password <redacted>" <<<"$masked" || fail "the userlist password isn't masked: $masked"
grep -q "stats auth admin:<redacted>" <<<"$masked" || fail "the stats auth password isn't masked: $masked"
grep -q "secrets masked" "$WORKDIR/reader.err" || fail "janusctl doesn't say the configuration is masked"
[ "$(wc -l <<<"$masked")" = "$(ctl haproxy get-config | wc -l)" ] || fail "masking moved lines"
printf '%s\n' "$masked" > "$WORKDIR/masked.cfg"
"$HAPROXY" -c -q -f "$WORKDIR/masked.cfg" || fail "HAProxy refuses the masked configuration"

# --- runtime diagnostics ---
# Lookups: HAProxy's own matching - the map is read as str, the ACL as ip.
ctl haproxy map-lookup "$WORKDIR/maps/hosts.map" www.example.com | grep -q "^value    backend1$" || fail "map-lookup didn't find www.example.com"
if ctl haproxy map-lookup "$WORKDIR/maps/hosts.map" nothing.test > /dev/null; then fail "map-lookup found a key the map hasn't"; fi
out="$(ctl haproxy acl-lookup "$WORKDIR/acls/blocked.acl" 1.2.3.4 2>&1)" || true
grep -q "^method   ip$" <<<"$out" || fail "acl-lookup didn't match 1.2.3.4 as an address: $out"

# A server that accepts connections and never answers: a request to it
# stays a stream until it's shut down.
python3 -c 'import socket, sys, time; s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("127.0.0.1", int(sys.argv[1]))); s.listen(8); time.sleep(300)' "$P_SILENT" &
SILENT_PID=$!
sleep 0.5
for _ in 1 2 3; do curl -s -o /dev/null --max-time 5 "http://127.0.0.1:$P_DIAG/fill" || fail "diag doesn't answer"; done
curl -s -o /dev/null --max-time 60 "http://127.0.0.1:$P_DIAG/held" &
CURL_PID=$!
sleep 1

ctl haproxy tables | grep -qE "^diag[[:space:]]+ip[[:space:]]+1[[:space:]]+1024$" || fail "tables doesn't list diag with its entry: $(ctl haproxy tables)"
ctl haproxy table diag | grep -qE "^127\.0\.0\.1[[:space:]]" || fail "table diag lacks the client"
ctl haproxy table -filter http_req_rate:gt:2 diag | grep -q "^127\.0\.0\.1" || fail "a filter on the request rate missed the client"
if ctl haproxy table -filter http_req_rate:gt:1000 diag | grep -q "^127\.0\.0\.1"; then fail "a filter let a client through it shouldn't"; fi
if ctl haproxy table nosuch 2> "$WORKDIR/err"; then fail "table nosuch didn't fail"; fi
grep -q "NotFound" "$WORKDIR/err" || fail "table nosuch: $(cat "$WORKDIR/err")"

held="$(ctl haproxy sessions -frontend diag)"
id="$(awk '$5 == "never" {print $1; exit}' <<<"$held")"
[ -n "$id" ] || fail "sessions doesn't show the held request: $held"
if ctl haproxy sessions | grep -q GLOBAL; then fail "sessions lists the runtime API's own stream"; fi
ctl haproxy session-kill "$id"
for _ in $(seq 1 20); do kill -0 "$CURL_PID" 2>/dev/null || break; sleep 0.2; done
if kill -0 "$CURL_PID" 2>/dev/null; then fail "the held request is still running after session-kill"; fi
# A stream's ID is its address: once it has ended, HAProxy may give that
# address to the next stream - often the runtime API connection of the next
# command, which then shuts itself down "successfully" (3.2 did, once) - so
# killing the same ID again isn't sure to fail. An address no stream has is.
if ctl haproxy session-kill 0x1 2> "$WORKDIR/err"; then fail "session-kill shut down a stream that doesn't exist"; fi
grep -q "NotFound" "$WORKDIR/err" || fail "session-kill 0x1: $(cat "$WORKDIR/err")"

ctl haproxy table-clear diag -key 127.0.0.1
if ctl haproxy table diag | grep -q "^127\.0\.0\.1"; then fail "table-clear left the entry"; fi
if ctl haproxy table-clear diag 2>/dev/null; then fail "table-clear with neither key, filter nor -all cleared"; fi

# An invalid request (a space in a header name) carrying credentials.
exec 3<>"/dev/tcp/127.0.0.1/$P_DIAG"
printf 'GET /x HTTP/1.1\r\nHost: diag\r\nAuthorization: Basic c2VjcmV0OnBhc3M=\r\nCookie: sid=s3cret\r\nBad Header: x\r\n\r\n' >&3
timeout 2 cat <&3 > /dev/null || true
exec 3>&-
errs="$(ctl haproxy errors -proxy diag)"
grep -q "frontend diag: invalid request" <<<"$errs" || fail "errors doesn't show the invalid request: $errs"
grep -qF 'Authorization: <redacted>\r\n' <<<"$errs" || fail "errors doesn't mask Authorization: $errs"
if grep -q -e c2VjcmV0 -e s3cret <<<"$errs"; then fail "errors shows a credential: $errs"; fi
grep -q "^ *\^$" <<<"$errs" || fail "errors doesn't point at the refused byte: $errs"

# What a reader may: look, not change, not read refused requests' bytes.
reader haproxy tables > /dev/null || fail "a reader can't list the stick tables"
reader haproxy sessions > /dev/null || fail "a reader can't list the streams"
if reader haproxy errors 2>/dev/null; then fail "a reader read the captured errors"; fi
if reader haproxy table-clear diag -all 2>/dev/null; then fail "a reader cleared a stick table"; fi

# What the Controller and the exporter read: show info and show stat
# parse on this branch.
ctl haproxy show-info | grep -q "$("$HAPROXY" -v | head -1 | awk '{print $3}')" || fail "show-info doesn't report this HAProxy's version"
ctl haproxy backends > /dev/null || fail "show stat doesn't parse"

echo "haproxy-runtime-test: OK"
