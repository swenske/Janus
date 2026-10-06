# Examples

The files the docs show, kept here so tests can run or check them: a
page shows each one verbatim, in a code block titled with its path -
the docs build fails when a block and its file differ (`make
docs-examples` copies them in again), or when an example is shown
nowhere.

| Example | Shown in | Checked by |
|---|---|---|
| [`terraform/libvirt/`](terraform/libvirt/) | [libvirt/KVM, end to end](../docs/private-cloud/platforms/kvm-libvirt.md) | `make examples-check` (OpenTofu validates it against the provider built from this tree); `make terraform-provider-test` applies it on a real Controller and libvirt host |
| [`terraform/proxmox/`](terraform/proxmox/) | [Proxmox VE, end to end](../docs/private-cloud/platforms/proxmox.md) | `make examples-check` |
| [`haproxy/web.cfg`](haproxy/web.cfg) | The Terraform examples | `make examples-test` (the node's own HAProxy, `haproxy -c`); applied to real nodes by `make terraform-provider-test` |
| [`network/`](network/) | [Networking](../docs/private-cloud/networking.md), [bare metal](../docs/private-cloud/platforms/bare-metal.md) | `go test ./internal/netconfig` - the node's own parser |
| [`nocloud/user-data.json`](nocloud/user-data.json) | [First boot](../docs/private-cloud/first-boot.md) | `go test ./internal/nocloud` - the node's own parser |
| [`orchestrator/`](orchestrator/) | [Your own orchestrator](../docs/private-cloud/own-orchestrator.md) and its pages | `make qemu-orchestrator-test` drives three real nodes with them; `go test ./internal/pki` checks the certificates they make |
| [`prometheus/`](prometheus/) | [Observability](../docs/private-cloud/observability.md), [metrics](../docs/metrics.md) | `make examples-check` (promtool) |
| [`compose/compose.yaml`](compose/compose.yaml) | [The Controller](../dashboard/README.md#the-compose-setup) | `make examples-check` (`docker compose config`) |
