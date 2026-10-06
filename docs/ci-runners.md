# CI runners

`ci.yml` runs on GitHub-hosted runners (it must accept fork pull
requests). Everything that builds or boots images - `image-build.yml`,
`schematic-build.yml`, `site-deploy.yml` - runs on this project's own
self-hosted runners, labeled `self-hosted, docker, janus`.

## How image-build runs

- **Six test jobs at once** (`test-boot`, `test-lifecycle`, `test-api`,
  `test-network`, `test-services`, `test-hardware`), balanced by
  duration. Each builds what its tests need: the Docker build cache makes
  that a matter of seconds, and nothing has to pass between jobs.
- **`publish`** (images, workflow artifacts, the Docker Hub push, the
  release) needs all six. A failing test still blocks everything that
  gets published.
- **Shared setup** is a composite action, `.github/actions/runner-setup`:
  Go (setup-go without its GitHub cache - the runners keep Go's caches on
  disk), and the QEMU and image tools. Tools are installed only when
  missing, under a lock, since several jobs share a machine.
- **KVM**: every x86 test asks for it and falls back to emulation
  (`-accel kvm -accel tcg`). A UEFI boot to HAProxy answering takes about
  4 s with KVM against 13 s emulated. arm64 guests are always emulated.
- **Tests that share host paths** (the native janusd and HAProxy ones)
  are all in `test-api`, so two of them never run at once.
- **`docs-screenshots`** takes the docs' screenshots of the Controller
  again (`make docs-screenshots-check`) and reports, never blocks:
  `publish` doesn't need it, a drift is a warning and an artifact. Its
  Docker network is a fixed `10.0.10.0/24`, so a host runs one at a
  time (a lock in `/tmp`).
- **No Node.js on the runners**: the tests build the Controller from its
  committed frontend (`make dashboard-bin`, Go only), and the docs site
  (`site/docs/Dockerfile`) and the browser image (`hack/browser`)
  install their npm packages in their own pinned images - never as the
  runner's user, which holds the publishing keys.
- **What gets published is built only on trusted runners**: `publish`,
  `schematic-build.yml` (images users download, signed) and
  `site-deploy.yml` also need the label `janus-publish`. A runner without
  it only ever runs test jobs: at worst a problem on it fails a test.

A full run takes about 6 minutes, against 25 when it was one job,
emulated.

## Runner machines

`janus-runner01` and `janus-runner02` are unprivileged LXC containers on
Proxmox hosts; `janus-runner03` is a laptop running Debian 13 itself.
Each machine runs several runner instances - one job each - named
`<machine>`, `<machine>-2`, `<machine>-3`...

| Machine | Instances | Labels | Size |
|---|---|---|---|
| `janus-runner01` | 5 | `docker`, `janus`, `janus-publish` | 12 cores, 24 GB, 64 GB disk |
| `janus-runner02` | 2 | `docker`, `janus` - tests only, see below | 8 cores, 12 GB, 64 GB disk |
| `janus-runner03` | 3 | `docker`, `janus` - tests only, sometimes offline | 8 cores / 16 threads, 32 GB, 3.6 TB disk |

**Instances on a machine never share a port.** Each instance's `.env`
(next to its `config.sh`) sets `JANUS_TEST_PORT_OFFSET` - 0, 2000, 4000,
6000, 8000 - and every test adds it to its default ports. Tests use
18080-19530 with no offset, so instances must stay 2000 apart; the
highest offset must keep ports below 32768 (Linux's ephemeral range).

`janus-runner02` runs tests only: its host's memory has been flipping
bits (corrupted downloads that had passed their checksum, compilers
crashing with `fatal error: fault`, a host that ended up hanging), so
nothing it builds is ever published. Its container was rebuilt from
scratch after the host's crash. Give it `janus-publish` only once the
host's memory has passed a memory test.

### A runner that is sometimes offline

`janus-runner03` is switched off now and then. GitHub only gives jobs
to runners that are online, so the workflows need nothing for it, and
it never gets `janus-publish`: nothing that publishes waits for it.

- **Before switching it off**, stop its services once it is idle (no
  `Runner.Worker` process, or "Idle" under Settings → Runners):
  `sudo systemctl stop 'actions.runner.*'`. Stopping them during a job
  cancels that job, and a machine that just disappears fails its jobs
  with "lost communication with the server" after a few minutes - "Re-run
  failed jobs" then.
- **After a long absence**, its Docker build cache is cold: the first
  jobs it takes rebuild the kernel, AWS-LC and HAProxy, and take much
  longer than usual.
- **After 14 days offline**, GitHub removes it. Configure each instance
  again: `sudo ./svc.sh uninstall`, delete `.runner`, `.credentials` and
  `.credentials_rsaparams`, then the `config.sh` and `svc.sh` steps
  below.
- **A test that fails only there** blocks `publish` like any other, and
  a re-run may land on it again (GitHub doesn't let you pick the
  runner): stop its services, re-run, then look into it.

## Setting up a runner machine

On the Proxmox host - an unprivileged Debian 13 container, Docker able
to run inside (`nesting`, `keyctl`), and the host's `/dev/kvm`:

```sh
pct create <id> local:vztmpl/debian-13-standard_<version>_amd64.tar.zst \
  --hostname janus-runnerNN --cores 8 --memory 12288 --swap 2048 \
  --rootfs local-lvm:64 --net0 name=eth0,bridge=vmbr0,ip=dhcp \
  --unprivileged 1 --features nesting=1,keyctl=1 --onboot 1
```

`/dev/kvm` belongs to the host's `kvm` group (gid 103 here), mode 0660.
Rather than opening it to everyone on the host, map that one group into
the container, onto the container's own `kvm` group (gid 993 on Debian
13), and let root map it (`/etc/subgid`):

```sh
echo "root:103:1" >> /etc/subgid
cat >> /etc/pve/lxc/<id>.conf <<'EOF'
lxc.idmap: u 0 100000 65536
lxc.idmap: g 0 100000 993
lxc.idmap: g 993 103 1
lxc.idmap: g 994 100994 64542
lxc.cgroup2.devices.allow: c 10:232 rwm
lxc.mount.entry: /dev/kvm dev/kvm none bind,optional,create=file
lxc.cgroup2.devices.allow: c 10:200 rwm
lxc.mount.entry: /dev/net/tun dev/net/tun none bind,optional,create=file
EOF
```

`/dev/net/tun` is for `make controller-libvirt-test`: the virtual
machines its libvirt host creates are plugged into a network through
it. Without it the test still runs, without booting a machine, and says
so in a CI warning.

Check both gids first (`getent group kvm` on the host and in the
container) and adjust the four `idmap` lines to match.

Inside the container:

- Docker CE from Docker's Debian repository;
- a user `actions-runner` with password-less sudo (the workflows install
  packages and run some tests as root), in the `docker` and `kvm` groups
  (`root` in `kvm` too);
- check: `su - actions-runner -c 'python3 -c "import os; os.open(\"/dev/kvm\", os.O_RDWR)"'`.

A physical machine (`janus-runner03`, a laptop) needs no container:
Debian 13 installed on it - a desktop doesn't hurt - with the CPU's
virtualization (SVM / VT-x) on in the firmware, so `/dev/kvm` and
`/dev/net/tun` are simply there. Then:

- apt from `apt.sw-servers.net` (its `debian` mirror for `trixie`,
  `trixie-updates` and `trixie-security`, its `docker-trixie` mirror for
  Docker CE), the same user and groups as above;
- never asleep: the sleep targets masked (`systemctl mask sleep.target
  suspend.target hibernate.target hybrid-sleep.target
  suspend-then-hibernate.target` - a desktop's login screen suspends an
  idle machine otherwise) and a `logind.conf.d` drop-in setting
  `HandleLidSwitch`, `HandleLidSwitchExternalPower` and
  `HandleLidSwitchDocked` to `ignore`;
- its battery always on mains, so charging stops at 60 %
  (`battery-charge-limit.service` writes
  `/sys/class/power_supply/BAT0/charge_control_end_threshold` at boot);
- its NVIDIA GPU unused: no test or build uses a GPU, and the docs'
  screenshots must render in software to stay identical between
  runners. With the default `nouveau` driver it stays powered off
  (`runtime_status` `suspended`).

Then each instance, from the official runner tarball (check its sha256
against the release) into `/opt/actions-runner`, `/opt/actions-runner-2`...:

```sh
sudo -u actions-runner ./config.sh --unattended --url https://github.com/swenske/Janus \
  --token <registration token> --name janus-runnerNN[-i] --labels docker,janus --work _work
printf 'LANG=C\nJANUS_TEST_PORT_OFFSET=%d\n' <offset> > .env
sudo ./svc.sh install actions-runner && sudo ./svc.sh start
```

A registration token comes from `gh api -X POST
repos/swenske/Janus/actions/runners/registration-token -q .token`.

## Publishing janusctl on apt.sw-servers.net

A release run ends by publishing janusctl's Debian packages on
`https://apt.sw-servers.net/janus` (README.md). The `janus-publish`
machine holds the only key for it, as the `actions-runner` user - never a
GitHub secret:

```sh
su - actions-runner -c 'ssh-keygen -t ed25519 -N "" \
  -C "janus-runnerNN janus-publish@apt.int.sw-servers.net" -f ~/.ssh/id_janus_aptly'
```

On the aptly server (`apt.int.sw-servers.net`), once, with that public
key - it creates the `janus` aptly repo and a `janus-publish` account
whose key can only upload a `janusctl_<version>_<arch>.deb` and publish
the repo:

```sh
scp -r packaging/apt apt.int.sw-servers.net:
ssh -t apt.int.sw-servers.net sudo ./apt/setup-server.sh "$(cat id_janus_aptly.pub)"
```

A new `janus-publish` machine (or a new key): `setup-server.sh` again
with its public key - it replaces the previous one.
