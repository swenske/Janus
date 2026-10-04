#!/usr/bin/env bash
# The Controller updating itself, with real Docker Compose, set up the way
# dashboard/README.md ("Updating the Controller") says:
#
#   - three Controller images in a local registry: A (running), B (the next
#     release) and C (a release that never starts);
#   - a fake GitHub releases API saying which one is newest, with its
#     controller-image.txt (tag@digest), as image-build.yml publishes it;
#   - the documented compose.yaml (janus-controller + janus-controller-
#     updater), first without the Compose directory mounted in the updater
#     - which must say so - then complete.
#
# Then, through the Controller's own API, as its page does: A -> B must
# succeed (.env pinned to B's digest, data and TLS identity kept, the same
# password signs in), and B -> C must roll back by itself (C never says
# it started): B again, its .env and data back.
#
# Needs Docker and python3; pulls registry:2. KEEP=1
# leaves everything running (to look at the page), its variables in
# $WORK/test-env.sh.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
PROJECT=janus-self-update-$$
PASSWORD=self-update-test-password
fail() { echo "FAIL: $*" >&2; FAILED=1; exit 1; }

free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
REG_PORT=$(free_port)
GH_PORT=$(free_port)
MAIN_PORT=$(free_port)
REGISTER_PORT=$(free_port)
REPO=localhost:$REG_PORT/janus-controller
A=v2026.10.01-50
B=v2026.10.01-51
C=v2026.10.01-52

# Driven with the Compose binary the image ships (the updater's own), not
# whatever the host has.
compose() { "$WORK/docker-compose" -p "$PROJECT" --project-directory "$WORK" -f "$WORK/compose.yaml" "$@"; }
cleanup() {
	set +e
	if [ -n "${KEEP:-}" ]; then
		declare -p WORK PROJECT REPO REG_PORT GH_PORT GH_PID MAIN_PORT REGISTER_PORT PASSWORD A B C > "$WORK/test-env.sh"
		echo "kept: $WORK (variables in $WORK/test-env.sh), Compose project $PROJECT"
		return
	fi
	compose logs --no-color > "$WORK/compose.log" 2>&1
	[ -n "${FAILED:-}" ] && tail -60 "$WORK/compose.log"
	compose down -v --remove-orphans >/dev/null 2>&1
	docker rm -f "$PROJECT-registry" >/dev/null 2>&1
	kill "$GH_PID" 2>/dev/null
	# Every reference into the test's registry, tag or digest (never by
	# image ID: that would untag janus-controller too if it's the same).
	docker images --digests --format '{{.Repository}} {{.Tag}} {{.Digest}}' |
		awk -v r="$REPO" '$1 == r { if ($2 != "<none>") print $1 ":" $2; if ($3 != "<none>") print $1 "@" $3 }' |
		xargs -r docker rmi >/dev/null 2>&1
	rm -rf "$WORK"
}
trap 'FAILED=1; cleanup' ERR
trap cleanup EXIT

echo "== registry, three Controller images"
docker run -d --name "$PROJECT-registry" -p "127.0.0.1:$REG_PORT:5000" registry:2 >/dev/null
# Tagged straight into the test's registry: never janus-controller, the
# image image-build.yml goes on to publish.
for v in "$A" "$B" "$C"; do
	make -s -C "$ROOT" dashboard-image VERSION="$v" DASHBOARD_IMAGE="$REPO:$v" > "$WORK/build-$v.log" 2>&1 || { tail -30 "$WORK/build-$v.log"; fail "building $v"; }
done
id=$(docker create "$REPO:$A")
docker cp "$id:/usr/local/bin/docker-compose" "$WORK/docker-compose"
docker rm "$id" >/dev/null
# C is a release whose Controller never comes up: it can't listen.
mkdir -p "$WORK/broken"
printf 'FROM %s\nENTRYPOINT ["/dashboardd", "-addr", "256.0.0.1:443"]\n' "$REPO:$C" > "$WORK/broken/Dockerfile"
docker build -q -t "$REPO:$C" "$WORK/broken" >/dev/null
for v in "$A" "$B" "$C"; do docker push -q "$REPO:$v" >/dev/null; done
digest_of() { docker inspect --format '{{index .RepoDigests 0}}' "$REPO:$1" | sed 's/.*@//'; }
B_IMAGE="$REPO:$B@$(digest_of "$B")"
C_IMAGE="$REPO:$C@$(digest_of "$C")"
# The updater has to pull B and C itself.
docker rmi "$REPO:$B" "$REPO:$C" >/dev/null

echo "== fake releases API"
mkdir -p "$WORK/gh"
publish() { # publish VERSION IMAGE: VERSION is the newest release
	echo "$2" > "$WORK/gh/controller-image-$1.txt"
	cat > "$WORK/gh/releases" <<EOF
[{"tag_name": "$1", "html_url": "https://github.com/swenske/Janus/releases/tag/$1", "published_at": "2026-10-02T08:00:00Z",
  "assets": [{"name": "controller-image.txt", "browser_download_url": "http://127.0.0.1:$GH_PORT/controller-image-$1.txt"}]}]
EOF
}
publish "$B" "$B_IMAGE"
python3 -m http.server --bind 127.0.0.1 --directory "$WORK/gh" "$GH_PORT" >/dev/null 2>&1 &
GH_PID=$!

echo "== compose project (the updater without the Compose directory first)"
write_compose() { # write_compose MOUNT_LINE
	cat > "$WORK/compose.yaml" <<EOF
services:
  janus-controller:
    image: \${JANUS_CONTROLLER_IMAGE:-$REPO:$A}
    container_name: $PROJECT-controller
    network_mode: host
    restart: unless-stopped
    command: ["-register-addr", "127.0.0.1:$REGISTER_PORT"]
    environment:
      JANUS_CONTROLLER_ADDR: "127.0.0.1:$MAIN_PORT"
      JANUS_CONTROLLER_RELEASES_URL: "http://127.0.0.1:$GH_PORT/releases"
    volumes:
      - janus-controller-data:/data
      - janus-controller-updater:/run/janus-updater

  janus-controller-updater:
    image: \${JANUS_CONTROLLER_IMAGE:-$REPO:$A}
    container_name: $PROJECT-updater
    entrypoint: ["/janus-controller-updater"]
    network_mode: none
    restart: unless-stopped
    environment:
      JANUS_UPDATER_REPOSITORY: "$REPO"
      JANUS_UPDATER_START_TIMEOUT: "45s"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
$1
      - janus-controller-data:/data
      - janus-controller-updater:/run/janus-updater
      - janus-controller-updater-state:/var/lib/janus-updater

volumes:
  janus-controller-data:
  janus-controller-updater:
  janus-controller-updater-state:
EOF
}
write_compose ""
# An operator's .env, with a comment and another setting to keep.
printf '# Janus Controller\nTZ=Europe/Paris\n' > "$WORK/.env"
compose up -d --quiet-pull

JAR=$WORK/cookies
api() { curl -sk -b "$JAR" -c "$JAR" "$@"; }
login() { api -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d "{\"password\":\"$PASSWORD\"}" "https://127.0.0.1:$MAIN_PORT/api/auth/login"; }
wait_for() { # wait_for SECONDS DESCRIPTION FUNCTION
	local deadline=$((SECONDS + $1))
	until "$3"; do
		[ "$SECONDS" -lt "$deadline" ] || fail "timed out waiting for $2"
		sleep 2
	done
}
controller_up() { curl -sk -o /dev/null "https://127.0.0.1:$MAIN_PORT/api/auth/status"; }
wait_for 60 "the Controller" controller_up
api -o /dev/null -H 'Content-Type: application/json' -d "{\"password\":\"$PASSWORD\",\"mfa_required\":\"nobody\"}" "https://127.0.0.1:$MAIN_PORT/api/auth/setup"
[ "$(login)" = 204 ] || fail "can't sign in"
identity() { echo | openssl s_client -connect "127.0.0.1:$MAIN_PORT" 2>/dev/null | openssl x509 -noout -fingerprint -sha256; }
IDENTITY=$(identity)

status() { api "https://127.0.0.1:$MAIN_PORT/api/controller/update"; }
field() { python3 -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }

echo "== the updater says what's missing"
reachable() { status | field 'd["updater"]["reachable"]' | grep -qx True; }
wait_for 60 "the updater to answer" reachable
S=$(status)
[ "$(field 'd["version"]' <<<"$S")" = "$A" ] || fail "version: $S"
[ "$(field 'd["update_available"] and d["latest"]["version"] == "'"$B"'" and d["latest"]["image"] == "'"$B_IMAGE"'"' <<<"$S")" = True ] || fail "no update offered: $S"
[ "$(field 'd["updater"]["ready"]' <<<"$S")" = False ] || fail "ready without the Compose directory: $S"
grep -q "mount the Compose directory at the same path: $WORK:$WORK" <<<"$S" || fail "the problem isn't explained: $S"
code=$(api -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d "{\"version\":\"$B\"}" "https://127.0.0.1:$MAIN_PORT/api/controller/update")
[ "$code" = 412 ] || fail "an update was accepted with the setup incomplete ($code)"
echo "ok: not ready, and why"

write_compose "      - $WORK:$WORK"
compose up -d --quiet-pull janus-controller-updater
ready() { status | field 'd["updater"].get("ready")' | grep -qx True; }
wait_for 60 "the updater to be ready" ready
echo "ok: ready once the Compose directory is mounted"

echo "== $A -> $B"
code=$(api -o "$WORK/post" -w '%{http_code}' -H 'Content-Type: application/json' -d "{\"version\":\"$B\"}" "https://127.0.0.1:$MAIN_PORT/api/controller/update")
[ "$code" = 202 ] || fail "update refused ($code): $(cat "$WORK/post")"
# The Controller restarts (sessions are in memory: sign in again), then
# reports the job's end.
job_ended() { # job_ended STATE VERSION
	[ "$(login 2>/dev/null)" = 204 ] || return 1
	local s
	s=$(status) || return 1
	[ "$(field 'd["version"]' <<<"$s" 2>/dev/null)" = "$2" ] && [ "$(field 'd["updater"]["job"]["state"]' <<<"$s" 2>/dev/null)" = "$1" ]
}
updated() { job_ended done "$B"; }
wait_for 300 "the update to $B" updated
S=$(status)
grep -qx "JANUS_CONTROLLER_IMAGE=$B_IMAGE" "$WORK/.env" || fail ".env: $(cat "$WORK/.env")"
grep -qx 'TZ=Europe/Paris' "$WORK/.env" && grep -qx '# Janus Controller' "$WORK/.env" || fail ".env lost its other lines: $(cat "$WORK/.env")"
[ "$(stat -c %U "$WORK/.env")" = "$(id -un)" ] || fail ".env changed owner"
[ "$(compose ps --format '{{.Image}}' janus-controller)" = "$B_IMAGE" ] || fail "running $(compose ps --format '{{.Image}}' janus-controller)"
[ "$(identity)" = "$IDENTITY" ] || fail "the TLS identity changed - /data wasn't kept"
[ "$(field 'd["update_available"]' <<<"$S")" = False ] || fail "still offered an update: $S"
echo "ok: $B runs, pinned to its digest; .env, data, identity and password kept"

echo "== $B -> $C (never starts)"
publish "$C" "$C_IMAGE"
compose restart janus-controller >/dev/null # forget the cached release
wait_for 60 "the Controller" controller_up
signed_in() { [ "$(login)" = 204 ]; }
wait_for 30 "signing in" signed_in
offered() { status | field 'd["latest"]["version"]' | grep -qx "$C"; }
wait_for 30 "$C to be offered" offered
code=$(api -o "$WORK/post" -w '%{http_code}' -H 'Content-Type: application/json' -d "{\"version\":\"$C\"}" "https://127.0.0.1:$MAIN_PORT/api/controller/update")
[ "$code" = 202 ] || fail "update refused ($code): $(cat "$WORK/post")"
rolled_back() { job_ended rolled-back "$B"; }
wait_for 300 "the rollback to $B" rolled_back
S=$(status)
grep -q "$C didn't start within 45s" <<<"$S" || fail "rollback reason: $S"
grep -qx "JANUS_CONTROLLER_IMAGE=$B_IMAGE" "$WORK/.env" || fail ".env not restored: $(cat "$WORK/.env")"
[ "$(compose ps --format '{{.Image}}' janus-controller)" = "$B_IMAGE" ] || fail "running $(compose ps --format '{{.Image}}' janus-controller) after the rollback"
[ "$(identity)" = "$IDENTITY" ] || fail "the TLS identity changed across the rollback"
echo "ok: $C rolled back by itself - $B, its .env and data are back"

echo "PASS: the Controller updates itself, and rolls back"
