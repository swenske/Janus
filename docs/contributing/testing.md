# Testing

A Janus feature is done when it's proven on a **real boot**: an image
booted under UEFI (OVMF), SELinux enforcing with no denial, real HAProxy,
real HTTP and gRPC over mutual TLS - not mocks. Logic that can be pure
is kept free of the system, so it gets ordinary unit tests too.

## The two layers

| | Runs | Where |
|---|---|---|
| **Unit tests** - `go test ./...` | Parsers, planners, state machines, the Controller's handlers, the provider's client, the docs' anchors and examples | `ci.yml`, on every push and pull request, GitHub-hosted |
| **System tests** - `make <test>` | Images booted in QEMU, a real Controller, a real libvirt host, real routers' daemons | `image-build.yml`, on the self-hosted runners, dispatched by hand |

```sh
make test       # go test, the control plane
make lint       # golangci-lint
make vet
```

## System tests

Most need QEMU with OVMF, some Docker or `sudo`; each builds what it
boots. The catalog:

| Area | Tests |
|---|---|
| The boot chain | `qemu-boot-test`, `qemu-network-test`, `qemu-hardening-test`, `qemu-verity-boot-test`, `qemu-state-persist-test`, `qemu-selinux-test`, `qemu-ab-boot-test`, `qemu-uefi-boot-test`, `qemu-uefi-ab-boot-test`, `qemu-secureboot-test`, `qemu-baremetal-test` |
| Lifecycle | `qemu-lifecycle-rollback-test`, `qemu-lifecycle-upgrade-test` (and its `-url-`, `-https-`, `-relay-`, `-health-` variants), `lifecycle-install-test`, `qemu-iso-boot-test`, `qemu-iso-install-test`, `qemu-pxe-fetch-test` |
| Provisioning | `qemu-self-register-test`, `seed-controller-test`, `nocloud-seed-test`, `qemu-network-config-test` |
| The API and the features | `qemu-system-api-test`, `qemu-system-info-test`, `qemu-packet-capture-test`, `qemu-metrics-test`, `qemu-fleet-trust-test`, `qemu-fleetctl-test`, `qemu-extensions-test`, `qemu-firewall-test`, `qemu-vrrp-test`, `qemu-bgp-test`, `qemu-acme-test`, `qemu-consul-test` |
| The Controller | `qemu-dashboard-test`, `controller-self-update-test`, `controller-libvirt-test`, `terraform-provider-test` |
| arm64 | `qemu-raspi4-boot-test`, `qemu-raspi4-daemon-test`, `qemu-arm64-network-test`, `qemu-arm64-uefi-boot-test`, `pi4-sdcard-image-test`, `pi5-sdcard-image-test` |
| The docs | `docs-build`, `docs-smoke`, `examples-check`, `examples-test` ([writing docs](writing-docs.md)) |

Several runners share a host: a test's ports are offset by
`JANUS_TEST_PORT_OFFSET`, one per runner instance.

## SELinux: every rule from a real denial

The policy (`selinux/policy.conf`) is written by hand, and every rule in
it answers a denial seen on an enforcing boot - none is guessed. A change
to what a daemon does - a new socket family, a new file under `/proc` or
`/sys`, a device - needs `make qemu-selinux-test`, plus the feature's own
enforcing test, run before pushing. `sysctl.kernel.printk_ratelimit=0`
on the test's command line shows every denial.

## Habits the tests rely on

- **Credentials** come from the disk's STATE (`debugfs`), or from the
  console for the ISO: a node prints them once.
- **Wait for janusd**, not for HTTP 200: HAProxy starts before the PKI,
  so a node answers HTTP before its API listens - wait for `listening
  on`.
- **After an upgrade or a rollback**, the old system answers for about
  two seconds: check the HTTP answer and the boot count together.
- **The guest's clock** under KVM comes from kvmclock, not `-rtc base=`:
  `-cpu qemu64,-kvmclock` to fake one.
- **QEMU acceleration**: `-accel kvm -accel tcg` - KVM where it's there,
  TCG otherwise.

## Shell traps already hit

- `grep -q ... && { ...; exit 1; }` as a statement under `set -e` aborts
  on the success path: use `if`.
- `$(...)` strips trailing newlines: build JSON bodies from files.
- `echo "$big" | grep -q` under `pipefail` can fail on SIGPIPE: use a
  here-string.
- `wait_for "msg" test "$(f)" = x` expands `$(f)` once: poll a function.
- `curl`'s `%{http_code}` `000` means no response at all, not a refused
  handshake.
- A reused scratch directory may hold a stale `console.log`.
- `httptest.NewTLSServer` reuses one certificate per process: generate
  an unrelated one for "wrong CA" tests.
- Never `pkill -f`/`pgrep -f` a pattern that appears in the same shell
  command - it kills the shell: use pid files.
