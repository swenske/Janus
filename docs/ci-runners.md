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
  disk), the QEMU and image tools, and Node.js. Tools are installed only
  when missing, under a lock, since several jobs share a machine.
- **KVM**: every x86 test asks for it and falls back to emulation
  (`-accel kvm -accel tcg`). A UEFI boot to HAProxy answering takes about
  4 s with KVM against 13 s emulated. arm64 guests are always emulated.
- **Tests that share host paths** (the native janusd and HAProxy ones)
  are all in `test-api`, so two of them never run at once.
- **What gets published is built only on trusted runners**: `publish`,
  `schematic-build.yml` (images users download, signed) and
  `site-deploy.yml` also need the label `janus-publish`. A runner without
  it only ever runs test jobs: at worst a problem on it fails a test.

A full run takes about 6 minutes, against 25 when it was one job,
emulated.

## Runner machines

Each machine is an unprivileged LXC container on a Proxmox host and runs
several runner instances - one job each - named `<machine>`,
`<machine>-2`, `<machine>-3`...

| Machine | Instances | Labels | Size |
|---|---|---|---|
| `janus-runner01` | 5 | `docker`, `janus`, `janus-publish` | 12 cores, 24 GB, 64 GB disk |
| `janus-runner02` | 2 | `docker`, `janus` - tests only, see below | 8 cores, 12 GB, 64 GB disk |

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
EOF
```

Check both gids first (`getent group kvm` on the host and in the
container) and adjust the four `idmap` lines to match.

Inside the container:

- Docker CE from Docker's Debian repository;
- a user `actions-runner` with password-less sudo (the workflows install
  packages and run some tests as root), in the `docker` and `kvm` groups
  (`root` in `kvm` too);
- check: `su - actions-runner -c 'python3 -c "import os; os.open(\"/dev/kvm\", os.O_RDWR)"'`.

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
