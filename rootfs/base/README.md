# rootfs/base

Files copied as-is into the immutable rootfs by `../assemble.sh`: today
only the bootstrap `etc/haproxy/haproxy.cfg` (see the rootfs section of
`../../docs/architecture.md`). There is no BusyBox and no shell - every
executable on a node is built elsewhere (`pkgs/`, `extensions/`, the Go
binaries) and placed by `../assemble.sh`.
