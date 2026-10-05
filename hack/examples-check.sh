#!/usr/bin/env bash
# The examples the docs show (examples/), checked by the tools that read
# them, with no node or hypervisor (make examples-check, ci.yml):
# - the Terraform configurations: OpenTofu formats and validates them
#   against the provider built from this tree;
# - the Prometheus configuration and its alert rules: promtool;
# - the Controller's compose.yaml: docker compose.
# The NoCloud and network JSON are go test's (internal/nocloud,
# internal/netconfig); haproxy.cfg the node's own HAProxy's (make
# examples-test).
#   hack/examples-check.sh <tofu> <terraform-provider-janus binary>
set -euo pipefail
cd "$(dirname "$0")/.."
tofu=$(realpath "$1")
provider_dir=$(dirname "$(realpath "$2")")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cat >"$work/tofurc" <<TOFURC
provider_installation {
  dev_overrides {
    "swenske/janus" = "$provider_dir"
  }
  direct {}
}
TOFURC
"$tofu" fmt -check -recursive examples/terraform
for dir in examples/terraform/*/; do
  echo "examples-check: $dir"
  (cd "$dir" && TF_CLI_CONFIG_FILE="$work/tofurc" TF_IN_AUTOMATION=1 "$tofu" validate -no-color)
done

docker build -q -t janus-promtool hack/promtool >/dev/null
docker run --rm -v "$PWD/examples/prometheus:/prometheus:ro" janus-promtool check config /prometheus/prometheus.yml

docker compose -f examples/compose/compose.yaml config -q
echo "examples-check: the Terraform, Prometheus and Compose examples are valid"
