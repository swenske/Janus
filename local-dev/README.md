# Janus local dev container

A real `janusd` + `haproxy` pair, running as an ordinary Docker
container, for fast local iteration on HAProxy config and the janusd/
gRPC API - no QEMU, no squashfs/dm-verity assembly, no real deployment.
Built out of the exact same `build/janusd`/`build/haproxy` binaries
every other Janus target uses (`daemon-static`/`haproxy-build`), just
run directly instead of packaged into the immutable appliance.

**This is not a target-OS artifact** - see `local-dev/Dockerfile`'s own
header for why it lives here, next to `dashboard/`, rather than under
`image/`.

## Build

```sh
make local-dev-image   # from the repo root - builds build/janusd + build/haproxy, then local-dev/Dockerfile
```

## Run

`janusd` needs no container-specific flags at all - every one of its
own defaults (`-pki-dir`, `-haproxy-config`, `-haproxy-chroot-dir`, ...)
already matches this image's own layout, and it creates every directory
it needs itself, exactly like a real node's first boot.

```sh
docker run -d --name janus-local-dev --network host janus-local-dev
```

`--network host` is the simplest option here specifically because
you'll usually be testing an HAProxy config with a listener port that
isn't known ahead of time (the whole point of this tool) - no need to
guess which port to `-p` map before you've even written the config.
If you're not on Linux (Docker Desktop on macOS/Windows doesn't support
`--network host` well), map the ports you actually need instead:

```sh
docker run -d --name janus-local-dev \
  -p 9505:9505 -p 8080:8080 \
  janus-local-dev
```

(`9505` is janusd's own gRPC/mTLS port; `8080` is the bootstrap
`haproxy.cfg`'s health-check frontend - add `-p <port>:<port>` for
whatever port your own `ApplyConfig` call binds to.)

PKI and the applied HAProxy config live in the container's own
filesystem, not a volume - a fresh container means a fresh CA/admin
cert, same as any other first boot. Mount `/etc/janus/pki` and
`/etc/haproxy` yourself if you want them to survive a container
restart:

```sh
docker run -d --name janus-local-dev --network host \
  -v janus-local-dev-pki:/etc/janus/pki \
  -v janus-local-dev-haproxy:/etc/haproxy \
  janus-local-dev
```

## Using it

`janusd` prints its bootstrapped PKI to the container's own logs on
first run, exactly like a real node:

```sh
docker logs janus-local-dev
# copy the CA cert / admin cert / admin key printed there into local files
```

Then drive it with `janusctl` exactly as you would a real node:

```sh
bin/janusctl -ca ca.crt -cert admin.crt -key admin.key -endpoint 127.0.0.1:9505 version
bin/janusctl -ca ca.crt -cert admin.crt -key admin.key -endpoint 127.0.0.1:9505 haproxy apply-config myconfig.cfg
curl http://127.0.0.1:8080/whatever-your-config-exposes
```

## Verification

Built and run for real before being documented here, not assumed: a
real container start (PKI bootstrap logged, `haproxy`'s own `[NOTICE]`
startup line present), a real `janusctl version` round trip over mTLS,
and a real `ApplyConfig` call applying a config that uses
`http-request use-service prometheus-exporter` - the exact config that
was rejected on a real deployed alpha node before `pkgs/haproxy/
Dockerfile` gained `USE_PROMEX=1` (see that file's own comment) -
confirmed accepted, and `/metrics` confirmed serving real Prometheus
output through the container's port mapping.
