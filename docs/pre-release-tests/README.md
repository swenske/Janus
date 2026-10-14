# Pre-release test reports

What each pre-release went through before it was promoted - or not: one
report per pre-release, `<version>.md`.

Each report has two parts:

- **CI**: the `image-build.yml` run that cut the pre-release - every
  QEMU test, with its result and duration, and the figures it measures
  (kexec against firmware reboots, the images' sizes).
- **The lab**: the pre-release on a real KVM host - two nodes with VRRP
  and BGP anycast behind a router, an auto-scaled node, nginx backends,
  a Controller, and clients outside the lab's host (the CI runners and
  a fourth machine). The newest release is measured the same way first, so each
  figure has something to compare with: the Controller's update, the
  image factory's build, a rolling update under load, failovers,
  firmware and kexec reboots, rollbacks, HAProxy's throughput and
  latency, and the nodes' health (SELinux, the CIS benchmark).

Every scenario, how it is run and what it measures:
[Lab test scenarios](lab-scenarios.md).

The lab's raw logs stay out of this repository: they carry its
addressing. The reports name what was measured by its role (the VIP,
the anycast route, node 01 and node 02) instead.

How a pre-release is cut and promoted: [Releasing](../contributing/releasing.md#pre-releases).
